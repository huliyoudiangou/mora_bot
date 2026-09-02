package bot

import (
	"context"
	"fmt"
	"strings"

	"mora_bot/internal/codes"
	"mora_bot/internal/db"
	"mora_bot/internal/jellyfin"
)

// cmdAccount /account 自助面板欢迎。
func (r *Router) cmdAccount(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		return
	}
	if len(args) == 0 {
		text, rows := accountPanel(deps, u)
		sendPanel(ctx, deps, msg.ChatID, 0, text, rows)
		return
	}
	switch strings.ToLower(args[0]) {
	case "pwd":
		r.cmdAccountPwd(ctx, msg, args[1:])
	case "security":
		r.cmdAccountSecurity(ctx, msg, args[1:])
	case "unbind":
		// /account unbind 始终走安全码向导，避免直接 CONFIRM 绕过身份校验
		r.cmdAccountUnbind(ctx, msg)
	case "delete":
		r.cmdAccountDelete(ctx, msg, args[1:])
	default:
		sendText(ctx, deps, msg.ChatID, "不认识的子命令：/account pwd|security|unbind|delete")
	}
}

// accountBindStatus 展示文字。
func accountBindStatus(u *db.User) string {
	if u == nil {
		return "未知"
	}
	if u.JellyfinUserID == "" {
		return "未绑定 Jellyfin"
	}
	return fmt.Sprintf("已绑定 Jellyfin（%s）", escapeHTML(u.JellyfinUserID))
}

// cmdAccountPwd 修改密码：开始安全码向导（安全码 → 旧密码 → 新密码）。
func (r *Router) cmdAccountPwd(ctx context.Context, msg *Message, _ []string) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		return
	}
	if u.JellyfinUserID == "" {
		sendText(ctx, deps, msg.ChatID, "尚未绑定 Jellyfin 账号，无法修改密码。")
		return
	}
	if deps.JF == nil {
		sendText(ctx, deps, msg.ChatID, "Jellyfin 服务未配置，无法修改密码。")
		return
	}
	if u.SecurityCodeHash == "" {
		sendText(ctx, deps, msg.ChatID, "你还没有设置安全码。请先点「🔐 设置安全码」或发送 /account security 设置后，再修改密码。")
		return
	}
	deps.Sessions.Begin(msg.From.ID, sessPwdChange)
	sendHTML(ctx, deps, msg.ChatID, "🔑 修改密码 · 第 1/3 步\n请输入你的<b>安全码</b>：\n\n回复 /cancel 可取消。")
}

