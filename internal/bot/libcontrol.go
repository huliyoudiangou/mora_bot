package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"mora_bot/internal/db"
	"mora_bot/internal/jellyfin"
)

// ---------------------------------------------------------------------------
// 媒体库访问控制（三层模型）
//
//   第 1 层 · 模板基线：Jellyfin 模板用户的 Policy 即全体普通用户的默认值，
//             管理面板直接读写（库访问 EnableAllFolders/EnabledFolders + 并发上限）。
//   第 2 层 · 白名单覆盖：白名单用户可在 bot 本地单独设置库访问/并发上限
//             （users.jelly_lib_override，JSON），设置后覆盖优先、立即单独应用；
//             未设置覆盖的白名单用户跟随模板（白名单不豁免库控制）。
//   第 3 层 · 用户自选隐藏：用户只能在"已被允许"的库范围内选择从客户端首页
//             隐藏/显示（Jellyfin 该用户自己的 Configuration.MyMediaExcludes），
//             纯显示层，永远无法给自己加权限。
// ---------------------------------------------------------------------------

// libOverride 白名单覆盖的本地存储结构（users.jelly_lib_override JSON）。
type libOverride struct {
	EnableAllFolders  bool     `json:"enable_all"`
	EnabledFolders    []string `json:"folders"`
	MaxActiveSessions int      `json:"max_sessions"`
}

// parseLibOverride 解析覆盖 JSON；空/非法返回 nil（=跟随模板）。
func parseLibOverride(raw string) *libOverride {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var lo libOverride
	if err := json.Unmarshal([]byte(raw), &lo); err != nil {
		return nil
	}
	return &lo
}

// encodeLibOverride 序列化覆盖；folders 键始终输出（无 omitempty），
// 全库模式下存空数组与"指定 0 个库"在语义上均不出现（保存侧已守卫至少 1 个可见库）。
func encodeLibOverride(lo libOverride) string {
	if lo.EnabledFolders == nil {
		lo.EnabledFolders = []string{}
	}
	b, err := json.Marshal(lo)
	if err != nil {
		return ""
	}
	return string(b)
}

// effectiveLibAccess 计算用户生效的库访问：白名单覆盖优先，否则模板基线。
// template 传 nil 表示模板读取失败/未配置——白名单无覆盖时返回 nil（调用方跳过应用）。
func effectiveLibAccess(lo *libOverride, template *jellyfin.LibAccess) *jellyfin.LibAccess {
	if lo != nil {
		folders := lo.EnabledFolders
		if folders == nil {
			folders = []string{}
		}
		return &jellyfin.LibAccess{
			EnableAllFolders:  lo.EnableAllFolders,
			EnabledFolders:    folders,
			MaxActiveSessions: lo.MaxActiveSessions,
		}
	}
	return template
}

// templateLibAccess 从模板用户当前策略读基线（库访问 + 并发上限）。
// 模板未配置或读取失败返回 nil。
// 读路径带 60 秒 TTL 缓存：用户侧面板/切换每次要拉模板策略 + 库列表，
// 每次都打 Jellyfin 会让回调响应明显变慢；写路径（模板保存/批量应用等）
// 调 invalidateLibAccessCache 立即失效，保证管理员改动即时可见。
func templateLibAccess(ctx context.Context, deps *HandlerDeps) *jellyfin.LibAccess {
	if deps == nil || deps.JF == nil || deps.JFServerBase == "" {
		return nil
	}
	if la, ok := loadLibAccessCache(); ok {
		return la
	}
	u, ok, err := deps.JF.GetUser(ctx, deps.JFServerBase)
	if err != nil || !ok {
		return nil
	}
	la := &jellyfin.LibAccess{
		EnableAllFolders:  u.Policy.EnableAllFolders,
		EnabledFolders:    u.Policy.EnabledFolders,
		MaxActiveSessions: u.Policy.MaxActiveSessions,
	}
	storeLibAccessCache(la)
	return la
}

// invalidateLibAccessCache 模板基线缓存失效（模板 Policy 任何写路径之后调用）。
func invalidateLibAccessCache() {
	libAccessCacheMu.Lock()
	libAccessCache = nil
	libAccessCacheMu.Unlock()
}

// libAccessCache 模板基线短 TTL 缓存（内存态，60 秒过期，写路径立即失效）。
var (
	libAccessCache   *jellyfin.LibAccess
	libAccessCacheAt time.Time
	libAccessCacheMu sync.Mutex
)

// libAccessCacheTTL 模板基线缓存存活时长。
const libAccessCacheTTL = 60 * time.Second

// loadLibAccessCache 读缓存（未过期返回 true）。
func loadLibAccessCache() (*jellyfin.LibAccess, bool) {
	libAccessCacheMu.Lock()
	defer libAccessCacheMu.Unlock()
	if libAccessCache == nil || time.Since(libAccessCacheAt) > libAccessCacheTTL {
		return nil, false
	}
	la := *libAccessCache
	return &la, true
}

// storeLibAccessCache 写缓存（拷贝存，防外部修改串味）。
func storeLibAccessCache(la *jellyfin.LibAccess) {
	libAccessCacheMu.Lock()
	defer libAccessCacheMu.Unlock()
	c := *la
	libAccessCache = &c
	libAccessCacheAt = time.Now()
}

// libFoldersCache 媒体库列表短 TTL 缓存（同模板基线，用户侧高频读）。
var (
	libFoldersCache   []jellyfin.VirtualFolder
	libFoldersCacheAt time.Time
	libFoldersCacheMu sync.Mutex
)

// libFoldersCacheTTL 媒体库列表缓存存活时长（列表变化低频，30 秒足够）。
const libFoldersCacheTTL = 30 * time.Second

// listVirtualFoldersCached 带缓存的媒体库列表（用户侧面板/切换高频读）。
func listVirtualFoldersCached(ctx context.Context, deps *HandlerDeps) ([]jellyfin.VirtualFolder, error) {
	libFoldersCacheMu.Lock()
	if libFoldersCache != nil && time.Since(libFoldersCacheAt) <= libFoldersCacheTTL {
		out := make([]jellyfin.VirtualFolder, len(libFoldersCache))
		copy(out, libFoldersCache)
		libFoldersCacheMu.Unlock()
		return out, nil
	}
	libFoldersCacheMu.Unlock()
	folders, err := deps.JF.ListVirtualFolders(ctx)
	if err != nil {
		return nil, err
	}
	libFoldersCacheMu.Lock()
	libFoldersCache = make([]jellyfin.VirtualFolder, len(folders))
	copy(libFoldersCache, folders)
	libFoldersCacheAt = time.Now()
	libFoldersCacheMu.Unlock()
	return folders, nil
}

