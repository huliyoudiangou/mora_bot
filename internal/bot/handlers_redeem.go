package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mora_bot/internal/codes"
)

// cmdRedeem /redeem <续期码>：用户核销续期码，为自己的 Jellyfin 账号续期。
func (r *Router) cmdRedeem(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		sendHTML(ctx, deps, msg.ChatID, "用法：<code>/redeem 续期码</code>\n续期码可在 /shop 用果果币购买。")
		return
	}
	code := strings.TrimSpace(strings.Join(args, " "))

	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		return
	}
	if deps.Pepper == "" {
		sendText(ctx, deps, msg.ChatID, "管理员未配置 SECURITY_PEPPER，卡密功能暂不可用。")
		return
	}
	// 被管理员停用的账号不接受续期：续期只改到期时间，解封权在管理员手里，
	// 现在核销只会白白消耗一张续期码（卡密不消耗、留待解封后再用）。
	if accountSuspended(u) {
		sendText(ctx, deps, msg.ChatID, suspendBlockedText)
		return
	}

	days, newExpire, err := redeemRenewalCode(deps, u, code)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, redeemErrText(err))
		return
	}
	// 若此前被到期停用，续期后立即恢复（不必等次日巡检）。
	restoreIfExpired(ctx, deps, u.TelegramID)
	sendHTML(ctx, deps, msg.ChatID, fmt.Sprintf(
		"✅ 续期成功！\n新增 %d 天，当前有效期至 <b>%s</b>",
		days, newExpire.Format("2006-01-02")))
}

// handleRedeemStep 面板「使用续期码」会话：收卡密并核销（复用 /redeem 核心逻辑）。
func (r *Router) handleRedeemStep(ctx context.Context, msg *Message) {
	deps := r.deps
	code := strings.TrimSpace(msg.Text)
	if code == "" {
		sendText(ctx, deps, msg.ChatID, "续期码不能为空，请重新发送。")
		return
	}
	if strings.EqualFold(code, "/cancel") {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消使用续期码。")
		return
	}

	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if deps.Pepper == "" {
		sendText(ctx, deps, msg.ChatID, "管理员未配置 SECURITY_PEPPER，卡密功能暂不可用。")
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	// 被管理员停用的账号不接受续期（见 cmdRedeem 注释）：清会话避免卡在向导里。
	if accountSuspended(u) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, suspendBlockedText)
		return
	}

	days, newExpire, err := redeemRenewalCode(deps, u, code)
	if err != nil {
		// 卡密无效可重试，保留会话；其余清掉避免卡死
		sendText(ctx, deps, msg.ChatID, redeemErrText(err))
		if !errors.Is(err, codes.ErrCodeNotFound) && !errors.Is(err, codes.ErrCodeUsed) {
			deps.Sessions.Clear(msg.From.ID)
		}
		return
	}
	deps.Sessions.Clear(msg.From.ID)
	// 若此前被到期停用，续期后立即恢复（不必等次日巡检）。
	restoreIfExpired(ctx, deps, u.TelegramID)
	sendHTML(ctx, deps, msg.ChatID, fmt.Sprintf(
		"✅ 续期成功！\n新增 %d 天，当前有效期至 <b>%s</b>",
		days, newExpire.Format("2006-01-02")))
}

// redeemErrText 核销失败文案：校验类错误给具体原因，事务内部错误一律脱敏。
func redeemErrText(err error) string {
	switch {
	case errors.Is(err, codes.ErrCodeNotFound):
		return "❌ 卡密不存在或已被使用。"
	case errors.Is(err, codes.ErrCodeUsed):
		return "❌ 该续期码已被使用。"
	case errors.Is(err, errRedeemPermanent):
		return "ℹ️ 你是白名单/永久账号，永久有效，无需核销续期码（卡密未消耗，可留作后用或转给他人）。"
	case errors.Is(err, errRedeemInternal):
		return "❌ 核销失败，请稍后再试；多次失败请联系管理员。"
	default:
		return "❌ " + err.Error()
	}
}