// handlePwdChangeStep 修改密码向导：安全码 → 旧密码 → 新密码。
func (r *Router) handlePwdChangeStep(ctx context.Context, msg *Message) {
	deps := r.deps
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消修改密码。")
		return
	}
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	switch sess.Step {
	case 0: // 安全码
		hash, err := codes.HashSecurityCode(strings.TrimSpace(msg.Text), deps.Pepper)
		if err != nil || hash != u.SecurityCodeHash {
			sendText(ctx, deps, msg.ChatID, "❌ 安全码错误，请重新输入（或输入 /cancel 放弃）。")
			return
		}
		s2 := deps.Sessions.Advance(msg.From.ID, nil)
		s2.Step = 1
		sendHTML(ctx, deps, msg.ChatID, "🔑 第 2/3 步\n请输入你的<b>旧密码</b>：\n\n回复 /cancel 可取消。")
	case 1: // 旧密码
		oldPw := strings.TrimSpace(msg.Text)
		if oldPw == "" {
			sendText(ctx, deps, msg.ChatID, "旧密码不能为空。")
			return
		}
		if deps.JF == nil {
			deps.Sessions.Clear(msg.From.ID)
			sendText(ctx, deps, msg.ChatID, "Jellyfin 服务未配置，无法修改密码。")
			return
		}
		res, err := deps.JF.AuthenticateByName(ctx, u.JellyfinUsername, oldPw)
		if err != nil {
			sendText(ctx, deps, msg.ChatID, "连接 Jellyfin 失败，请稍后再试。")
			return
		}
		switch res {
		case jellyfin.AuthOK:
			// 继续第 3 步
		case jellyfin.AuthBlocked:
			// 密码大概率是对的，是 Jellyfin 不肯建会话（多为在线设备数达上限或账号被停用）。
			// 绝不能报"旧密码错误"——那会让用户以为自己密码丢了。
			// 用户已经通过安全码校验且本地绑定属于本人，因此这里不再卡死流程，
			// 允许继续设置新密码；同时明确告知限制原因和后续处理方式。
			s2 := deps.Sessions.Advance(msg.From.ID, map[string]any{"old_pw": oldPw})
			s2.Step = 2
			sendHTML(ctx, deps, msg.ChatID,
				"⚠️ 旧密码暂时无法在线校验，<b>这不代表你记错了密码</b>。\n\n"+
					"通常是<b>同时在线设备数已达上限</b>，也可能是账号被管理员停用。\n"+
					"你已通过<b>安全码</b>验证，可以继续设置新密码。\n"+
					"若修改后仍无法登录，请先到「⚙️ 账号管理 → 📱 登录设备」清理设备。\n\n"+
					"🔑 第 3/3 步\n请输入你的<b>新密码</b>（至少 6 位）：\n\n回复 /cancel 可取消。")
			return
		default: // jellyfin.AuthBadCredentials
			sendText(ctx, deps, msg.ChatID, "❌ 旧密码错误，请重新输入。")
			return
		}
		s2 := deps.Sessions.Advance(msg.From.ID, map[string]any{"old_pw": oldPw})
		s2.Step = 2
		sendHTML(ctx, deps, msg.ChatID, "🔑 第 3/3 步\n请输入你的<b>新密码</b>（至少 6 位）：\n\n回复 /cancel 可取消。")
	case 2: // 新密码
		newPw := strings.TrimSpace(msg.Text)
		if len(newPw) < 6 {
			sendText(ctx, deps, msg.ChatID, "新密码过短（至少 6 位），请重新输入。")
			return
		}
		if err := deps.JF.AdminSetPassword(ctx, u.JellyfinUserID, newPw); err != nil {
			sendText(ctx, deps, msg.ChatID, "修改密码失败："+err.Error())
			return
		}
		deps.Sessions.Clear(msg.From.ID)
		_ = db.WriteAudit(deps.DB, msg.From.ID, "user_change_pwd", "user", itoa(int(u.TelegramID)), "修改密码(安全码校验)")
		sendText(ctx, deps, msg.ChatID, "✅ 密码已更新。出于安全考虑，请重新登录 Jellyfin。")
	}
}

// cmdAccountSecurity 设置/修改安全码：开始向导（新码 → 确认）。
func (r *Router) cmdAccountSecurity(ctx context.Context, msg *Message, _ []string) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		return
	}
	if u.SecurityCodeHash != "" {
		sendText(ctx, deps, msg.ChatID, "你已经设置过安全码。如需修改，请联系管理员重置。")
		return
	}
	deps.Sessions.Begin(msg.From.ID, sessSetSecurity)
	sendHTML(ctx, deps, msg.ChatID, "🔐 设置安全码 · 第 1/2 步\n请设置你的<b>安全码</b>（4-20 位字母/数字，用于改密/解绑校验）：\n\n回复 /cancel 可取消。")
}

