package bot

import (
	"context"
	"fmt"
	"strings"

	"mora_bot/internal/codes"
	"mora_bot/internal/db"
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
		// /account unbind 提示安全码；/account unbind CONFIRM 直接解绑（保留兼容）
		if len(args) >= 2 && strings.EqualFold(args[1], "CONFIRM") {
			r.cmdAccountUnbindConfirmed(ctx, msg)
		} else {
			r.cmdAccountUnbind(ctx, msg)
		}
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
	return fmt.Sprintf("已绑定 Jellyfin（%s）", u.JellyfinUserID)
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
	if u.SecurityCodeHash == "" {
		sendText(ctx, deps, msg.ChatID, "你还没有设置安全码。请先点「🔐 设置安全码」或发送 /account security 设置后，再修改密码。")
		return
	}
	deps.Sessions.Begin(msg.From.ID, sessPwdChange)
	sendText(ctx, deps, msg.ChatID, "🔑 修改密码 · 第 1/3 步\n请输入你的<b>安全码</b>：\n\n回复 /cancel 可取消。")
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
		sendText(ctx, deps, msg.ChatID, "🔑 第 2/3 步\n请输入你的<b>旧密码</b>：\n\n回复 /cancel 可取消。")
	case 1: // 旧密码
		oldPw := strings.TrimSpace(msg.Text)
		if oldPw == "" {
			sendText(ctx, deps, msg.ChatID, "旧密码不能为空。")
			return
		}
		ok, err := deps.JF.AuthenticateByName(ctx, u.JellyfinUsername, oldPw)
		if err != nil {
			sendText(ctx, deps, msg.ChatID, "连接 Jellyfin 失败，请稍后再试。")
			return
		}
		if !ok {
			sendText(ctx, deps, msg.ChatID, "❌ 旧密码错误，请重新输入。")
			return
		}
		s2 := deps.Sessions.Advance(msg.From.ID, map[string]any{"old_pw": oldPw})
		s2.Step = 2
		sendText(ctx, deps, msg.ChatID, "🔑 第 3/3 步\n请输入你的<b>新密码</b>（至少 6 位）：\n\n回复 /cancel 可取消。")
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
	sendText(ctx, deps, msg.ChatID, "🔐 设置安全码 · 第 1/2 步\n请设置你的<b>安全码</b>（4-20 位字母/数字，用于改密/解绑校验）：\n\n回复 /cancel 可取消。")
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
		sendText(ctx, deps, msg.ChatID, "🔐 第 2/2 步\n请<b>再次输入</b>安全码确认：\n\n回复 /cancel 可取消。")
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
			sendText(ctx, deps, msg.ChatID, "请发送 <b>CONFIRM</b> 确认解绑，或 /cancel 放弃。")
			return
		}
		deps.Sessions.Clear(msg.From.ID)
		_ = db.WriteAudit(deps.DB, msg.From.ID, "user_unbind", "user", itoa(int(u.TelegramID)), "解绑(安全码校验)")
		r.cmdAccountUnbindConfirmed(ctx, msg)
	}
}

// cmdAccountUnbindConfirmed 真正执行解绑。
func (r *Router) cmdAccountUnbindConfirmed(ctx context.Context, msg *Message) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		return
	}
	if u.JellyfinUserID == "" {
		return
	}
	if deps.JF != nil {
		if e := deps.JF.DeleteUser(ctx, u.JellyfinUserID); e != nil {
			sendText(ctx, deps, msg.ChatID, "删除远程账号失败，联系管理员。")
			return
		}
	}
	_ = deps.DB.Model(u).Updates(map[string]any{
		"jellyfin_user_id":  "",
		"jellyfin_username": "",
	}).Error
	sendText(ctx, deps, msg.ChatID, "✅ 已解除绑定。")
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
	if deps.JF != nil && u.JellyfinUserID != "" {
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
