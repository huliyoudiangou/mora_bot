package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mora_bot/internal/codes"
	"mora_bot/internal/db"
)

// Session kinds for multi-step flows.
const (
	sessRegInvite      = "reg_invite"
	sessRegUsername    = "reg_username"
	sessRegPassword    = "reg_password"
	sessRegSecurity    = "reg_security" // 注册第 4 步：设置安全码（开注时第 3 步）
	sessBindExistU     = "bind_exist_user"
	sessBindExistPw    = "bind_exist_pw"
	sessRedeem         = "redeem_code"      // 面板「使用续期码」：收卡密
	sessDramaFeedback  = "drama_feedback"   // 第 1 步：收分享链接/剧名
	sessDramaInfo      = "drama_info"       // 第 2 步：补 剧名+主演名
	sessAdminGenInvite = "admin_gen_invite" // 管理面板生成邀请码：收数量
	sessAdminGenRenew  = "admin_gen_renew"  // 管理面板生成续期码：第 1 步收天数 → 第 2 步收数量

	// 管理面板新增功能会话
	sessAdminAdjPoints = "admin_adj_points" // 调积分：第 1 步 tg_id → 第 2 步 delta
	sessAdminQueryUser = "admin_query_user" // 查用户：收 tg_id
	sessAdminQueryCode = "admin_query_code" // 查卡密：收卡密
	sessAdminWL        = "admin_whitelist"  // 白名单：Data.mode=add/del，收 tg_id
	sessAdminPrice     = "admin_set_price"  // 定价：Data.kind=invite/renewal，收积分价
	sessAdminLineAdd   = "admin_line_add"   // 添加线路：收 URL
	sessAdminLineDel   = "admin_line_del"   // 删除线路：收 id 或 URL
	sessAdminQuota     = "admin_set_quota"  // 设置积分兑换邀请码配额：收整数
	sessAdminDramaRej  = "admin_drama_rej"  // 求剧工单驳回：收理由（Data.req_id/msg_id）

	// 账号安全会话
	sessSetSecurity = "account_set_security"  // 设置/修改安全码：第 1 步收码 → 第 2 步确认
	sessPwdChange   = "account_pwd_change"    // 修改密码：第 1 步安全码 → 第 2 步旧密码 → 第 3 步新密码
	sessUnbind      = "account_unbind"        // 解绑：第 1 步安全码 → 第 2 步确认
)

// cmdRegister /register 注册新 Jellyfin 账号（开注时免邀请码，否则需邀请码）。
func (r *Router) cmdRegister(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		return
	}
	if u.JellyfinUserID != "" {
		sendText(ctx, deps, msg.ChatID, "你已有关联的 Jellyfin 账号，无需重复注册。如需更换请先解绑。")
		return
	}
	if registrationOpen(deps) {
		// 开注：免邀请码，直接收用户名
		deps.Sessions.Begin(msg.From.ID, sessRegUsername)
		sendText(ctx, deps, msg.ChatID,
			"📝 注册新账号（开注中，免邀请码）\n第 1/3 步：请设置你的 Jellyfin 用户名。")
		return
	}
	deps.Sessions.Begin(msg.From.ID, sessRegInvite)
	sendText(ctx, deps, msg.ChatID,
		"📝 注册新账号（需邀请码）\n第 1/4 步：请发送你的邀请码。")
}

// handleRegStepInvite 注册第 1 步，收邀请码。
func (r *Router) handleRegStepInvite(ctx context.Context, msg *Message) {
	deps := r.deps
	code := strings.TrimSpace(msg.Text)
	if code == "" {
		sendText(ctx, deps, msg.ChatID, "邀请码不能为空，请重新输入。")
		return
	}
	inv, ok, err := dbFindInviteCode(deps.DB, code, deps.Pepper)
	if err != nil || !ok {
		sendText(ctx, deps, msg.ChatID, "邀请码无效或已被使用。")
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	sess := deps.Sessions.Advance(msg.From.ID, map[string]any{"invite_id": inv.ID, "code": code})
	sess.Kind = sessRegUsername
	sendText(ctx, deps, msg.ChatID, "第 2/4 步：请设置你的 Jellyfin 用户名。")
}

// handleRegStepPassword 注册第 2 步：收用户名 (kind == reg_username)。
func (r *Router) handleRegStepPassword(ctx context.Context, msg *Message) {
	deps := r.deps
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		return
	}
	uName := strings.TrimSpace(msg.Text)
	if uName == "" || len(uName) > 32 {
		sendText(ctx, deps, msg.ChatID, "用户名不合法，请重新输入。")
		return
	}
	s2 := deps.Sessions.Advance(msg.From.ID, map[string]any{"username": uName})
	s2.Kind = sessRegPassword
	sendText(ctx, deps, msg.ChatID, "第 3/4 步：请设置 Jellyfin 密码（至少 6 位）。")
}

