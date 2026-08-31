package bot

import (
	"context"
	"time"

	"mora_bot/internal/db"
)

// cmdHelp /help：输出帮助。
func (r *Router) cmdHelp(ctx context.Context, msg *Message) {
	sendText(ctx, r.deps, msg.ChatID, helpText)
}

// markUsedInvite 一旦绑定成功把邀请码标记为已使用。
func markUsedInvite(deps *HandlerDeps, inviteID uint, userID int64) {
	if deps == nil || deps.DB == nil || inviteID == 0 {
		return
	}
	usedBy := userID
	deps.DB.Model(&db.InviteCode{}).
		Where("id = ?", inviteID).
		Updates(map[string]any{"used_by": usedBy, "used_at": time.Now(), "status": db.CodeStatusUsed})
}
