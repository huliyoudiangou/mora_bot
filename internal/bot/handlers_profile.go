package bot

import (
	"context"
	"fmt"
	"time"
)

// cmdProfile /profile: 本地账号 + 订阅状态展示。
func (r *Router) cmdProfile(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		return
	}
	b := fmt.Sprintf("👤 <b>%s</b>\n果果币：%d 枚\n", escapeHTML(u.DisplayName()), u.GuoGuo)
	if u.JellyfinUsername != "" {
		b += fmt.Sprintf("Jellyfin 账号：%s\n", escapeHTML(u.JellyfinUsername))
	}
	now := time.Now()
	switch {
	case u.IsPermanent:
		b += "订阅：永久 ✅\n"
	case u.ExpireAt == nil:
		b += "订阅：尚未开通\n"
	case u.ExpireAt.After(now):
		b += fmt.Sprintf("订阅到期：%s（剩余 %d 天）\n",
			u.ExpireAt.Format("2006-01-02"),
			int(u.ExpireAt.Sub(now).Hours()/24)+1)
	default:
		b += "订阅已到期，请使用 /shop 续期。\n"
	}
	b += fmt.Sprintf("连续签到：%d 天　账号状态：%s\n", u.SignStreak, u.Status)
	sendHTML(ctx, deps, msg.ChatID, b)
}