// applyLibAccessToUser 对单个用户套用库访问（管理员跳过；无生效配置跳过）。
// 返回 (applied, skipped 原因, error)。
func applyLibAccessToUser(ctx context.Context, deps *HandlerDeps, u db.User, template *jellyfin.LibAccess) (bool, string, error) {
	if deps.JF == nil {
		return false, "", fmt.Errorf("Jellyfin 未配置")
	}
	if u.JellyfinUserID == "" {
		return false, "未绑定", nil
	}
	la := effectiveLibAccess(parseLibOverride(u.JellyLibOverride), template)
	if la == nil {
		return false, "无模板基线", nil
	}
	if err := deps.JF.ApplyLibAccess(ctx, u.JellyfinUserID, *la); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// applyLibAccessToAll 把库访问批量套用到全部本地绑定用户（模板基线/白名单覆盖各自生效）。
// 顺序执行（Jellyfin 侧写压力大），结果汇报给管理员；失败列表截前 failShow 个。
// 应放 goroutine 异步执行（百余用户 × 2 次调用较耗时），不阻塞消息处理。
func applyLibAccessToAll(ctx context.Context, deps *HandlerDeps, adminID int64) {
	// 独立超时上下文：批量应用不随触发消息的 handler context 取消而中断。
	runCtx, cancel := context.WithTimeout(context.Background(), applyLibTimeout)
	defer cancel()

	template := templateLibAccess(runCtx, deps)
	if template == nil {
		sendText(runCtx, deps, adminID, "❌ 批量应用取消：模板用户未配置或读取失败，无法得到基线配置。")
		return
	}
	var users []db.User
	if err := deps.DB.Where("jellyfin_user_id <> ''").Find(&users).Error; err != nil {
		sendText(runCtx, deps, adminID, "❌ 批量应用失败：查询用户列表出错。")
		return
	}
	var okN, skipN, failN int
	var failed []string
	var consecFails int
	for _, u := range users {
		// 连续失败快速中止：Jellyfin 整体不可达时逐用户重试没有意义，
		// 每次还要等 15s 客户端超时，连续 5 个失败提前结束并汇报（可稍后重试）。
		if consecFails >= 5 {
			sendText(runCtx, deps, adminID, "❌ 批量应用中止：连续多个用户处理失败，Jellyfin 服务可能不可达，请稍后重试。")
			break
		}
		// 与覆盖套用互斥：批量写 Policy 期间不允许覆盖保存/清除并发写同一用户。
		libApplyMu.Lock()
		// Jellyfin 侧管理员账户永远跳过：bot 不碰管理员的库权限。
		if ju, found, err := deps.JF.GetUser(runCtx, u.JellyfinUserID); err == nil && found && ju.Policy.IsAdministrator {
			libApplyMu.Unlock()
			skipN++
			continue
		}
		applied, _, err := applyLibAccessToUser(runCtx, deps, u, template)
		libApplyMu.Unlock()
		switch {
		case err != nil:
			failN++
			consecFails++
			if len(failed) < failListMax {
				failed = append(failed, fmt.Sprintf("tg=%d", u.TelegramID))
			}
		case applied:
			okN++
			consecFails = 0
		default:
			skipN++
			consecFails = 0
		}
	}
	if consecFails >= 5 {
		_ = db.WriteAudit(deps.DB, adminID, "admin_lib_apply_all", "jellyfin_policy", "all_users",
			fmt.Sprintf("批量应用库访问中止（连续失败）：成功 %d，跳过 %d，失败 %d", okN, skipN, failN))
		return
	}
	_ = db.WriteAudit(deps.DB, adminID, "admin_lib_apply_all", "jellyfin_policy", "all_users",
		fmt.Sprintf("批量应用库访问：成功 %d，跳过 %d（管理员/未绑定/无基线），失败 %d", okN, skipN, failN))
	var b strings.Builder
	b.WriteString("📚 批量应用完成\n\n")
	b.WriteString(fmt.Sprintf("✅ 成功：%d 人\n⏭ 跳过：%d 人（Jellyfin 管理员/未绑定/无基线）\n❌ 失败：%d 人", okN, skipN, failN))
	if len(failed) > 0 {
		b.WriteString("\n\n失败列表：" + strings.Join(failed, "、"))
		if failN > failListMax {
			b.WriteString(fmt.Sprintf(" 等共 %d 人", failN))
		}
	}
	sendText(runCtx, deps, adminID, b.String())
}

// applyLibTimeout 批量应用的执行上限（每用户约 2 次 API 调用，15s 客户端超时）。
const applyLibTimeout = 10 * time.Minute

// failListMax 批量结果中失败 tg_id 最多列出的人数。
const failListMax = 20

// ---------------------------------------------------------------------------
// 白名单覆盖的套用与清除
// ---------------------------------------------------------------------------

// libApplyMu 串行化所有写用户 Policy 的路径（覆盖套用/清除、批量应用、
// 移除白名单重套、注册基线套用），防止同一用户被并发写两个不同配置互相覆盖。
var libApplyMu sync.Mutex

// saveOverrideAndApply 保存白名单覆盖并立即单独套用到该用户。
// 覆盖设为 nil 表示清除（恢复跟随模板）。
func saveOverrideAndApply(ctx context.Context, deps *HandlerDeps, u *db.User, lo *libOverride) error {
	libApplyMu.Lock()
	defer libApplyMu.Unlock()
	val := ""
	if lo != nil {
		val = encodeLibOverride(*lo)
		if val == "" {
			return fmt.Errorf("覆盖配置序列化失败")
		}
	}
	if err := deps.DB.Model(u).Update("jelly_lib_override", val).Error; err != nil {
		return err
	}
	u.JellyLibOverride = val
	// 清除覆盖：恢复模板基线；设置覆盖：套用覆盖。
	template := templateLibAccess(ctx, deps)
	if lo == nil {
		if template == nil {
			// 无基线可恢复：仅清本地覆盖，Jellyfin 侧保持现状（管理员可手动应用）。
			return nil
		}
		la := *template
		return deps.JF.ApplyLibAccess(ctx, u.JellyfinUserID, la)
	}
	la := effectiveLibAccess(lo, template)
	if la == nil {
		return fmt.Errorf("模板用户未配置，无法套用")
	}
	return deps.JF.ApplyLibAccess(ctx, u.JellyfinUserID, *la)
}

// ---------------------------------------------------------------------------
// 用户自选隐藏（第 3 层，纯显示层）
// ---------------------------------------------------------------------------

// myLibStatus 用户视角的单个库状态。
type myLibStatus struct {
	Folder    *jellyfin.VirtualFolder
	Allowed   bool // 是否在用户生效策略的允许范围内
	Hidden    bool // 是否已被用户自己隐藏（MyMediaExcludes）
}

// computeMyLibs 计算用户视角的库列表（开放/未开放/已隐藏）。
func computeMyLibs(folders []jellyfin.VirtualFolder, la *jellyfin.LibAccess, excludes []string) []myLibStatus {
	exSet := make(map[string]bool, len(excludes))
	for _, id := range excludes {
		exSet[strings.ToLower(id)] = true
	}
	out := make([]myLibStatus, 0, len(folders))
	for i := range folders {
		f := folders[i]
		allowed := la != nil && (la.EnableAllFolders || containsFold(la.EnabledFolders, f.ID))
		out = append(out, myLibStatus{
			Folder:  &folders[i],
			Allowed: allowed,
			Hidden:  exSet[strings.ToLower(f.ID)],
		})
	}
	return out
}

// containsFold 列表中是否包含目标 ID（大小写不敏感）。
func containsFold(list []string, id string) bool {
	for _, v := range list {
		if strings.EqualFold(v, id) {
			return true
		}
	}
	return false
}

// currentExcludes 从用户 Configuration 中取出 MyMediaExcludes（缺失/类型异常返回空）。
func currentExcludes(cfg map[string]any) []string {
	raw, ok := cfg["MyMediaExcludes"]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, it := range v {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// 用户面板「我的媒体库」：库列表 + 自选显示/隐藏（纯显示层）
// ---------------------------------------------------------------------------

// sendMyLibsPanel 发送用户自己的媒体库面板：实时取库列表，
// 比对生效策略给出 开放/未开放，已隐藏的标注并附显示按钮。
func sendMyLibsPanel(ctx context.Context, deps *HandlerDeps, userID int64, chatID int64, msgID int, u *db.User) {
	if deps.JF == nil {
		sendText(ctx, deps, chatID, "Jellyfin 未配置。")
		return
	}
	if u.JellyfinUserID == "" {
		sendText(ctx, deps, chatID, "你还没有绑定 Jellyfin 账号，请先「🔗 绑定已有账号」或「📝 注册新账号」。")
		return
	}
	folders, err := listVirtualFoldersCached(ctx, deps)
	if err != nil {
		sendText(ctx, deps, chatID, "获取媒体库列表失败，请稍后再试。")
		return
	}
	la := effectiveLibAccess(parseLibOverride(u.JellyLibOverride), templateLibAccess(ctx, deps))
	cfg, err := deps.JF.GetUserConfiguration(ctx, u.JellyfinUserID)
	if err != nil {
		sendText(ctx, deps, chatID, "获取你的媒体库设置失败，请稍后再试。")
		return
	}
	excludes := currentExcludes(cfg)
	statuses := computeMyLibs(folders, la, excludes)
	var b strings.Builder
	b.WriteString("📚 <b>我的媒体库</b>\n\n")
	if la != nil {
		b.WriteString("⏱ 同时在线上限：" + sessionsLimitText(la.MaxActiveSessions) + "\n\n")
	}
	if len(statuses) == 0 {
		b.WriteString("服务器上暂无媒体库。")
	}
	// 隐藏超过 20 个库时放弃逐库按钮（面板会超 Telegram 64 键盘上限），
	// 超出的库只做状态展示。
	const maxButtons = 20
	rows := make([][]KeyboardButton, 0, len(statuses)+1)
	shownButtons := 0
	for _, st := range statuses {
		label := st.Folder.Name
		if strings.TrimSpace(label) == "" {
			label = st.Folder.ID
		}
		switch {
		case !st.Allowed:
			b.WriteString("🔒 " + escapeHTML(label) + "（未开放）\n")
		case st.Hidden:
			b.WriteString("⚪ " + escapeHTML(label) + "（已隐藏）\n")
			if shownButtons < maxButtons {
				rows = append(rows, []KeyboardButton{{
					Text: "👁 显示 " + label,
					Data: BuildCallbackData(DKMenu, "libshow", st.Folder.ID),
				}})
				shownButtons++
			}
		default:
			b.WriteString("🟢 " + escapeHTML(label) + "\n")
			if shownButtons < maxButtons {
				rows = append(rows, []KeyboardButton{{
					Text: "🙈 隐藏 " + label,
					Data: BuildCallbackData(DKMenu, "libhide", st.Folder.ID),
				}})
				shownButtons++
			}
		}
	}
	b.WriteString("\n隐藏/显示只影响客户端「我的媒体」首页展示，不影响搜索观看；🔒 未开放的库由管理员控制。")
	rows = append(rows, []KeyboardButton{
		{Text: "🔄 刷新", Data: BuildCallbackData(DKMenu, "mylibs")},
		{Text: "↩️ 返回主菜单", Data: BuildCallbackData(DKMenu, "home")},
	})
	sendPanel(ctx, deps, chatID, msgID, b.String(), rows)
}

// handleMyLibToggle 用户切换某个库的首页隐藏状态（libhide=隐藏 / libshow=显示）。
// 先立即 ACK（清转圈），再执行 Jellyfin 读写链；结果文案由刷新的面板呈现，
// 失败时用 alert 提示。
func handleMyLibToggle(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, hide bool, folderID string) {
	ack := func(text string, alert bool) {
		_ = deps.Snd.AnswerCallback(ctx, cq.ID, text, alert)
	}
	// 立即 ACK 清转圈：后面的 Jellyfin 链（单用户配置读 + 隐藏写 + 面板刷新）
	// 可能要几秒，不先 ACK 用户侧会一直转圈。
	ack("", false)
	u, err := ensureUser(ctx, deps, cq.From)
	if err != nil {
		ack("查询失败，请稍后再试", true)
		return
	}
	if u.JellyfinUserID == "" {
		ack("请先绑定 Jellyfin 账号", true)
		return
	}
	if hide {
		_, err = toggleMyLibHideTo(ctx, deps, u, folderID, true)
	} else {
		_, err = toggleMyLibHideTo(ctx, deps, u, folderID, false)
	}
	if err != nil {
		ack(err.Error(), true)
		return
	}
	// 原地刷新面板（结果以面板三态呈现）
	sendMyLibsPanel(ctx, deps, cq.From.ID, cq.ChatID, messageIDOf(cq), u)
}

// toggleMyLibHideTo 按目标状态（而非翻转）设置隐藏，幂等：目标状态已满足时直接成功。
func toggleMyLibHideTo(ctx context.Context, deps *HandlerDeps, u *db.User, folderID string, hide bool) (bool, error) {
	if deps == nil || deps.JF == nil || u == nil || u.JellyfinUserID == "" {
		return false, fmt.Errorf("Jellyfin 未配置或账号未绑定")
	}
	la := effectiveLibAccess(parseLibOverride(u.JellyLibOverride), templateLibAccess(ctx, deps))
	folders, err := listVirtualFoldersCached(ctx, deps)
	if err != nil {
		return false, fmt.Errorf("获取媒体库失败，请稍后再试")
	}
	var target *jellyfin.VirtualFolder
	for i := range folders {
		if strings.EqualFold(folders[i].ID, folderID) {
			target = &folders[i]
			break
		}
	}
	if target == nil {
		return false, fmt.Errorf("媒体库不存在")
	}
	allowed := la != nil && (la.EnableAllFolders || containsFold(la.EnabledFolders, target.ID))
	if !allowed {
		return false, fmt.Errorf("该媒体库未对你开放")
	}
	cfg, err := deps.JF.GetUserConfiguration(ctx, u.JellyfinUserID)
	if err != nil {
		return false, fmt.Errorf("获取设置失败，请稍后再试")
	}
	cur := currentExcludes(cfg)
	already := containsFold(cur, target.ID)
	if already == hide {
		return hide, nil // 幂等：目标状态已满足
	}
	var next []string
	if hide {
		if already != hide {
			next = append(cur, target.ID)
		} else {
			next = cur
		}
	} else {
		for _, v := range cur {
			if !strings.EqualFold(v, target.ID) {
				next = append(next, v)
			}
		}
	}
	if err := deps.JF.SetMyMediaExcludes(ctx, u.JellyfinUserID, next); err != nil {
		return false, fmt.Errorf("保存失败，请稍后再试")
	}
	return hide, nil
}

// ---------------------------------------------------------------------------
// 管理面板：模板库访问编辑（逐库开关，会话内暂存，保存时写模板 Policy）
// ---------------------------------------------------------------------------

// libEditorSessions 模板库编辑器会话状态（每管理员一份）。
// 回调切换只改内存暂存，💾 保存才真正写模板 Policy。
// 带 30 分钟 TTL：过期自动丢弃（编辑器/确认状态均为内存态，/cancel 之外
// 的唯一清理手段，防止管理员放弃编辑后暂存永久滞留）。
type libEditorSessions struct {
	mu     sync.Mutex
	owners map[int64]*libEditorEntry
}

// libEditorEntry 编辑器 + 最后活跃时间。
type libEditorEntry struct {
	Ed       *libEditor
	LastUsed time.Time
}

// libEditorTTL 编辑器/确认状态的存活上限（与会话 TTL 一致）。
const libEditorTTL = 30 * time.Minute

// touch 刷新最后活跃时间（调用方已持有锁时直接改字段）。
func (s *libEditorSessions) touchLocked(adminID int64) {
	if e, ok := s.owners[adminID]; ok {
		e.LastUsed = time.Now()
	}
}

// gcLocked 丢弃超时未活跃的编辑器（调用方已持有锁）。
func (s *libEditorSessions) gcLocked(now time.Time) {
	for id, e := range s.owners {
		if now.Sub(e.LastUsed) > libEditorTTL {
			delete(s.owners, id)
		}
	}
}

// libEditor 单个管理员的编辑器状态（模板编辑与白名单覆盖编辑共用）。
type libEditor struct {
	Folders     []jellyfin.VirtualFolder // 全部库（含顺序）
	Allowed     map[string]bool          // folderID → 是否允许（键统一小写，大小写不敏感）
	MaxSessions int                      // 并发上限暂存（模板编辑沿用模板现值；白名单覆盖编辑可改）
	TargetTG    int64                    // 白名单覆盖编辑的目标 tg_id（模板编辑为 0）
}

// libEditors 模板库编辑器全局状态（内存态，30 分钟 TTL 过期自动丢弃）。
var libEditors = &libEditorSessions{owners: map[int64]*libEditorEntry{}}

// GCLibEditors 周期清理超时编辑器（供 main 的分钟级 GC 调用）。
func GCLibEditors(now time.Time) {
	libEditors.mu.Lock()
	defer libEditors.mu.Unlock()
	libEditors.gcLocked(now)
}

// beginLibEditor 开始编辑：以模板当前策略为初值。
func (s *libEditorSessions) beginLibEditor(adminID int64, folders []jellyfin.VirtualFolder, la *jellyfin.LibAccess) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &libEditor{
		Folders: folders,
		Allowed: make(map[string]bool, len(folders)),
	}
	for _, f := range folders {
		allowed := la.EnableAllFolders || containsFold(la.EnabledFolders, f.ID)
		e.Allowed[strings.ToLower(f.ID)] = allowed
	}
	// 全库模式下进入编辑器仍是逐库开关展示（全部为 true），保存时若全为 true 按 All=true 落库
	s.owners[adminID] = &libEditorEntry{Ed: e, LastUsed: time.Now()}
}

// editorOf 取编辑器状态；无或已过期返回 nil。
func (s *libEditorSessions) editorOf(adminID int64) *libEditor {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.owners[adminID]
	if !ok {
		return nil
	}
	// 惰性过期：读取时超时即丢弃
	if time.Since(entry.LastUsed) > libEditorTTL {
		delete(s.owners, adminID)
		return nil
	}
	entry.LastUsed = time.Now()
	return entry.Ed
}

// drop 丢弃编辑器状态。
func (s *libEditorSessions) drop(adminID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.owners, adminID)
}

// toggle 切换某个库的暂存状态，返回切换后的值。
func (e *libEditor) toggle(folderID string) bool {
	key := strings.ToLower(folderID)
	e.Allowed[key] = !e.Allowed[key]
	return e.Allowed[key]
}

// isAll 当前暂存是否全部库都允许。
// 以 Folders 列表为准遍历（Allowed 缺键按 false），不依赖"全部库都已写入键"的隐含约定。
func (e *libEditor) isAll() bool {
	if len(e.Folders) == 0 {
		return false
	}
	for _, f := range e.Folders {
		if !e.Allowed[strings.ToLower(f.ID)] {
			return false
		}
	}
	return true
}

// toLibAccess 把暂存转成 LibAccess。
func (e *libEditor) toLibAccess(maxSessions int) jellyfin.LibAccess {
	if e.isAll() {
		return jellyfin.LibAccess{EnableAllFolders: true, EnabledFolders: nil, MaxActiveSessions: maxSessions}
	}
	folders := make([]string, 0, len(e.Allowed))
	for _, f := range e.Folders {
		if e.Allowed[strings.ToLower(f.ID)] {
			folders = append(folders, f.ID)
		}
	}
	return jellyfin.LibAccess{EnableAllFolders: false, EnabledFolders: folders, MaxActiveSessions: maxSessions}
}

// sendLibEditorPanel 发送模板库编辑面板（逐库 🟢/⚪ 开关 + 保存/取消）。
func sendLibEditorPanel(ctx context.Context, deps *HandlerDeps, adminID int64, chatID int64, msgID int, e *libEditor) {
	var b strings.Builder
	b.WriteString("🎬 <b>编辑模板库访问</b>\n\n")
	b.WriteString("点库切换对全体普通用户的可见性（🟢 可见 / ⚪ 隐藏），💾 保存后写入模板基线。\n")
	b.WriteString("⚠️ 保存后需点「🚀 应用到全体用户」才会同步到存量用户。\n\n")
	rows := make([][]KeyboardButton, 0, len(e.Folders)+2)
	if len(e.Folders) == 0 {
		b.WriteString("服务器上暂无媒体库。")
	}
	for _, f := range e.Folders {
		mark := "⚪"
		if e.Allowed[strings.ToLower(f.ID)] {
			mark = "🟢"
		}
		label := f.Name
		if strings.TrimSpace(label) == "" {
			label = f.ID
		}
		// 正文不再重复状态行（按钮文字已带 🟢/⚪），只列库名，控制消息长度
		b.WriteString(fmt.Sprintf("· %s\n", escapeHTML(label)))
		rows = append(rows, []KeyboardButton{{
			Text: fmt.Sprintf("%s %s", mark, label),
			Data: BuildCallbackData(DKAdmin, "libt", f.ID),
		}})
	}
	b.WriteString("\n⏱ 并发上限不在此处修改（返回后用「⏱ 设置并发上限」）。")
	rows = append(rows,
		[]KeyboardButton{
			{Text: "💾 保存", Data: BuildCallbackData(DKAdmin, "lib:save")},
			{Text: "✖️ 取消", Data: BuildCallbackData(DKAdmin, "lib:cancel")},
		},
		[]KeyboardButton{
			{Text: "↩️ 返回媒体库面板", Data: BuildCallbackData(DKAdmin, "lib")},
		},
	)
	sendPanel(ctx, deps, chatID, msgID, b.String(), rows)
}

// libConfirmSessions 批量应用确认会话（确认词模式，复用设备清理 confirm 思路）。
// 内存态：adminID → 请求时间；30 分钟 TTL 过期自动失效（与编辑器一致），
// 防止管理员点了「🚀」后放弃、待确认状态永久滞留（之后任何文本都被当确认词消费）。
var libConfirmPending = map[int64]time.Time{}
var libConfirmMu sync.Mutex

// setLibConfirmPending 标记/清除批量应用待确认状态。
func setLibConfirmPending(adminID int64, pending bool) {
	libConfirmMu.Lock()
	defer libConfirmMu.Unlock()
	if pending {
		libConfirmPending[adminID] = time.Now()
	} else {
		delete(libConfirmPending, adminID)
	}
}

// isLibConfirmPending 是否处于待确认状态（超时自动失效并清除）。
func isLibConfirmPending(adminID int64) bool {
	libConfirmMu.Lock()
	defer libConfirmMu.Unlock()
	ts, ok := libConfirmPending[adminID]
	if !ok {
		return false
	}
	if time.Since(ts) > libEditorTTL {
		delete(libConfirmPending, adminID)
		return false
	}
	return true
}

// handleAdminLibCallback 媒体库访问控制子面板的全部回调（action=lib/libt）。
// 调用方（handleAdminCallback）已校验管理员身份并完成全局 ACK，此处不再应答。
func (r *Router) handleAdminLibCallback(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, action string, args []string) {
	switch action {
	case "lib":
		if len(args) == 0 {
			u, err := ensureUser(ctx, deps, cq.From)
			if err != nil {
				sendText(ctx, deps, cq.ChatID, "查询失败，请稍后再试。")
				return
			}
			text, rows := libPanel(ctx, deps, u)
			sendPanel(ctx, deps, cq.ChatID, messageIDOf(cq), text, rows)
			return
		}
		switch args[0] {
		case "edit":
			if deps.JF == nil {
				sendText(ctx, deps, cq.ChatID, "Jellyfin 未配置。")
				return
			}
			folders, err := listVirtualFoldersCached(ctx, deps)
			if err != nil {
				sendText(ctx, deps, cq.ChatID, "获取媒体库列表失败："+jellyfinErrText(err))
				return
			}
			la := templateLibAccess(ctx, deps)
			if la == nil {
				sendText(ctx, deps, cq.ChatID, "模板用户未配置或读取失败，无法编辑基线。")
				return
			}
			libEditors.beginLibEditor(cq.From.ID, folders, la)
			e := libEditors.editorOf(cq.From.ID)
			sendLibEditorPanel(ctx, deps, cq.From.ID, cq.ChatID, messageIDOf(cq), e)
		case "save":
			e := libEditors.editorOf(cq.From.ID)
			if e == nil {
				sendText(ctx, deps, cq.ChatID, "没有进行中的编辑，请先点「🎬 编辑模板库访问」。")
				return
			}
			if deps.JF == nil || deps.JFServerBase == "" {
				sendText(ctx, deps, cq.ChatID, "模板用户未配置，无法保存。")
				return
			}
			la := e.toLibAccess(templateLibAccess(ctx, deps).MaxActiveSessions)
			// 直接写模板用户 Policy：模板即基线存储。
			if err := deps.JF.UpdateUserPolicy(ctx, deps.JFServerBase, buildTemplatePolicy(ctx, deps, la)); err != nil {
				sendText(ctx, deps, cq.ChatID, "保存失败："+jellyfinErrText(err))
				return
			}
			// 模板 Policy 已变：立即失效基线缓存，管理员/用户侧改动即时可见。
			invalidateLibAccessCache()
			libEditors.drop(cq.From.ID)
			_ = db.WriteAudit(deps.DB, cq.From.ID, "admin_lib_edit_template", "jellyfin_policy",
				"template:"+deps.JFServerBase,
				fmt.Sprintf("模板库访问已更新：%s，并发上限 %d", libDescText(la.EnableAllFolders, len(la.EnabledFolders)), la.MaxActiveSessions))
			sendText(ctx, deps, cq.ChatID,
				"✅ 模板基线已保存："+libDescText(la.EnableAllFolders, len(la.EnabledFolders))+
					"，并发上限 "+sessionsLimitText(la.MaxActiveSessions)+"。\n"+
					"存量用户不会自动变化，需点「🚀 应用到全体用户」同步。")
		case "cancel":
			libEditors.drop(cq.From.ID)
			sendText(ctx, deps, cq.ChatID, "已取消编辑，模板基线未改动。")
		case "sessions":
			deps.Sessions.Begin(cq.From.ID, sessAdminLibSessions)
			cur := "不限"
			if la := templateLibAccess(ctx, deps); la != nil {
				cur = sessionsLimitText(la.MaxActiveSessions)
			}
			sendText(ctx, deps, cq.ChatID,
				"⏱ 设置并发会话上限（模板基线）\n"+
					"当前："+cur+"\n\n"+
					"请输入上限（非负整数）：\n"+
					"<code>2</code> = 最多 2 台设备同时在线（约等于 2 路同时播放）\n"+
					"<code>0</code> = 不限\n\n"+
					"保存后需点「🚀 应用到全体用户」同步存量用户。回复 /cancel 可取消。")
		case "apply":
			if deps.JF == nil {
				sendText(ctx, deps, cq.ChatID, "Jellyfin 未配置。")
				return
			}
			if isLibConfirmPending(cq.From.ID) {
				sendText(ctx, deps, cq.ChatID, "已有一个待确认的批量应用，请回复「确认」或「取消」。")
				return
			}
			var boundCount int64
			deps.DB.Model(&db.User{}).Where("jellyfin_user_id <> ''").Count(&boundCount)
			setLibConfirmPending(cq.From.ID, true)
			sendHTML(ctx, deps, cq.ChatID, fmt.Sprintf(
				"⚠️ <b>批量应用库访问</b>\n\n将按当前模板基线覆盖 <b>%d</b> 个本地绑定用户的库访问与并发上限（白名单单独覆盖不受影响，Jellyfin 管理员账户自动跳过）。\n\n"+
					"注意：会覆盖用户在 Jellyfin 后台被手动修改的库权限。\n\n"+
					"回复「<b>确认</b>」开始执行，回复「<b>取消</b>」放弃。", boundCount))
		}
	case "libt":
		// 编辑器内切换某个库
		if len(args) == 0 {
			return
		}
		e := libEditors.editorOf(cq.From.ID)
		if e == nil {
			sendText(ctx, deps, cq.ChatID, "没有进行中的编辑，请先点「🎬 编辑模板库访问」。")
			return
		}
		e.toggle(args[0])
		sendLibEditorPanel(ctx, deps, cq.From.ID, cq.ChatID, messageIDOf(cq), e)
	}
}

// buildTemplatePolicy 构造模板用户更新用 Policy：读当前策略原样保留，
// 只替换库访问三项字段（与 ApplyLibAccess 同模式，避免覆盖模板其它设置）。
func buildTemplatePolicy(ctx context.Context, deps *HandlerDeps, la jellyfin.LibAccess) jellyfin.UserPolicy {
	u, ok, err := deps.JF.GetUser(ctx, deps.JFServerBase)
	if err != nil || !ok {
		// 读不到模板时退化为最小 Policy：只带库访问三项（Jellyfin 对缺失字段按默认处理，
		// 模板读取失败本就不该走到这里——save 分支已提前校验）。
		return jellyfin.UserPolicy{
			EnableAllFolders:  la.EnableAllFolders,
			EnabledFolders:    la.EnabledFolders,
			MaxActiveSessions: la.MaxActiveSessions,
		}
	}
	p := u.Policy
	p.EnableAllFolders = la.EnableAllFolders
	p.EnabledFolders = la.EnabledFolders
	p.MaxActiveSessions = la.MaxActiveSessions
	return p
}

// handleAdminLibConfirmStep 批量应用的「确认/取消」回复处理（文本消息路径）。
// 返回是否已消费这条消息。
func (r *Router) handleAdminLibConfirmStep(ctx context.Context, msg *Message) bool {
	if !isLibConfirmPending(msg.From.ID) {
		return false
	}
	t := strings.TrimSpace(msg.Text)
	switch {
	case strings.EqualFold(t, "确认"), strings.EqualFold(t, "confirm"):
		setLibConfirmPending(msg.From.ID, false)
		sendText(ctx, r.deps, msg.ChatID, "🚀 开始批量应用库访问，完成后汇报结果（执行期间请勿重复操作）。")
		// 异步执行：百余用户 × 2 次 API 调用较耗时，不阻塞消息处理。
		deps := r.deps
		adminID := msg.From.ID
		go applyLibAccessToAll(context.Background(), deps, adminID)
	case strings.EqualFold(t, "取消"), strings.EqualFold(t, "cancel"):
		setLibConfirmPending(msg.From.ID, false)
		sendText(ctx, r.deps, msg.ChatID, "已取消批量应用。")
	default:
		sendText(ctx, r.deps, msg.ChatID, "请回复「确认」开始批量应用，或回复「取消」放弃。")
	}
	return true
}

// handleAdminLibSessionsStep 设置模板并发上限向导：收非负整数（0=不限）。
func (r *Router) handleAdminLibSessionsStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消设置并发上限。")
		return
	}
	n := parseInt64Safe(msg.Text)
	if n < 0 || n > 1000 {
		sendText(ctx, deps, msg.ChatID, "并发上限必须是非负整数（0=不限，最大 1000），请重新输入。")
		return
	}
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil || sess.Kind != sessAdminLibSessions {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	deps.Sessions.Clear(msg.From.ID)
	if deps.JF == nil || deps.JFServerBase == "" {
		sendText(ctx, deps, msg.ChatID, "模板用户未配置，无法保存。")
		return
	}
	la := templateLibAccess(ctx, deps)
	if la == nil {
		sendText(ctx, deps, msg.ChatID, "模板用户读取失败，无法保存。")
		return
	}
	la.MaxActiveSessions = int(n)
	if err := deps.JF.UpdateUserPolicy(ctx, deps.JFServerBase, buildTemplatePolicy(ctx, deps, *la)); err != nil {
		sendText(ctx, deps, msg.ChatID, "保存失败："+jellyfinErrText(err))
		return
	}
	// 模板 Policy 已变：立即失效基线缓存。
	invalidateLibAccessCache()
	_ = db.WriteAudit(deps.DB, msg.From.ID, "admin_lib_sessions", "jellyfin_policy",
		"template:"+deps.JFServerBase, fmt.Sprintf("模板并发上限=%d", n))
	sendText(ctx, deps, msg.ChatID,
		"✅ 模板并发会话上限已设为 "+sessionsLimitText(int(n))+"。\n"+
			"存量用户不会自动变化，需点「🚀 应用到全体用户」同步。")
}

