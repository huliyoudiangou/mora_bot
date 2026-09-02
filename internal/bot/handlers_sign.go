package bot

import (
	"context"
	"errors"
	"fmt"

	"mora_bot/internal/db"
)

// cmdSignin /signin：每日固定奖励（5 果果币），无连签加成。
func (r *Router) cmdSignin(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	u, err := ensureUser(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "系统繁忙，请稍后再试。")
		return
	}
	var signed *db.SignInRecord
	deps.Lockers.WithUser(msg.From.ID, func() {
		s, e := db.DoSignIn(deps.DB, u.TelegramID)
		if e != nil {
			sendText(ctx, deps, msg.ChatID, signinErrText(e))
			return
		}
		signed = s
	})
	if signed == nil {
		return
	}
	sendText(ctx, deps, msg.ChatID, fmt.Sprintf(
		"✅ 签到成功！\n奖励 %d 果果币　连续 %d 天",
		signed.Reward, signed.Streak))
}

// signinErrText 常见错误语。
func signinErrText(err error) string {
	switch {
	case errors.Is(err, db.ErrAlreadySignedIn):
		return "今天已经签过了，明天再来吧。"
	case errors.Is(err, db.ErrOptimisticLock):
		return "系统繁忙，请再尝试一次。"
	default:
		return "签到失败，请稍后再试。"
	}
}
