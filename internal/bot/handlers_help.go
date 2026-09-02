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

// claimInviteCode 尝试原子占用一张邀请码（仅当 unused 且未绑定使用者时成功）。
// 返回 false 表示已被并发使用/不存在。成功后调用方应在失败路径 releaseInviteCode 归还。
func claimInviteCode(deps *HandlerDeps, inviteID uint, userID int64) bool {
	if deps == nil || deps.DB == nil || inviteID == 0 {
		return false
	}
	now := time.Now()
	res := deps.DB.Model(&db.InviteCode{}).
		Where("id = ? AND status = ? AND used_by IS NULL", inviteID, db.CodeStatusUnused).
		Updates(map[string]any{"used_by": userID, "used_at": now, "status": db.CodeStatusUsed})
	return res.Error == nil && res.RowsAffected == 1
}

// releaseInviteCode 归还一张被临时占用的邀请码（仅当使用者是当前用户且状态为 used 时）。
// 只清理使用标记与状态，不删除卡密。
func releaseInviteCode(deps *HandlerDeps, inviteID uint, userID int64) {
	if deps == nil || deps.DB == nil || inviteID == 0 {
		return
	}
	deps.DB.Model(&db.InviteCode{}).
		Where("id = ? AND used_by = ? AND status = ?", inviteID, userID, db.CodeStatusUsed).
		Updates(map[string]any{"used_by": nil, "used_at": nil, "status": db.CodeStatusUnused})
}