// jellyfinErrText 用户侧（含管理员）Jellyfin 错误的通用提示：
// 网络层错误可能携带服务器地址/内部拓扑，不透原始错误，管理员可看日志排查。
func jellyfinErrText(err error) string {
	if err == nil {
		return "未知错误"
	}
	return "请稍后再试（详情见服务日志）"
}

// ---------------------------------------------------------------------------
// 白名单单独覆盖：库访问 + 并发上限（编辑器复用 libEditor 暂存机制）
// ---------------------------------------------------------------------------

// wlLibEditors 白名单覆盖编辑器（每管理员一份，target 存目标 tg_id）。
var wlLibEditors = &libEditorSessions{owners: map[int64]*libEditorEntry{}}

// wlLibTargetOf 取白名单覆盖编辑器的目标 tg_id（无编辑器返回 0）。
func wlLibTargetOf(adminID int64) int64 {
	e := wlLibEditors.editorOf(adminID)
	if e == nil {
		return 0
	}
	return e.TargetTG
}

// beginWLLibEditor 开始白名单覆盖编辑：以该用户当前覆盖（无则模板基线）为初值。
func (s *libEditorSessions) beginWLLibEditor(adminID, targetTG int64, folders []jellyfin.VirtualFolder, la *jellyfin.LibAccess) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &libEditor{
		Folders:     folders,
		Allowed:     make(map[string]bool, len(folders)),
		MaxSessions: la.MaxActiveSessions,
		TargetTG:    targetTG,
	}
	for _, f := range folders {
		allowed := la.EnableAllFolders || containsFold(la.EnabledFolders, f.ID)
		e.Allowed[strings.ToLower(f.ID)] = allowed
	}
	s.owners[adminID] = &libEditorEntry{Ed: e, LastUsed: time.Now()}
}

