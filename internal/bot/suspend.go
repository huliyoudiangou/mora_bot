package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"mora_bot/internal/db"
)

// ---------------------------------------------------------------------------
// 管理员手动停用 / 解封账号
// ---------------------------------------------------------------------------
//
// 「是否被管理员停用」的唯一真相是本地 users.status：
//   - UserStatusInactive —— 管理员手动停用（封禁），只能由管理员解封；
//   - UserStatusExpired  —— 订阅到期由巡检自动停用，续期/提权可自动恢复。
//
// 两者严格区分：巡检只处理 active↔expired，永远不会自动恢复 inactive；反过来，
// 管理员停用可以覆盖 expired（停用优先级更高，解封时再按订阅状态回落到 expired）。
//
// 停用/解封都要同步 Jellyfin（SetUserDisabled）并踢掉在线会话：Jellyfin 的禁用只拦
// 新登录，已签发的 token 仍能继续播放，不同时踢会话等于没停用。

var (
	errSuspendTargetMissing = errors.New("目标用户不存在")
	errSuspendAlready       = errors.New("该账号已处于停用状态")
	errSuspendNot           = errors.New("该账号当前不是停用状态")
	errSuspendDeleted       = errors.New("该账号已注销")
	errSuspendJFSync        = errors.New("Jellyfin 侧同步失败")
	errSuspendStateChanged  = errors.New("账号状态刚被其它操作改动")
)

// suspendReasonMax 停用理由长度上限（与 SuspendReason 列宽对齐并留余量）。
const suspendReasonMax = 200

// suspendBlockedText 被管理员停用的用户尝试自助操作（注册/绑定/续期）时的统一提示。
const suspendBlockedText = "⛔ 你的账号已被管理员停用，暂时无法使用此功能。\n如有疑问请直接联系管理员。"

// accountSuspended 该用户是否处于「管理员手动停用」状态。
func accountSuspended(u *db.User) bool {
	return u != nil && u.Status == db.UserStatusInactive
}

// suspendErrText 把内部错误映射为可安全展示给管理员的文案。
func suspendErrText(err error) string {
	switch {
	case errors.Is(err, errSuspendTargetMissing):
		return "❌ 未找到该用户。"
	case errors.Is(err, errSuspendAlready):
		return "ℹ️ 该账号已处于停用状态，无需重复停用。"
	case errors.Is(err, errSuspendNot):
		return "ℹ️ 该账号当前不是停用状态，无需解封。"
	case errors.Is(err, errSuspendDeleted):
		return "❌ 该账号已注销，没有可停用的账号。"
	case errors.Is(err, errSuspendJFSync):
		return "❌ 同步 Jellyfin 失败，本地状态未改动（账号仍可用）。请确认 Jellyfin 服务正常后重试。"
	case errors.Is(err, errSuspendStateChanged):
		return "⚠️ 该账号状态刚被其它操作改动，本次操作已回滚，请重新查询后再试。"
	case errors.Is(err, db.ErrNilDB):
		return "❌ 数据库未就绪，请稍后再试。"
	default:
		return "❌ 操作失败，请稍后再试。"
	}
}

// sanitizeSuspendReason 归一化停用理由：去空白、限长，空则给默认值。
func sanitizeSuspendReason(reason string) string {
	r := strings.TrimSpace(reason)
	if r == "" {
		return "未填写理由"
	}
	return truncateRunes(r, suspendReasonMax)
}

// suspendNotice 告知用户账号已被停用（理由一并转达——管理员输入时会看到该提示）。
func suspendNotice(reason string) string {
	return "⛔ 你的账号已被管理员停用，暂时无法使用 Jellyfin。\n理由：" + reason +
		"\n\n如有疑问请直接联系管理员。"
}

