package bot

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"mora_bot/internal/db"
)

// ---------------------------------------------------------------------------
// 追剧工单：创建、管理员通知、文本解析
// ---------------------------------------------------------------------------

var (
	reURL     = regexp.MustCompile(`https?://[^\s]+`)
	reBracket = regexp.MustCompile(`《([^》]+)》`)
)

// extractURL 从文本中提取第一个 http(s) 链接（去掉尾部标点）。
func extractURL(s string) string {
	m := reURL.FindString(s)
	if m == "" {
		return ""
	}
	m = strings.TrimRight(m, "，。、；;！!？?）)】》\"'）).，,，")
	// Telegram 的 URL 按钮不接受引号/尖括号/反引号/控制字符，
	// 带这种链接的按钮会让整条管理员通知发送失败（工单静默丢失）。
	// 在唯一入口处截断，保证后续按钮 URL 恒为 Telegram 可接受的形式。
	if i := strings.IndexFunc(m, func(r rune) bool {
		return r < 0x21 || r == 0x7f || r == '"' || r == '<' || r == '>' || r == '`'
	}); i >= 0 {
		m = m[:i]
	}
	return m
}

// removeURL 去掉文本中的全部链接。
func removeURL(s string) string {
	return strings.TrimSpace(reURL.ReplaceAllString(s, " "))
}

// extractTitleFromBrackets 从《…》提取剧名（红果分享文案通常含）。
func extractTitleFromBrackets(s string) string {
	m := reBracket.FindStringSubmatch(s)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// parseTitleActor 解析「剧名 主演名」：支持
//   - "剧名 / 主演"
//   - "剧名、主演" / "剧名，主演"
//   - "剧名 主演 演员名"（主演关键词）
//   - 仅剧名（无主演）
func parseTitleActor(s string) (title, actor string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	// 主演关键词
	for _, kw := range []string{"主演", "演员"} {
		if idx := strings.Index(s, kw); idx > 0 {
			title = strings.TrimSpace(s[:idx])
			actor = strings.TrimSpace(strings.TrimSpace(s[idx+len(kw):]))
			actor = strings.TrimLeft(actor, "：: ")
			return titleStripped(title), actor
		}
	}
	// 分隔符：/ 、 ，
	for _, sep := range []string{"/", "、", "，", ","} {
		if idx := strings.Index(s, sep); idx > 0 {
			title = strings.TrimSpace(s[:idx])
			actor = strings.TrimSpace(s[idx+len(sep):])
			return titleStripped(title), actor
		}
	}
	return titleStripped(s), ""
}

// titleStripped 去掉标题里的常见前缀杂质。
func titleStripped(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "·*#️⃣☞→▪️")
	return strings.TrimSpace(s)
}

// createDramaTicket 创建求剧工单并通知管理员。
// 返回创建成功的工单；ok=false 表示失败或已超限（已发提示）。
func createDramaTicket(ctx context.Context, deps *HandlerDeps, msg *Message, title, link, actor string) (*db.DramaRequest, bool) {
	if deps == nil || deps.DB == nil {
		return nil, false
	}
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		return nil, false
	}
	if !dramaDailyLimitOK(ctx, deps, u.TelegramID) {
		return nil, false
	}
	t := strings.TrimSpace(title)
	if t == "" {
		t = "（未提供剧名）"
	}
	if len(t) > 200 {
		t = t[:200]
	}
	if len(link) > 512 {
		link = link[:512]
	}
	if len(actor) > 200 {
		actor = actor[:200]
	}
	// 主演并入备注（保留用户原始追问内容）
	note := actor

	req := db.DramaRequest{
		UserID:     u.TelegramID,
		TgUsername: msg.From.Username,
		Title:      t,
		Link:       link,
		Note:       note,
		Status:     db.DramaStatusPending,
	}
	if err := deps.DB.Create(&req).Error; err != nil {
		sendText(ctx, deps, msg.ChatID, "创建工单失败，请稍后再试。")
		return nil, false
	}
	deps.DB.Create(&db.DramaRequestLog{RequestID: req.ID, Action: "create", Detail: msg.From.Username})
	_ = db.WriteAudit(deps.DB, u.TelegramID, "drama_create", "drama_request", itoa(int(req.ID)), "提交求剧工单")
	// 通知管理员
	notifyAdminsDrama(ctx, deps, &req, u)
	return &req, true
}