// sendWLLibEditorPanel 发送白名单覆盖编辑面板（逐库开关 + 并发上限 + 保存/清除）。
func sendWLLibEditorPanel(ctx context.Context, deps *HandlerDeps, adminID int64, chatID int64, msgID int, e *libEditor, curOverride *libOverride) {
	var b strings.Builder
	ovLine := "未设置覆盖（跟随模板基线）"
	if curOverride != nil {
		ovLine = "已设置覆盖"
	}
	b.WriteString(fmt.Sprintf("📚 <b>白名单单独库/并发设置</b>\n\n目标用户：tg=%d（%s）\n\n", e.TargetTG, ovLine))
	b.WriteString("点库切换该用户的可见性（🟢 可见 / ⚪ 隐藏），⏱ 切换并发上限模式，💾 保存后立即生效。\n\n")
	rows := make([][]KeyboardButton, 0, len(e.Folders)+4)
	if len(e.Folders) == 0 {
		b.WriteString("服务器上暂无媒体库。")
	}
	for _, f := range e.Folders {
		mark := "⚪"
		if e.Allowed[strings.ToLower(f.ID)] {
			mark = "🟢"
		}
		label := f.Name
		if strings.TrimSpace(label) == "" {
			label = f.ID
		}
		b.WriteString(fmt.Sprintf("· %s\n", escapeHTML(label)))
		rows = append(rows, []KeyboardButton{{
			Text: fmt.Sprintf("%s %s", mark, label),
			Data: BuildCallbackData(DKAdmin, "wllib:t", f.ID),
		}})
	}
	// 并发上限：点「⏱」逐级 +1，点「♾」切不限（0）。
	sessLine := fmt.Sprintf("⏱ 并发上限：%s（点「⏱」+1）", sessionsLimitText(e.MaxSessions))
	if e.MaxSessions <= 0 {
		sessLine = "⏱ 并发上限：不限（点「⏱」设为 1）"
	}
	b.WriteString("\n" + sessLine)
	rows = append(rows,
		[]KeyboardButton{
			{Text: "⏱ 并发上限 +1", Data: BuildCallbackData(DKAdmin, "wllib:sess")},
			{Text: "♾ 不限", Data: BuildCallbackData(DKAdmin, "wllib:sess0")},
		},
		[]KeyboardButton{
			{Text: "💾 保存覆盖", Data: BuildCallbackData(DKAdmin, "wllib:save")},
			{Text: "♻️ 清除覆盖", Data: BuildCallbackData(DKAdmin, "wllib:clear")},
		},
		[]KeyboardButton{
			{Text: "✖️ 取消", Data: BuildCallbackData(DKAdmin, "wllib:cancel")},
			{Text: "↩️ 返回白名单面板", Data: BuildCallbackData(DKAdmin, "whitelist")},
		},
	)
	sendPanel(ctx, deps, chatID, msgID, b.String(), rows)
}