// handleRegStepResetPw 注册第 3 步：收密码。
func (r *Router) handleRegStepResetPw(ctx context.Context, msg *Message) {
	deps := r.deps
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		return
	}
	pwd := strings.TrimSpace(msg.Text)
	if len(pwd) < 6 {
		sendText(ctx, deps, msg.ChatID, "密码过短，请重新输入。")
		return
	}
	s2 := deps.Sessions.Advance(msg.From.ID, map[string]any{"pwd": pwd})
	s2.Kind = sessRegSecurity
	sendText(ctx, deps, msg.ChatID, "第 4/4 步：请设置安全码（4-20 位，用于后续改密/解绑校验）。")
}

// handleRegStepSecurity 注册第 4 步：收安全码并完成创建。
func (r *Router) handleRegStepSecurity(ctx context.Context, msg *Message) {
	deps := r.deps
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		return
	}
	secCode, err := codes.ValidateSecurityCode(msg.Text)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "安全码不合法："+err.Error()+"\n请使用 4-20 位字母/数字/_-.")
		return
	}
	if deps.JF == nil {
		sendText(ctx, deps, msg.ChatID, "Jellyfin 服务未配置，无法完成注册。")
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	secHash, err := codes.HashSecurityCode(secCode, deps.Pepper)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "安全码处理失败，请重试。")
		return
	}
	uName, _ := sess.Data["username"].(string)
	pwd, _ := sess.Data["pwd"].(string)
	ju, err := deps.JF.CreateUser(ctx, uName, pwd)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "创建 Jellyfin 用户失败："+err.Error())
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	// 权限/设置克隆自模板用户（Policy + Configuration 1:1 复刻）
	if deps.JFServerBase != "" {
		if err := deps.JF.ClonePolicyFromTemplate(ctx, deps.JFServerBase, ju.ID); err != nil {
			sendText(ctx, deps, msg.ChatID, "注册成功，但模板权限同步失败："+err.Error())
		}
	}
	u, err := ensureUser(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "本地更新失败，稍后再试。")
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	_ = deps.DB.Model(u).Updates(map[string]any{
		"jellyfin_user_id":  ju.ID,
		"jellyfin_username": uName,
		"bind_type":         db.BindTypeRegistered,
		"status":            db.UserStatusActive, // 重新注册后恢复活跃（曾注销的用户）
		"security_code_hash": secHash,
	}).Error
	// 新注册账号默认有效期（NEW_ACCOUNT_VALID_DAYS，0=永久）
	if deps.NewAccountValidDays > 0 && u.ExpireAt == nil && !u.IsPermanent {
		t := time.Now().AddDate(0, 0, deps.NewAccountValidDays)
		_ = deps.DB.Model(u).Update("expire_at", t).Error
	}
	// 消耗邀请码（开注模式无 invite_id）
	if id, ok := sess.Data["invite_id"].(uint); ok && id != 0 {
		markUsedInvite(deps, id, u.TelegramID)
	}
	deps.Sessions.Clear(msg.From.ID)
	sendText(ctx, deps, msg.ChatID, "✅ 注册成功，欢迎加入果果屋。")
}

// cmdBind /bind 绑定已有 Jellyfin 账号到本机（区别于 /register 注册新号）。
func (r *Router) cmdBind(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	if deps.JF == nil {
		sendText(ctx, deps, msg.ChatID, "Jellyfin 服务未配置，无法绑定。")
		return
	}
	_, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		return
	}
	// 允许换绑：已有绑定也直接覆盖
	deps.Sessions.Begin(msg.From.ID, sessBindExistU)
	sendText(ctx, deps, msg.ChatID,
		"🔗 绑定已有 Jellyfin 账号\n第 1/2 步：请输入你的 Jellyfin 用户名。")
}

// handleBindExistUser 绑定第 1 步：收用户名。
func (r *Router) handleBindExistUser(ctx context.Context, msg *Message) {
	deps := r.deps
	uName := strings.TrimSpace(msg.Text)
	if uName == "" || len(uName) > 64 {
		sendText(ctx, deps, msg.ChatID, "用户名不合法，请重新输入。")
		return
	}
	sess := deps.Sessions.Advance(msg.From.ID, map[string]any{"username": uName})
	sess.Kind = sessBindExistPw
	sendText(ctx, deps, msg.ChatID, "第 2/2 步：请输入该账号的密码。")
}

// handleBindExistPw 绑定第 2 步：收密码并完成关联。
func (r *Router) handleBindExistPw(ctx context.Context, msg *Message) {
	deps := r.deps
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		return
	}
	defer deps.Sessions.Clear(msg.From.ID)
	pwd := strings.TrimSpace(msg.Text)
	if pwd == "" {
		sendText(ctx, deps, msg.ChatID, "密码不能为空。")
		return
	}
	uName, _ := sess.Data["username"].(string)
	// 1. 用户侧自证（Jellyfin 官方认证接口）
	ok, err := deps.JF.AuthenticateByName(ctx, uName, pwd)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "验证失败："+err.Error())
		return
	}
	if !ok {
		sendText(ctx, deps, msg.ChatID, "用户名或密码错误，绑定失败。")
		return
	}
	// 2. 管理员 API 拿到该用户的 ID
	ju, found, err := deps.JF.FindUserByName(ctx, uName)
	if err != nil || !found {
		sendText(ctx, deps, msg.ChatID, "未能在 Jellyfin 找到该账号，请稍后再试。")
		return
	}
	u, err := ensureUser(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "本地更新失败，稍后再试。")
		return
	}
	_ = deps.DB.Model(u).Updates(map[string]any{
		"jellyfin_user_id":  ju.ID,
		"jellyfin_username": ju.Name,
		"bind_type":         db.BindTypeExisting,
		"status":            db.UserStatusActive,
	}).Error
	sendText(ctx, deps, msg.ChatID, fmt.Sprintf("✅ 绑定成功：已关联 Jellyfin 账号「%s」。", ju.Name))
}