// unsuspendNotice 告知用户账号已恢复。expired=true 表示订阅仍到期，账号依旧不可用。
func unsuspendNotice(expired bool) string {
	if expired {
		return "ℹ️ 你的账号已解除停用，但订阅仍处于到期状态（Jellyfin 账号依旧停用）。\n续期后即可继续使用：/shop buy 后再 /redeem。"
	}
	return "✅ 你的账号已解除停用，可以继续使用 Jellyfin 了。"
}

// SuspendUserAccount 管理员手动停用账号：
//  1. Jellyfin 侧禁用 + 踢掉全部在线会话（失败则本地状态不变，避免"假停用"）；
//  2. 本地 status=inactive + 记录理由（条件更新，与并发操作串行化）；
//  3. 私聊告知用户并写审计。
//
// 返回可直接发送给管理员的结果文案；失败时返回的错误请用 suspendErrText 渲染。
func SuspendUserAccount(ctx context.Context, deps *HandlerDeps, tgID int64, reason string, adminID int64) (string, error) {
	if deps == nil || deps.DB == nil {
		return "", db.ErrNilDB
	}
	var u db.User
	if err := deps.DB.Where("telegram_id = ?", tgID).First(&u).Error; err != nil {
		return "", errSuspendTargetMissing
	}
	switch u.Status {
	case db.UserStatusInactive:
		return "", errSuspendAlready
	case db.UserStatusDeleted:
		return "", errSuspendDeleted
	}
	reason = sanitizeSuspendReason(reason)

	// 1) 先同步 Jellyfin：这一步失败则本地状态保持原样，管理员可重试；
	//    反过来（先改本地）会留下"本地已停用、远端仍能登录"的假停用。
	if err := setJFDisabled(ctx, deps, u.JellyfinUserID, true); err != nil {
		return "", fmt.Errorf("%w: %v", errSuspendJFSync, err)
	}
	// 禁用只拦新登录，必须同时踢掉在线会话，停用才算真正落地（踢失败不阻断）。
	kicked, _ := kickSessions(ctx, deps, u.JellyfinUserID)

	// 2) 条件更新：只从读取时的状态迁移。RowsAffected==0 说明期间被并发改动
	//   （另一位管理员同时停用、用户恰好注销等），复核后再决定是否回滚 Jellyfin。
	res := deps.DB.Model(&db.User{}).
		Where("telegram_id = ? AND status = ?", tgID, u.Status).
		Updates(map[string]any{
			"status":         db.UserStatusInactive,
			"suspend_reason": reason,
		})
	if res.Error != nil || res.RowsAffected == 0 {
		var cur struct{ Status string }
		_ = deps.DB.Model(&db.User{}).Select("status").Where("telegram_id = ?", tgID).Scan(&cur).Error
		if cur.Status == db.UserStatusInactive {
			// 并发下另一位管理员已经停用成功：保留停用结果，绝不回滚 Jellyfin。
			return "", errSuspendAlready
		}
		_ = setJFDisabled(ctx, deps, u.JellyfinUserID, false)
		return "", errSuspendStateChanged
	}

	_ = db.WriteAudit(deps.DB, adminID, "admin_suspend", "user", itoa64s(tgID),
		"停用账号："+truncateRunes(reason, 80))
	notifyUserText(deps, tgID, suspendNotice(reason))

	detail := "（未绑定 Jellyfin，仅停用本地账号）"
	if u.JellyfinUserID != "" {
		detail = "（Jellyfin 账号已禁用并断开在线会话）"
		if !kicked {
			detail = "（Jellyfin 账号已禁用）"
		}
	}
	return fmt.Sprintf("✅ 已停用账号：tg=%d %s\n理由：%s\n\n该账号将无法再注册/绑定/续期，解封请点「✅ 解除停用」。",
		tgID, detail, reason), nil
}