// handleAdminWLLibStep 白名单单独库/并发设置第 1 步：收 tg_id，打开覆盖编辑器。
func (r *Router) handleAdminWLLibStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消白名单库设置。")
		return
	}
	tgID := parseInt64Safe(msg.Text)
	if tgID == 0 {
		sendText(ctx, deps, msg.ChatID, "请输入合法的用户 tg_id（数字）。")
		return
	}
	deps.Sessions.Clear(msg.From.ID)
	var u db.User
	if err := deps.DB.Where("telegram_id = ?", tgID).First(&u).Error; err != nil {
		sendText(ctx, deps, msg.ChatID, "未找到该用户（tg_id="+itoa64s(tgID)+"）。")
		return
	}
	if !u.IsPermanent {
		sendText(ctx, deps, msg.ChatID, "该用户不在白名单，单独库/并发设置仅对白名单用户开放（普通用户跟随模板基线）。")
		return
	}
	if deps.JF == nil {
		sendText(ctx, deps, msg.ChatID, "Jellyfin 未配置。")
		return
	}
	folders, err := listVirtualFoldersCached(ctx, deps)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "获取媒体库列表失败："+jellyfinErrText(err))
		return
	}
	// 初值：现有覆盖优先，无则模板基线。
	lo := parseLibOverride(u.JellyLibOverride)
	la := effectiveLibAccess(lo, templateLibAccess(ctx, deps))
	if la == nil {
		// 无覆盖也无模板：从全库默认开始
		la = &jellyfin.LibAccess{EnableAllFolders: true, MaxActiveSessions: 0}
	}
	wlLibEditors.beginWLLibEditor(msg.From.ID, tgID, folders, la)
	e := wlLibEditors.editorOf(msg.From.ID)
	sendWLLibEditorPanel(ctx, deps, msg.From.ID, msg.ChatID, 0, e, lo)
}