// handleSetSecurityStep 设置安全码向导：新码 → 确认。
func (r *Router) handleSetSecurityStep(ctx context.Context, msg *Message) {
	deps := r.deps
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消设置安全码。")
		return
	}
	switch sess.Step {
	case 0: // 新安全码
		code, err := codes.ValidateSecurityCode(msg.Text)
		if err != nil {
			sendText(ctx, deps, msg.ChatID, "安全码不合法："+err.Error()+"\n请使用 4-20 位字母/数字/_-.")
			return
		}
		s2 := deps.Sessions.Advance(msg.From.ID, map[string]any{"new_code": code})
		s2.Step = 1
		sendHTML(ctx, deps, msg.ChatID, "🔐 第 2/2 步\n请<b>再次输入</b>安全码确认：\n\n回复 /cancel 可取消。")
	case 1: // 确认
		code, _ := sess.Data["new_code"].(string)
		if strings.TrimSpace(msg.Text) != code {
			sendText(ctx, deps, msg.ChatID, "两次输入不一致，请重新点「🔐 设置安全码」再来。")
			deps.Sessions.Clear(msg.From.ID)
			return
		}
		hash, err := codes.HashSecurityCode(code, deps.Pepper)
		if err != nil {
			sendText(ctx, deps, msg.ChatID, "处理失败，请重试。")
			deps.Sessions.Clear(msg.From.ID)
			return
		}
		deps.Sessions.Clear(msg.From.ID)
		if err := deps.DB.Model(&db.User{}).Where("telegram_id = ?", msg.From.ID).Update("security_code_hash", hash).Error; err != nil {
			sendText(ctx, deps, msg.ChatID, "保存安全码失败："+err.Error())
			return
		}
		sendText(ctx, deps, msg.ChatID, "✅ 安全码已设置。今后修改密码、解绑都需要它，请牢记（若遗忘需联系管理员）。")
	}
}

// cmdAccountUnbind 解绑 Jellyfin：开始安全码向导。
func (r *Router) cmdAccountUnbind(ctx context.Context, msg *Message) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败。")
		return
	}
	if u.JellyfinUserID == "" {
		sendText(ctx, deps, msg.ChatID, "本来就未绑定。")
		return
	}
	if u.SecurityCodeHash == "" {
		sendText(ctx, deps, msg.ChatID, "你还没有设置安全码。请先点「🔐 设置安全码」或发送 /account security 设置后，再解绑。")
		return
	}
	deps.Sessions.Begin(msg.From.ID, sessUnbind)
	sendText(ctx, deps, msg.ChatID,
		"🔗 解除绑定\n请输入你的<b>安全码</b>以验证身份：\n\n回复 /cancel 可取消。")
}

// handleUnbindStep 解绑向导：安全码 → 确认。
func (r *Router) handleUnbindStep(ctx context.Context, msg *Message) {
	deps := r.deps
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消解绑。")
		return
	}
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败。")
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	switch sess.Step {
	case 0: // 安全码
		hash, err := codes.HashSecurityCode(strings.TrimSpace(msg.Text), deps.Pepper)
		if err != nil || hash != u.SecurityCodeHash {
			sendText(ctx, deps, msg.ChatID, "❌ 安全码错误，请重新输入（或输入 /cancel 放弃）。")
			return
		}
		s2 := deps.Sessions.Advance(msg.From.ID, nil)
		s2.Step = 1
		sendText(ctx, deps, msg.ChatID,
			"✅ 安全码验证通过。\n请发送 <b>CONFIRM</b> 确认解绑（解绑后当前 Jellyfin 账号将不再关联，但订阅保留，可换绑）。")
	case 1: // CONFIRM
		if !strings.EqualFold(strings.TrimSpace(msg.Text), "CONFIRM") {
			sendHTML(ctx, deps, msg.ChatID, "请发送 <b>CONFIRM</b> 确认解绑，或 /cancel 放弃。")
			return
		}
		deps.Sessions.Clear(msg.From.ID)
		_ = db.WriteAudit(deps.DB, msg.From.ID, "user_unbind", "user", itoa(int(u.TelegramID)), "解绑(安全码校验)")
		r.cmdAccountUnbindConfirmed(ctx, msg)
	}
}