// notifyAdminsDrama 新工单私聊推送所有管理员（含链接与操作按钮）。
func notifyAdminsDrama(ctx context.Context, deps *HandlerDeps, req *db.DramaRequest, u *db.User) {
	if deps == nil || deps.Snd == nil || req == nil {
		return
	}
	for _, id := range deps.SuperAdminIDs {
		// 管理员本人发的工单也提醒，方便测试与跟踪
		_ = deps.Snd.SendKeyboard(ctx, id, dramaAdminCard(req, u, ""), dramaAdminRows(req, true))
	}
}

// dramaAdminCard 管理员侧工单卡片文本（HTML）；footer 非空时追加状态行。
func dramaAdminCard(req *db.DramaRequest, u *db.User, footer string) string {
	s := fmt.Sprintf(
		"🎬 <b>求剧工单 #%d</b> · %s\n"+
			"👤 %s\n"+
			"📺 剧名：%s\n"+
			"🧑 主演：%s\n"+
			"🕐 提交于 %s",
		req.ID,
		dramaStatusText(req.Status),
		displayTg(u),
		escapeHTML(req.Title),
		escapeHTML(emptyDash(req.Note)),
		req.CreatedAt.In(db.ChinaLoc).Format("01-02 15:04"),
	)
	if footer != "" {
		s += "\n\n" + footer
	}
	return s
}

// dramaAdminRows 工单卡片键盘：withClaim 为 true 时含「接单」按钮；
// false 时仅保留链接（已结单状态）。
func dramaAdminRows(req *db.DramaRequest, withClaim bool) [][]KeyboardButton {
	rows := [][]KeyboardButton{}
	if req.Link != "" {
		rows = append(rows, []KeyboardButton{{Text: "🔗 打开分享链接", URL: req.Link}})
	}
	if withClaim {
		rows = append(rows, []KeyboardButton{
			{Text: "🙋 接单", Data: BuildCallbackData(DKDrama, "claim", itoa(int(req.ID)))},
			{Text: "✅ 完成", Data: BuildCallbackData(DKDrama, "resolve", itoa(int(req.ID)))},
			{Text: "❌ 驳回", Data: BuildCallbackData(DKDrama, "reject", itoa(int(req.ID)))},
		})
	}
	return rows
}

// dramaStatusText 工单状态中文名。
func dramaStatusText(s string) string {
	switch s {
	case db.DramaStatusPending:
		return "🕐 待处理"
	case db.DramaStatusClaimed:
		return "🧑‍🔧 处理中"
	case db.DramaStatusCompleted:
		return "✅ 已完成"
	case db.DramaStatusRejected:
		return "❌ 已驳回"
	case db.DramaStatusNeedInfo:
		return "❓ 待补充"
	case db.DramaStatusCancelled:
		return "🚫 已取消"
	}
	return s
}

// dramaOwner 加载工单提交人（用于卡片展示，查不到返回 nil）。
func dramaOwner(deps *HandlerDeps, req *db.DramaRequest) *db.User {
	var u db.User
	if err := deps.DB.Where("telegram_id = ?", req.UserID).First(&u).Error; err != nil {
		return nil
	}
	return &u
}

// tgDisplayName TGUser 展示名（@username 或姓名，兜底 tg_id）。
func tgDisplayName(u TGUser) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	n := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
	if n == "" {
		return fmt.Sprintf("tg_%d", u.ID)
	}
	return n
}

// 接单/结单结果码。
const (
	claimOK = iota
	claimAlreadyMine
	claimTaken    // 已被他人接单
	claimSettled  // 已处理（完成/驳回/取消）
	claimFailed   // 查询或更新失败
	settleOK      = claimOK
	settleTaken   = claimTaken
	settleAlready = claimSettled
	settleFailed  = claimFailed
)