// handleDramaStepFeedback 追剧第 1 步：用户发送红果短剧分享链接（或剧名+主演名）。
func (r *Router) handleDramaStepFeedback(ctx context.Context, msg *Message) {
	deps := r.deps
	raw := strings.TrimSpace(msg.Text)
	if raw == "" {
		sendText(ctx, deps, msg.ChatID, "内容为空，请重新发送。")
		return
	}
	if strings.EqualFold(raw, "/cancel") {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消求剧。")
		return
	}
	if len(raw) > 2000 {
		sendText(ctx, deps, msg.ChatID, "内容过长，请精简后重发（链接 + 剧名即可）。")
		return
	}

	link := extractURL(raw)
	rest := removeURL(raw)
	title, actor := parseTitleActor(rest)
	// 从《》提取剧名（红果分享文案常含）
	if bt := extractTitleFromBrackets(raw); bt != "" {
		title = bt
	}

	// 有链接但没解析出剧名 → 第 2 步补充
	if link != "" && strings.TrimSpace(title) == "" {
		deps.Sessions.Begin(msg.From.ID, sessDramaInfo)
		deps.Sessions.Advance(msg.From.ID, map[string]any{"link": link, "actor": actor})
		sendText(ctx, deps, msg.ChatID, "已收到分享链接 ✅\n请再补充：剧名 和 主演名\n（例如：双面人生 / 杨幂，没有可只发剧名）")
		return
	}
	// 无链接：必须有剧名+主演名才直接建单，否则第 2 步补充
	if link == "" && (strings.TrimSpace(title) == "" || strings.TrimSpace(actor) == "") {
		deps.Sessions.Begin(msg.From.ID, sessDramaInfo)
		deps.Sessions.Advance(msg.From.ID, map[string]any{"raw": raw})
		sendText(ctx, deps, msg.ChatID, "未检测到分享链接。\n请补充：剧名 和 主演名\n（例如：双面人生 / 杨幂；没有主演可只发剧名）")
		return
	}

	req, ok := createDramaTicket(ctx, deps, msg, title, link, actor)
	if !ok {
		return
	}
	deps.Sessions.Clear(msg.From.ID)
	sendText(ctx, deps, msg.ChatID, fmt.Sprintf("✅ 求剧工单 #%d 已创建：%s", req.ID, req.Title))
}

// handleDramaStepInfo 追剧第 2 步：补齐 剧名+主演名（无链接时）。
func (r *Router) handleDramaStepInfo(ctx context.Context, msg *Message) {
	deps := r.deps
	sess := deps.Sessions.Current(msg.From.ID)
	defer deps.Sessions.Clear(msg.From.ID)
	raw := strings.TrimSpace(msg.Text)
	if raw == "" {
		sendText(ctx, deps, msg.ChatID, "内容为空，已取消。重新点「我要求剧」再试。")
		return
	}
	if strings.EqualFold(raw, "/cancel") {
		sendText(ctx, deps, msg.ChatID, "已取消求剧。")
		return
	}
	if len(raw) > 500 {
		sendText(ctx, deps, msg.ChatID, "内容过长，请精简。")
		return
	}
	// 历史待补充数据（第一步可能已给过链接/部分信息）
	var link string
	if sess != nil {
		link, _ = sess.Data["link"].(string)
	}
	title, actor := parseTitleActor(raw)
	if bt := extractTitleFromBrackets(raw); bt != "" {
		title = bt
	}
	// 第一步若已提供演员名，保留
	if sess != nil {
		if prevActor, _ := sess.Data["actor"].(string); prevActor != "" && actor == "" {
			actor = prevActor
		}
	}
	if strings.TrimSpace(title) == "" && strings.TrimSpace(link) == "" {
		sendText(ctx, deps, msg.ChatID, "还是没收到有效内容，已取消。请重新点「我要求剧」，发送链接或 剧名/主演名。")
		return
	}
	if title == "" {
		title = "（未提供剧名）"
	}
	req, ok := createDramaTicket(ctx, deps, msg, title, link, actor)
	if !ok {
		return
	}
	sendText(ctx, deps, msg.ChatID, fmt.Sprintf("✅ 求剧工单 #%d 已创建：%s", req.ID, req.Title))
}