// UnsuspendUserAccount 管理员解除停用：
//   - 订阅仍到期（非白名单且 expire_at 已过）→ 回落到 expired（Jellyfin 保持禁用），
//     由续期/提权负责恢复，避免"刚解封就白送一段已过期的服务"；
//   - 否则恢复 active 并启用 Jellyfin。
//
// 返回可直接发送给管理员的结果文案；失败时返回的错误请用 suspendErrText 渲染。
func UnsuspendUserAccount(ctx context.Context, deps *HandlerDeps, tgID int64, adminID int64) (string, error) {
	if deps == nil || deps.DB == nil {
		return "", db.ErrNilDB
	}
	var u db.User
	if err := deps.DB.Where("telegram_id = ?", tgID).First(&u).Error; err != nil {
		return "", errSuspendTargetMissing
	}
	if u.Status != db.UserStatusInactive {
		return "", errSuspendNot
	}
	now := time.Now()
	backToExpired := !u.IsPermanent && u.ExpireAt != nil && !u.ExpireAt.After(now)
	target := db.UserStatusActive
	if backToExpired {
		target = db.UserStatusExpired
	}
	// 先同步 Jellyfin 再改本地：启用失败则保持 inactive，管理员可重试。
	if !backToExpired {
		if err := setJFDisabled(ctx, deps, u.JellyfinUserID, false); err != nil {
			return "", fmt.Errorf("%w: %v", errSuspendJFSync, err)
		}
	}
	res := deps.DB.Model(&db.User{}).
		Where("telegram_id = ? AND status = ?", tgID, db.UserStatusInactive).
		Updates(map[string]any{
			"status":         target,
			"suspend_reason": "",
		})
	if res.Error != nil || res.RowsAffected == 0 {
		// 并发下状态已被改动：本地没有写入，把刚才的 Jellyfin 启用改回去。
		if !backToExpired {
			_ = setJFDisabled(ctx, deps, u.JellyfinUserID, true)
		}
		return "", errSuspendStateChanged
	}

	_ = db.WriteAudit(deps.DB, adminID, "admin_unsuspend", "user", itoa64s(tgID), "解除停用")
	notifyUserText(deps, tgID, unsuspendNotice(backToExpired))
	if backToExpired {
		return fmt.Sprintf("✅ 已解除停用：tg=%d\n⚠️ 该账号订阅已到期，已回到「到期停用」状态（Jellyfin 仍禁用）。续期后自动恢复。", tgID), nil
	}
	return fmt.Sprintf("✅ 已解除停用：tg=%d（Jellyfin 账号已恢复启用）", tgID), nil
}

// ---------------------------------------------------------------------------
// 管理侧：用户卡片 / 停用向导 / 命令 / 名单
// ---------------------------------------------------------------------------

// userStatusText 本地账号状态的中文描述（管理侧展示用）。
func userStatusText(s string) string {
	switch s {
	case db.UserStatusActive:
		return "active（正常）"
	case db.UserStatusInactive:
		return "inactive（管理员停用）"
	case db.UserStatusExpired:
		return "expired（到期停用）"
	case db.UserStatusDeleted:
		return "deleted（已注销）"
	default:
		return s
	}
}