// claimDramaRequest 管理员接单：pending → claimed。
// 已被他人接单或已处理时返回对应结果码，防止重复接单。
func claimDramaRequest(ctx context.Context, deps *HandlerDeps, reqID uint, admin TGUser) (db.DramaRequest, int) {
	var req db.DramaRequest
	if deps == nil || deps.DB == nil {
		return req, claimFailed
	}
	if err := deps.DB.First(&req, reqID).Error; err != nil {
		return req, claimFailed
	}
	switch req.Status {
	case db.DramaStatusCompleted, db.DramaStatusRejected, db.DramaStatusCancelled:
		return req, claimSettled
	case db.DramaStatusClaimed:
		if req.ClaimedBy != nil && *req.ClaimedBy == admin.ID {
			return req, claimAlreadyMine
		}
		return req, claimTaken
	}
	name := tgDisplayName(admin)
	res := deps.DB.Model(&db.DramaRequest{}).
		Where("id = ? AND status = ?", req.ID, db.DramaStatusPending).
		Updates(map[string]any{
			"status":           db.DramaStatusClaimed,
			"claimed_by":       admin.ID,
			"claimed_by_name":  name,
		})
	if res.Error != nil {
		return req, claimFailed
	}
	if res.RowsAffected == 0 {
		_ = deps.DB.First(&req, reqID)
		return req, claimTaken
	}
	deps.DB.Create(&db.DramaRequestLog{RequestID: req.ID, Action: "claim", Detail: name, OperatorID: admin.ID})
	_ = db.WriteAudit(deps.DB, admin.ID, "drama_claim", "drama_request", itoa(int(req.ID)), "管理员接单")
	req.Status = db.DramaStatusClaimed
	req.ClaimedBy = &admin.ID
	req.ClaimedByName = name
	return req, claimOK
}

// dramaSettleRequest 管理员处理工单：标记完成或驳回（管理员门禁在调用方）。
// status 应为 db.DramaStatusCompleted / db.DramaStatusRejected；reason 仅驳回时必填。
// 未接单直接处理视为自动接单（记录处理人）。已被他人接单/已处理时拒绝。
func dramaSettleRequest(ctx context.Context, deps *HandlerDeps, reqID uint, status string, admin TGUser, reason string) (db.DramaRequest, int) {
	var req db.DramaRequest
	if deps == nil || deps.DB == nil {
		return req, settleFailed
	}
	if err := deps.DB.First(&req, reqID).Error; err != nil {
		return req, settleFailed
	}
	switch req.Status {
	case db.DramaStatusCompleted, db.DramaStatusRejected, db.DramaStatusCancelled:
		return req, settleAlready
	case db.DramaStatusClaimed:
		if req.ClaimedBy == nil || *req.ClaimedBy != admin.ID {
			return req, settleTaken
		}
	}
	reason = strings.TrimSpace(reason)
	if r := []rune(reason); len(r) > 80 {
		reason = string(r[:80])
	}
	name := tgDisplayName(admin)
	updates := map[string]any{
		"status":          status,
		"resolved_at":     time.Now(),
		"claimed_by":      admin.ID,
		"claimed_by_name": name,
	}
	if status == db.DramaStatusRejected {
		updates["reject_reason"] = reason
	}
	res := deps.DB.Model(&db.DramaRequest{}).
		Where("id = ? AND status = ?", req.ID, req.Status).
		Updates(updates)
	if res.Error != nil {
		return req, settleFailed
	}
	if res.RowsAffected == 0 {
		// 并发下状态已被他人改变，重读后归类
		_ = deps.DB.First(&req, reqID)
		if req.Status == db.DramaStatusClaimed && req.ClaimedBy != nil && *req.ClaimedBy != admin.ID {
			return req, settleTaken
		}
		return req, settleAlready
	}
	action := "complete"
	detail := fmt.Sprintf("by_admin:%s", name)
	if status == db.DramaStatusRejected {
		action = "reject"
		detail = fmt.Sprintf("by_admin:%s reason:%s", name, reason)
	}
	deps.DB.Create(&db.DramaRequestLog{RequestID: req.ID, Action: action, Detail: detail, OperatorID: admin.ID})
	_ = db.WriteAudit(deps.DB, admin.ID, "drama_"+action, "drama_request", itoa(int(req.ID)), "管理员处理求剧工单")
	req.Status = status
	req.ClaimedBy = &admin.ID
	req.ClaimedByName = name
	req.RejectReason = reason
	// 通知提交人（驳回附理由）
	if req.UserID != admin.ID {
		if status == db.DramaStatusRejected {
			sendText(ctx, deps, req.UserID, fmt.Sprintf(
				"❌ 你的求剧工单 #%d《%s》已被驳回。\n🚫 驳回理由：%s\n\n如有疑问可联系管理员，或修改后重新提交。",
				req.ID, req.Title, reason))
		} else {
			sendText(ctx, deps, req.UserID, fmt.Sprintf(
				"✅ 你的求剧工单 #%d《%s》已完成，感谢支持！\n如需再求其他剧目，欢迎继续提交。",
				req.ID, req.Title))
		}
	}
	return req, settleOK
}

