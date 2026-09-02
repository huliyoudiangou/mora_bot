package bot

import (
	"context"

	"mora_bot/internal/db"
)

// getLocal 取本地用户，无副作用。
func getLocal(ctx context.Context, deps *HandlerDeps, from TGUser) (*db.User, error) {
	return ensureUser(ctx, deps, from)
}

// ensureUser 找到用户，若无则 GetOrCreateUser；最后同步活跃时间。
func ensureUser(_ context.Context, deps *HandlerDeps, from TGUser) (*db.User, error) {
	if deps.DB == nil {
		return nil, db.ErrNilDB
	}
	u, err := db.GetOrCreateUser(deps.DB, from.ID, from.Username, from.FirstName, from.LastName)
	if err != nil {
		return nil, err
	}
	db.TouchUserActive(deps.DB, u.TelegramID)
	return u, nil
}

// continueSession 若用户在活跃会话中，将本次文本视作下一步输入推进会话。
// 返回是否已消费这条消息。
func (r *Router) continueSession(ctx context.Context, msg *Message) bool {
	sess := r.deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		return false
	}
	switch sess.Kind {
	case sessRegPassword:
		r.handleRegStepResetPw(ctx, msg)
		return true
	case sessRegInvite:
		r.handleRegStepInvite(ctx, msg)
		return true
	case sessRegUsername:
		r.handleRegStepPassword(ctx, msg)
		return true
	case sessRegSecurity:
		r.handleRegStepSecurity(ctx, msg)
		return true
	case sessBindExistU:
		r.handleBindExistUser(ctx, msg)
		return true
	case sessBindExistPw:
		r.handleBindExistPw(ctx, msg)
		return true
	case sessRedeem:
		r.handleRedeemStep(ctx, msg)
		return true
	case sessDramaFeedback:
		r.handleDramaStepFeedback(ctx, msg)
		return true
	case sessDramaInfo:
		r.handleDramaStepInfo(ctx, msg)
		return true
	case sessAdminGenInvite:
		r.handleAdminGenInviteStep(ctx, msg)
		return true
	case sessAdminGenRenew:
		r.handleAdminGenRenewStep(ctx, msg)
		return true
	case sessAdminAdjPoints:
		r.handleAdminAdjPointsStep(ctx, msg)
		return true
	case sessAdminQueryUser:
		r.handleAdminQueryUserStep(ctx, msg)
		return true
	case sessAdminQueryCode:
		r.handleAdminQueryCodeStep(ctx, msg)
		return true
	case sessAdminWL:
		r.handleAdminWLStep(ctx, msg)
		return true
	case sessAdminPrice:
		r.handleAdminPriceStep(ctx, msg)
		return true
	case sessAdminLineAdd:
		r.handleAdminLineAddStep(ctx, msg)
		return true
	case sessAdminLineDel:
		r.handleAdminLineDelStep(ctx, msg)
		return true
	case sessAdminQuota:
		r.handleAdminQuotaStep(ctx, msg)
		return true
	case sessAdminRegQuota:
		r.handleAdminRegQuotaStep(ctx, msg)
		return true
	case sessAdminDramaRej:
		r.handleAdminDramaRejectStep(ctx, msg)
		return true
	case sessSetSecurity:
		r.handleSetSecurityStep(ctx, msg)
		return true
	case sessPwdChange:
		r.handlePwdChangeStep(ctx, msg)
		return true
	case sessUnbind:
		r.handleUnbindStep(ctx, msg)
		return true
	}
	return false
}

// sendText 统一发送文本。
func sendText(ctx context.Context, deps *HandlerDeps, chatID int64, text string) {
	if deps.Snd == nil {
		return
	}
	_ = deps.Snd.SendText(ctx, chatID, text)
}

// sendHTML 发送 HTML 消息。
func sendHTML(ctx context.Context, deps *HandlerDeps, chatID int64, html string) {
	if deps.Snd == nil {
		return
	}
	_ = deps.Snd.SendTextHTML(ctx, chatID, html)
}

// helpText 帮助文本（面板按钮为主）。
var helpText = `
📖 果果屋使用说明

所有功能都能通过主面板按钮完成：发 /start 或 /menu 打开主面板，点下方按钮即可操作。

📌 常用功能：
✅ 每日签到 — 领取果果币
👤 我的账号 — 查看订阅与果果币
🛒 果果币商店 — 购买续期码/邀请码
🎟 使用续期码 — 输入续期码为自己的账号续期
🎬 追剧中心 — 提交求剧工单（红果短剧分享链接）
🔗 绑定已有账号 — 把已有 Jellyfin 账号关联到本 bot（用户名+密码）
📝 注册新账号 — 开通新 Jellyfin 账号（需邀请码；开注且有名额时免邀请码）
⚙️ 账号管理 — 改密/解绑（需安全码）/注销/登录设备
🔐 安全码 — 注册时设置；修改密码、解绑需校验
🛠 管理面板 — 管理员统计与发卡（仅 /admin） 
`

// ensureAdmin 判定管理员（tg_id 必须与 env 配置的 SUPER_ADMIN_TG_IDS/ADMIN_TELEGRAM_IDS 一致）。
func (r *Router) ensureAdmin(ctx context.Context, msg *Message) bool {
	if !r.deps.IsSuper(msg.From.ID) {
		sendText(ctx, r.deps, msg.ChatID, "您不是管理员，无法使用。")
		return false
	}
	return true
}
