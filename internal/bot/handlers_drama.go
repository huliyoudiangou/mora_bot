package bot

import (
	"context"
	"fmt"
	"time"

	"mora_bot/internal/db"
)

// cmdDrama /drama：追剧中心面板。
func (r *Router) cmdDrama(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		return
	}
	if len(args) == 0 {
		// 面板按钮入口
		text, rows := dramaPanel(deps, u)
		sendPanel(ctx, deps, msg.ChatID, 0, text, rows)
		return
	}
	// 带参直接创建（兼容命令式）：/drama 剧名 主演 或 /drama 链接
	raw := joinArgs(args)
	link := extractURL(raw)
	title, actor := parseTitleActor(removeURL(raw))
	if bt := extractTitleFromBrackets(raw); bt != "" {
		title = bt
	}
	if title == "" && link == "" {
		sendText(ctx, deps, msg.ChatID, "未识别到内容，请发送：红果短剧分享链接 或 剧名/主演名。")
		return
	}
	req, ok := createDramaTicket(ctx, deps, msg, title, link, actor)
	if !ok {
		return
	}
	sendText(ctx, deps, msg.ChatID, fmt.Sprintf("✅ 求剧工单 #%d 已创建：%s", req.ID, req.Title))
}

// joinArgs 全部参数合并为一个字符串。
func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// cmdListDrama 列出我的求剧记录（面板按钮触发）。
func (r *Router) cmdListDrama(ctx context.Context, msg *Message) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败。")
		return
	}
	var list []db.DramaRequest
	if err := deps.DB.Where("user_id = ?", u.TelegramID).Order("id desc").Limit(10).Find(&list).Error; err != nil {
		sendText(ctx, deps, msg.ChatID, "查询工单失败。")
		return
	}
	if len(list) == 0 {
		sendText(ctx, deps, msg.ChatID, "你还没有求剧记录。点「我要求剧」，发送红果短剧分享链接（或剧名+主演名）创建第一条吧。")
		return
	}
	text := "🎬 我的求剧记录（最近 10 条）\n\n"
	for _, it := range list {
		text += fmt.Sprintf("《%s》%s · %s\n", it.Title, dramaStatusText(it.Status), it.CreatedAt.Format("01-02"))
		if it.Status == db.DramaStatusRejected && it.RejectReason != "" {
			text += "　└ 驳回理由：" + it.RejectReason + "\n"
		}
	}
	text += "\n↩️ 返回面板可继续操作。"
	sendText(ctx, deps, msg.ChatID, text)
}

// dramaDailyLimitOK 检查用户今天是否已达每日求剧上限。达到时发提示并返回 false。
func dramaDailyLimitOK(ctx context.Context, deps *HandlerDeps, userID int64) bool {
	if deps == nil || deps.DB == nil || deps.DramaDailyLimit <= 0 {
		return true
	}
	from := time.Now().In(db.ChinaLoc).Format("2006-01-02 00:00:00")
	var cnt int64
	deps.DB.Model(&db.DramaRequest{}).
		Where("user_id = ? AND created_at >= ?", userID, from).
		Count(&cnt)
	if cnt >= int64(deps.DramaDailyLimit) {
		sendText(ctx, deps, userID, fmt.Sprintf("今天求剧已达上限（%d 条/天），明天再来吧。", deps.DramaDailyLimit))
		return false
	}
	return true
}