// editAdminDramaCard 原地更新管理员通知消息：重建卡片文本与键盘。
func editAdminDramaCard(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, req db.DramaRequest, footer string) {
	if cq == nil || cq.MessageID == 0 {
		return
	}
	sendPanel(ctx, deps, cq.ChatID, cq.MessageID,
		dramaAdminCard(&req, dramaOwner(deps, &req), footer),
		dramaAdminRows(&req, req.Status == db.DramaStatusPending || req.Status == db.DramaStatusClaimed))
}

// handleAdminDramaRejectStep 驳回理由收集：收到理由文本后执行驳回并通知用户。
func (r *Router) handleAdminDramaRejectStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if deps == nil || deps.IsSuper == nil || !deps.IsSuper(msg.From.ID) {
		if deps != nil {
			deps.Sessions.Clear(msg.From.ID)
		}
		return
	}
	if strings.EqualFold(strings.TrimSpace(msg.Text), "/cancel") {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消驳回。")
		return
	}
	reason := strings.TrimSpace(msg.Text)
	if reason == "" {
		sendText(ctx, deps, msg.ChatID, "理由不能为空，请直接回复驳回理由（回复 /cancel 可取消）。")
		return
	}
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		return
	}
	reqID, _ := sess.Data["req_id"].(int64)
	msgID, _ := sess.Data["msg_id"].(int)
	deps.Sessions.Clear(msg.From.ID)
	if reqID <= 0 {
		sendText(ctx, deps, msg.ChatID, "会话数据异常，请重新从工单卡片发起驳回。")
		return
	}
	req, res := dramaSettleRequest(ctx, deps, uint(reqID), db.DramaStatusRejected, msg.From, reason)
	switch res {
	case settleOK:
		sendText(ctx, deps, msg.ChatID, fmt.Sprintf("已驳回工单 #%d，理由已私信通知提交用户。", reqID))
		editAdminDramaCard(ctx, deps,
			&CallbackQuery{From: msg.From, ChatID: msg.ChatID, MessageID: msgID},
			req,
			fmt.Sprintf("❌ 已驳回（%s）：", escapeHTML(req.ClaimedByName))+escapeHTML(req.RejectReason))
	case settleAlready:
		sendText(ctx, deps, msg.ChatID, "该工单已处理，无需重复操作。")
	case settleTaken:
		sendText(ctx, deps, msg.ChatID, "该工单已被 "+req.ClaimedByName+" 接单，无法重复处理。")
	default:
		sendText(ctx, deps, msg.ChatID, "操作失败，请稍后再试。")
	}
}

// displayTg 用户展示名。
func displayTg(u *db.User) string {
	if u == nil {
		return "未知用户"
	}
	n := u.DisplayName()
	if n == "" {
		n = fmt.Sprintf("tg_%d", u.TelegramID)
	}
	if u.TgUsername != "" {
		return fmt.Sprintf("@%s（%s）", escapeHTML(u.TgUsername), escapeHTML(n))
	}
	return fmt.Sprintf("<code>%d</code>（%s）", u.TelegramID, escapeHTML(n))
}

// emptyDash 空值显示为“—”。
func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