// handleAdminWLLibCallback 白名单覆盖编辑器的全部回调。
// 调用方（handleAdminCallback）已校验管理员身份并完成全局 ACK，此处不再应答。
func (r *Router) handleAdminWLLibCallback(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, args []string) {
	e := wlLibEditors.editorOf(cq.From.ID)
	if e == nil {
		sendText(ctx, deps, cq.ChatID, "没有进行中的编辑，请从「✅ 白名单」→「📚 单独库/并发设置」重新开始。")
		return
	}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "t":
		// 切换某个库
		if len(args) < 2 {
			return
		}
		e.toggle(args[1])
		var u db.User
		cur := (*libOverride)(nil)
		if err := deps.DB.Where("telegram_id = ?", e.TargetTG).First(&u).Error; err == nil {
			cur = parseLibOverride(u.JellyLibOverride)
		}
		sendWLLibEditorPanel(ctx, deps, cq.From.ID, cq.ChatID, messageIDOf(cq), e, cur)
	case "sess":
		// 并发上限 +1（不限(0)时先设为 1）
		if e.MaxSessions <= 0 {
			e.MaxSessions = 1
		} else if e.MaxSessions < 1000 {
			e.MaxSessions++
		}
		var u db.User
		cur := (*libOverride)(nil)
		if err := deps.DB.Where("telegram_id = ?", e.TargetTG).First(&u).Error; err == nil {
			cur = parseLibOverride(u.JellyLibOverride)
		}
		sendWLLibEditorPanel(ctx, deps, cq.From.ID, cq.ChatID, messageIDOf(cq), e, cur)
	case "sess0":
		// 并发上限切不限
		e.MaxSessions = 0
		var u db.User
		cur := (*libOverride)(nil)
		if err := deps.DB.Where("telegram_id = ?", e.TargetTG).First(&u).Error; err == nil {
			cur = parseLibOverride(u.JellyLibOverride)
		}
		sendWLLibEditorPanel(ctx, deps, cq.From.ID, cq.ChatID, messageIDOf(cq), e, cur)
	case "save":
		if deps.JF == nil {
			sendText(ctx, deps, cq.ChatID, "Jellyfin 未配置。")
			return
		}
		var u db.User
		if err := deps.DB.Where("telegram_id = ?", e.TargetTG).First(&u).Error; err != nil {
			sendText(ctx, deps, cq.ChatID, "目标用户不存在，已取消。")
			wlLibEditors.drop(cq.From.ID)
			return
		}
		if u.JellyfinUserID == "" {
			sendText(ctx, deps, cq.ChatID, "该用户未绑定 Jellyfin 账号，已取消。")
			return
		}
		lo := libOverride{
			EnableAllFolders:  e.isAll(),
			EnabledFolders:    nil,
			MaxActiveSessions: e.MaxSessions,
		}
		if !e.isAll() {
			folders := make([]string, 0, len(e.Allowed))
			for _, f := range e.Folders {
				if e.Allowed[strings.ToLower(f.ID)] {
					folders = append(folders, f.ID)
				}
			}
			lo.EnabledFolders = folders
			// 一个库都没选：不合法（用户将看不到任何库），拒绝保存
			if len(folders) == 0 {
				sendText(ctx, deps, cq.ChatID, "⚠️ 至少保留 1 个可见库（全隐藏会让用户无法使用），请调整后再保存。")
				return
			}
		}
		if err := saveOverrideAndApply(ctx, deps, &u, &lo); err != nil {
			sendText(ctx, deps, cq.ChatID, "保存失败："+jellyfinErrText(err))
			return
		}
		wlLibEditors.drop(cq.From.ID)
		_ = db.WriteAudit(deps.DB, cq.From.ID, "admin_wl_lib_override", "user", itoa64s(e.TargetTG),
			fmt.Sprintf("白名单库/并发覆盖：库 %s，并发上限 %d（已应用）", libDescText(lo.EnableAllFolders, len(lo.EnabledFolders)), lo.MaxActiveSessions))
		sendText(ctx, deps, cq.ChatID, fmt.Sprintf(
			"✅ 已保存并应用：tg=%d 库访问 %s，并发上限 %s。",
			e.TargetTG, libDescText(lo.EnableAllFolders, len(lo.EnabledFolders)), sessionsLimitText(lo.MaxActiveSessions)))
	case "clear":
		var u db.User
		if err := deps.DB.Where("telegram_id = ?", e.TargetTG).First(&u).Error; err != nil {
			sendText(ctx, deps, cq.ChatID, "目标用户不存在，已取消。")
			wlLibEditors.drop(cq.From.ID)
			return
		}
		if u.JellyLibOverride == "" {
			sendText(ctx, deps, cq.ChatID, "该用户未设置覆盖，无需清除。")
			return
		}
		if deps.JF == nil || u.JellyfinUserID == "" {
			// 仅清本地覆盖，Jellyfin 侧保持现状
			if err := deps.DB.Model(&u).Update("jelly_lib_override", "").Error; err != nil {
				sendText(ctx, deps, cq.ChatID, "清除失败："+jellyfinErrText(err))
				return
			}
		} else if err := saveOverrideAndApply(ctx, deps, &u, nil); err != nil {
			sendText(ctx, deps, cq.ChatID, "清除失败："+jellyfinErrText(err))
			return
		}
		_ = db.WriteAudit(deps.DB, cq.From.ID, "admin_wl_lib_clear", "user", itoa64s(e.TargetTG), "清除白名单库/并发覆盖（恢复跟随模板基线）")
		wlLibEditors.drop(cq.From.ID)
		sendText(ctx, deps, cq.ChatID, fmt.Sprintf("✅ 已清除覆盖：tg=%d 恢复跟随模板基线（已重套）。", e.TargetTG))
	case "cancel":
		wlLibEditors.drop(cq.From.ID)
		sendText(ctx, deps, cq.ChatID, "已取消，覆盖未改动。")
	}
}