// adminUserCard 管理员用户详情卡片：展示档案 + 停用/解封按钮。
func adminUserCard(deps *HandlerDeps, u *db.User) (string, [][]KeyboardButton) {
	isAdmin := deps.IsSuper != nil && deps.IsSuper(u.TelegramID)
	perm := "否"
	if u.IsPermanent {
		perm = "是（白名单）"
	}
	expire := "无"
	if u.ExpireAt != nil {
		expire = u.ExpireAt.Format("2006-01-02")
	}
	if u.IsPermanent {
		// 白名单用户不存在到期时间（提权时已清空），与"白名单：是"并排显示到期日会误导管理员。
		expire = "无（白名单永久）"
	}
	sec := "否"
	if u.SecurityCodeHash != "" {
		sec = "是"
	}
	text := fmt.Sprintf(
		"👤 <b>tg=%d</b>\n用户名：%s %s\nJellyfin：%s（%s）\n果果币：%d\n状态：%s\n白名单：%s\n到期：%s\n连签：%d 天\n安全码：%s\n管理员：%v",
		u.TelegramID, escapeHTML(u.FirstName), escapeHTML(u.LastName),
		escapeHTML(u.JellyfinUsername), escapeHTML(u.JellyfinUserID),
		u.GuoGuo, userStatusText(u.Status), perm, expire, u.SignStreak, sec, isAdmin)
	if u.Status == db.UserStatusInactive {
		reason := strings.TrimSpace(u.SuspendReason)
		if reason == "" {
			reason = "（未记录）"
		}
		text += "\n停用理由：" + escapeHTML(truncateRunes(reason, 80))
	}

	rows := [][]KeyboardButton{}
	switch u.Status {
	case db.UserStatusInactive:
		rows = append(rows, []KeyboardButton{
			{Text: "✅ 解除停用", Data: BuildCallbackData(DKAdmin, "user", "unsuspend", itoa64s(u.TelegramID))},
		})
	case db.UserStatusDeleted:
		// 已注销账号没有可停用的对象（Jellyfin 绑定已清空）。
	default:
		rows = append(rows, []KeyboardButton{
			{Text: "🚫 停用账号", Data: BuildCallbackData(DKAdmin, "user", "suspend", itoa64s(u.TelegramID))},
		})
	}
	rows = append(rows, []KeyboardButton{
		{Text: "↩️ 返回管理面板", Data: BuildCallbackData(DKAdmin, "view")},
	})
	return text, rows
}

// handleAdminSuspendStep 停用向导：收停用理由 → 执行停用（tg_id 由卡片按钮带入会话）。
func (r *Router) handleAdminSuspendStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消停用操作。")
		return
	}
	tgID, _ := sess.Data["tg_id"].(int64)
	if tgID == 0 {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "会话已失效，请重新查询用户后再操作。")
		return
	}
	reason := strings.TrimSpace(msg.Text)
	// 与 sanitizeSuspendReason 的上限一致：超长直接打回，避免理由被静默截断。
	if n := len([]rune(reason)); n > suspendReasonMax {
		sendText(ctx, deps, msg.ChatID, fmt.Sprintf("理由过长（%d 字，上限 %d），请精简后重发。", n, suspendReasonMax))
		return
	}
	deps.Sessions.Clear(msg.From.ID)
	res, err := SuspendUserAccount(ctx, deps, tgID, reason, msg.From.ID)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, suspendErrText(err))
		return
	}
	sendText(ctx, deps, msg.ChatID, res)
}

// cmdAdminSuspend /admin suspend [tg_id] [停用理由...]
// 不带参数时列出当前被停用的账号（便于核对与解封）。
func (r *Router) cmdAdminSuspend(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	if len(args) == 0 {
		r.sendSuspendedList(ctx, deps, msg.ChatID)
		return
	}
	tgID := resolveUserID(args, 0)
	if tgID == 0 {
		sendText(ctx, deps, msg.ChatID, "用法：/admin suspend <tg_id> [停用理由]\n不带参数则列出当前被停用的账号。")
		return
	}
	res, err := SuspendUserAccount(ctx, deps, tgID, strings.Join(args[1:], " "), msg.From.ID)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, suspendErrText(err))
		return
	}
	sendText(ctx, deps, msg.ChatID, res)
}

// cmdAdminUnsuspend /admin unsuspend <tg_id>
func (r *Router) cmdAdminUnsuspend(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	tgID := resolveUserID(args, 0)
	if tgID == 0 {
		sendText(ctx, deps, msg.ChatID, "用法：/admin unsuspend <tg_id>")
		return
	}
	res, err := UnsuspendUserAccount(ctx, deps, tgID, msg.From.ID)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, suspendErrText(err))
		return
	}
	sendText(ctx, deps, msg.ChatID, res)
}