// cmdAccountUnbindConfirmed 真正执行解绑：只断开本地关联，不删除 Jellyfin 账号。
// 远程账号仍由用户自己保留，之后可随时重新绑定或继续在客户端使用。
func (r *Router) cmdAccountUnbindConfirmed(ctx context.Context, msg *Message) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		return
	}
	if u.JellyfinUserID == "" {
		return
	}
	if err := deps.DB.Model(u).Updates(map[string]any{
		"jellyfin_user_id":  "",
		"jellyfin_username": "",
		"bind_type":         "",
	}).Error; err != nil {
		sendText(ctx, deps, msg.ChatID, "解除绑定失败，请稍后再试。")
		return
	}
	sendText(ctx, deps, msg.ChatID, "✅ 已解除绑定。Jellyfin 账号未被删除，之后可重新绑定。")
}

// cmdAccountDelete 删除账号（需 CONFIRM）。
func (r *Router) cmdAccountDelete(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败。")
		return
	}
	if len(args) == 0 || args[0] != "CONFIRM" {
		sendText(ctx, deps, msg.ChatID,
			"删除账号是不可逆操作。请发送 <code>/account delete CONFIRM</code>")
		return
	}
	if u.JellyfinUserID != "" {
		if deps.JF == nil {
			sendText(ctx, deps, msg.ChatID, "Jellyfin 服务未配置，无法删除远程账号；已取消注销。")
			return
		}
		if err := deps.JF.DeleteUser(ctx, u.JellyfinUserID); err != nil {
			sendText(ctx, deps, msg.ChatID, "删除 Jellyfin 用户失败，请联系管理员。")
			return
		}
	}
	// 本地档案同步注销：解绑 + 清空订阅状态（过期时间/永久标记/绑定类型），
	// 避免 Jellyfin 已删除但本地还显示“有效订阅”。
	if err := deps.DB.Model(u).Updates(map[string]any{
		"status":            db.UserStatusDeleted,
		"jellyfin_user_id":  "",
		"jellyfin_username": "",
		"expire_at":         nil,
		"is_permanent":      false,
		"bind_type":         "",
	}).Error; err != nil {
		sendText(ctx, deps, msg.ChatID, "删除本地账号失败，请联系管理员。")
		return
	}
	sendText(ctx, deps, msg.ChatID, "✅ 账号已注销，感谢使用。")
}

// ---------------------------------------------------------------------------
// 登录设备（自助清理在线会话）
// ---------------------------------------------------------------------------
//
// Jellyfin 的 Policy.MaxActiveSessions 一旦占满，用户即使密码完全正确也会被拒登
// （403）。用户自己在客户端上未必找得到"退出其它设备"的入口，所以这里给一个
// 自助清理的兜底——这是用户从上限中恢复的唯一手段。

// accountDevicesContext 取出渲染设备面板所需的一切；不可用时已向用户解释原因。
// 返回 ok=false 表示调用方应直接返回。
func accountDevicesContext(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery) (u *db.User, sessions []jellyfin.UserSession, maxSessions int, ok bool) {
	u, err := ensureUser(ctx, deps, cq.From)
	if err != nil {
		sendText(ctx, deps, cq.ChatID, "查询失败，请稍后再试。")
		return nil, nil, 0, false
	}
	if u.JellyfinUserID == "" {
		sendText(ctx, deps, cq.ChatID, "尚未绑定 Jellyfin 账号，无法查看登录设备。")
		return nil, nil, 0, false
	}
	if deps.JF == nil {
		sendText(ctx, deps, cq.ChatID, "Jellyfin 服务暂不可用，请稍后再试。")
		return nil, nil, 0, false
	}
	sessions, err = deps.JF.ListUserSessions(ctx, u.JellyfinUserID)
	if err != nil {
		sendText(ctx, deps, cq.ChatID, "读取登录设备失败，请稍后再试。")
		return nil, nil, 0, false
	}
	// 上限读不到不算致命：列表本身仍有价值，按"不限"渲染即可。
	maxSessions, _ = deps.JF.MaxActiveSessions(ctx, u.JellyfinUserID)
	return u, sessions, maxSessions, true
}

// devicesText 渲染设备列表正文。
func devicesText(sessions []jellyfin.UserSession, maxSessions int) string {
	limit := "不限"
	if maxSessions > 0 {
		limit = fmt.Sprintf("%d 台", maxSessions)
	}
	b := &strings.Builder{}
	fmt.Fprintf(b, "📱 <b>登录设备</b>\n\n同时在线上限：%s\n当前在线：%d 台\n", limit, len(sessions))
	if len(sessions) == 0 {
		b.WriteString("\n当前没有在线设备。")
		return b.String()
	}
	b.WriteString("\n")
	for i, s := range sessions {
		name := strings.TrimSpace(s.DeviceName)
		if name == "" {
			name = "未知设备"
		}
		line := escapeHTML(name)
		if c := strings.TrimSpace(s.Client); c != "" {
			line += " · " + escapeHTML(c)
		}
		fmt.Fprintf(b, "%d. %s\n", i+1, line)
		if !s.LastActivity.IsZero() {
			fmt.Fprintf(b, "   最后活动：%s\n", s.LastActivity.In(db.ChinaLoc).Format("01-02 15:04"))
		}
	}
	if maxSessions > 0 && len(sessions) >= maxSessions {
		b.WriteString("\n⚠️ 已达在线上限，新设备将无法登录（会提示密码错误之类的报错，其实是名额满了）。")
	}
	return b.String()
}

// cmdAccountDevices 展示当前在线设备列表，并可逐个选择清理。
func cmdAccountDevices(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery) {
	_, sessions, maxSessions, ok := accountDevicesContext(ctx, deps, cq)
	if !ok {
		return
	}
	rows := [][]KeyboardButton{}
	if len(sessions) > 0 {
		// 每个设备一行「清理」按钮，让用户选择具体踢哪一台，而不是只能一键清理全部。
		for i := range sessions {
			idx := i + 1
			rows = append(rows, []KeyboardButton{
				{Text: "🧹 清理设备 " + itoa(idx), Data: BuildCallbackData(DKAccount, "devices", "clear", itoa(idx))},
			})
		}
		// 保留“全部清理”作为可选快捷方式，且仍需二次确认。
		rows = append(rows, []KeyboardButton{
			{Text: "🧹 清理全部设备", Data: BuildCallbackData(DKAccount, "devices", "clear")},
		})
	}
	rows = append(rows,
		[]KeyboardButton{
			{Text: "🔄 刷新", Data: BuildCallbackData(DKAccount, "devices")},
			{Text: "↩️ 返回账号管理", Data: BuildCallbackData(DKAccount, "view")},
		},
	)
	sendPanel(ctx, deps, cq.ChatID, messageIDOf(cq), devicesText(sessions, maxSessions), rows)
}

// deviceDisplayName 返回设备展示名（无名称时用未知设备）。
func deviceDisplayName(s jellyfin.UserSession) string {
	name := strings.TrimSpace(s.DeviceName)
	if name == "" {
		name = "未知设备"
	}
	return escapeHTML(name)
}