// sendSuspendedList 列出当前被管理员停用的账号（含理由），便于核对与解封。
func (r *Router) sendSuspendedList(ctx context.Context, deps *HandlerDeps, chatID int64) {
	// 截断展示，避免名单很长时消息超过 Telegram 4096 上限被整条拒发。
	const maxShow = 15
	if deps == nil || deps.DB == nil {
		sendText(ctx, deps, chatID, "数据库未就绪，请稍后再试。")
		return
	}
	var total int64
	deps.DB.Model(&db.User{}).Where("status = ?", db.UserStatusInactive).Count(&total)
	if total == 0 {
		sendText(ctx, deps, chatID, "🚫 当前没有被管理员停用的账号。")
		return
	}
	var users []db.User
	if err := deps.DB.Where("status = ?", db.UserStatusInactive).
		Order("updated_at desc").Limit(maxShow).Find(&users).Error; err != nil {
		sendText(ctx, deps, chatID, "查询失败，请稍后再试。")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🚫 <b>管理员停用名单</b>（共 %d 人）\n", total)
	for _, u := range users {
		line := fmt.Sprintf("· %s（tg=%d）", escapeHTML(truncateRunes(u.DisplayName(), 20)), u.TelegramID)
		if u.JellyfinUsername != "" {
			line += " · JF:" + escapeHTML(truncateRunes(u.JellyfinUsername, 24))
		}
		if reason := strings.TrimSpace(u.SuspendReason); reason != "" {
			line += " · 理由：" + escapeHTML(truncateRunes(reason, 30))
		}
		b.WriteString(line + "\n")
	}
	if total > int64(maxShow) {
		fmt.Fprintf(&b, "…仅显示前 %d 人\n", maxShow)
	}
	b.WriteString("\n解封：<code>/admin unsuspend &lt;tg_id&gt;</code>")
	sendHTML(ctx, deps, chatID, b.String())
}

// handleAdminUserAction 用户卡片上的停用/解封按钮。
// 回调格式：admin:user:suspend:<tg_id> / admin:user:unsuspend:<tg_id>。
func handleAdminUserAction(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, args []string) {
	if deps.IsSuper == nil || !deps.IsSuper(cq.From.ID) {
		return
	}
	if len(args) < 2 {
		sendText(ctx, deps, cq.ChatID, "参数错误，请重新查询用户。")
		return
	}
	tgID := parseInt64Safe(args[1])
	if tgID == 0 {
		sendText(ctx, deps, cq.ChatID, "参数错误，请重新查询用户。")
		return
	}
	switch args[0] {
	case "suspend":
		// 停用需要理由（会一并告知用户）：先开会话收理由，避免"一键停用"误触。
		var u db.User
		if err := deps.DB.Where("telegram_id = ?", tgID).First(&u).Error; err != nil {
			sendText(ctx, deps, cq.ChatID, "未找到该用户。")
			return
		}
		if accountSuspended(&u) {
			sendText(ctx, deps, cq.ChatID, "ℹ️ 该账号已处于停用状态。")
			return
		}
		if u.Status == db.UserStatusDeleted {
			sendText(ctx, deps, cq.ChatID, "❌ 该账号已注销，没有可停用的账号。")
			return
		}
		deps.Sessions.Begin(cq.From.ID, sessAdminSuspend)
		deps.Sessions.Advance(cq.From.ID, map[string]any{"tg_id": tgID})
		sendHTML(ctx, deps, cq.ChatID, fmt.Sprintf(
			"🚫 <b>停用账号</b> tg=%d\n\n请回复<b>停用理由</b>（会一并告知该用户，例如：违规分享 / 账号共享）。\n\n回复 /cancel 可取消。", tgID))
	case "unsuspend":
		res, err := UnsuspendUserAccount(ctx, deps, tgID, cq.From.ID)
		if err != nil {
			sendText(ctx, deps, cq.ChatID, suspendErrText(err))
			return
		}
		sendText(ctx, deps, cq.ChatID, res)
	}
}