// cmdAccountDevicesConfirm 二次确认。idx>0 表示清理指定设备，idx=0 表示清理全部。
// 清理是可恢复操作（重新登录即可），所以不像解绑/注销那样要安全码，但仍要一次显式确认。
func cmdAccountDevicesConfirm(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, idx int) {
	_, sessions, maxSessions, ok := accountDevicesContext(ctx, deps, cq)
	if !ok {
		return
	}
	if len(sessions) == 0 {
		cmdAccountDevices(ctx, deps, cq)
		return
	}
	var text string
	if idx > 0 {
		if idx > len(sessions) {
			cmdAccountDevices(ctx, deps, cq)
			return
		}
		s := sessions[idx-1]
		line := fmt.Sprintf("设备：<b>%s</b>", deviceDisplayName(s))
		if c := strings.TrimSpace(s.Client); c != "" {
			line += "\n客户端：" + escapeHTML(c)
		}
		if !s.LastActivity.IsZero() {
			line += "\n最后活动：" + s.LastActivity.In(db.ChinaLoc).Format("01-02 15:04")
		}
		text = "📱 <b>清理指定设备</b>\n\n" + line +
			"\n\n❓ 确认清理该设备吗？\n清理后这台设备需要重新登录（正在播放的会中断）。"
	} else {
		text = devicesText(sessions, maxSessions) +
			fmt.Sprintf("\n\n❓ 确认要清理全部 <b>%d</b> 台设备吗？\n清理后这些设备都需要重新登录（正在播放的会中断）。", len(sessions))
	}
	rows := [][]KeyboardButton{
		{
			{Text: "✅ 确认清理", Data: BuildCallbackData(DKAccount, "devices", "clearok", itoa(idx))},
			{Text: "❌ 取消", Data: BuildCallbackData(DKAccount, "devices")},
		},
	}
	sendPanel(ctx, deps, cq.ChatID, messageIDOf(cq), text, rows)
}

// cmdAccountDevicesClear 真正执行清理，然后回到设备列表。idx>0 清理指定设备，idx=0 清理全部。
func cmdAccountDevicesClear(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, idx int) {
	u, err := ensureUser(ctx, deps, cq.From)
	if err != nil {
		sendText(ctx, deps, cq.ChatID, "查询失败，请稍后再试。")
		return
	}
	if u.JellyfinUserID == "" || deps.JF == nil {
		sendText(ctx, deps, cq.ChatID, "当前无法清理登录设备，请稍后再试。")
		return
	}
	if idx > 0 {
		sessions, err := deps.JF.ListUserSessions(ctx, u.JellyfinUserID)
		if err != nil {
			sendText(ctx, deps, cq.ChatID, "读取登录设备失败，请稍后再试。")
			return
		}
		if idx > len(sessions) {
			sendText(ctx, deps, cq.ChatID, "未找到该设备，请刷新后重试。")
			return
		}
		s := sessions[idx-1]
		if err := deps.JF.DeleteDevice(ctx, s.DeviceID); err != nil {
			sendText(ctx, deps, cq.ChatID, "清理该设备失败："+err.Error())
			return
		}
		name := strings.TrimSpace(s.DeviceName)
		if name == "" {
			name = "未知设备"
		}
		_ = db.WriteAudit(deps.DB, cq.From.ID, "user_logout_devices", "user",
			itoa(int(u.TelegramID)), fmt.Sprintf("自助清理登录设备：%s", name))
		sendText(ctx, deps, cq.ChatID, fmt.Sprintf("✅ 已清理设备「%s」，现在可以重新登录了。", name))
		cmdAccountDevices(ctx, deps, cq)
		return
	}
	n, err := deps.JF.LogoutAllDevices(ctx, u.JellyfinUserID)
	if err != nil {
		sendText(ctx, deps, cq.ChatID, "清理登录设备失败："+err.Error())
		return
	}
	_ = db.WriteAudit(deps.DB, cq.From.ID, "user_logout_devices", "user",
		itoa(int(u.TelegramID)), fmt.Sprintf("自助清理登录设备 %d 台", n))
	if n == 0 {
		sendText(ctx, deps, cq.ChatID, "当前没有可清理的在线设备。")
	} else {
		sendText(ctx, deps, cq.ChatID, fmt.Sprintf("✅ 已清理 %d 台登录设备，现在可以重新登录了。", n))
	}
	// 刷新面板，让用户直接看到结果。
	cmdAccountDevices(ctx, deps, cq)
}
