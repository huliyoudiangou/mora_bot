package bot

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"mora_bot/internal/codes"
	"mora_bot/internal/db"
)

// ---------------------------------------------------------------------------
// 1) 调整用户积分（两步：tg_id → delta）
// ---------------------------------------------------------------------------

// handleAdminAdjPointsStep 调整积分向导。
func (r *Router) handleAdminAdjPointsStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消调整积分。")
		return
	}
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if sess.Step == 0 {
		tgID := parseInt64Safe(msg.Text)
		if tgID == 0 {
			sendText(ctx, deps, msg.ChatID, "请输入合法的用户 tg_id（数字）。")
			return
		}
		s2 := deps.Sessions.Advance(msg.From.ID, map[string]any{"tg_id": tgID})
		s2.Step = 1
		sendText(ctx, deps, msg.ChatID, "请输入积分变动值（正数加、负数减，例如 <b>100</b> 或 <b>-50</b>）：")
		return
	}
	// step 1: delta
	delta := parseInt64Safe(msg.Text)
	if delta == 0 {
		sendText(ctx, deps, msg.ChatID, "积分变动不能为 0，请重新输入（如 100 或 -50）。")
		return
	}
	tgID, _ := sess.Data["tg_id"].(int64)
	deps.Sessions.Clear(msg.From.ID)
	var u db.User
	if err := deps.DB.Where("telegram_id = ?", tgID).First(&u).Error; err != nil {
		sendText(ctx, deps, msg.ChatID, "未找到该用户（tg_id="+itoa64s(tgID)+"）。")
		return
	}
	if err := db.AddPoints(deps.DB, u.TelegramID, int(delta), "admin_adjust", "AdminPanel", 0); err != nil {
		sendText(ctx, deps, msg.ChatID, "调整失败："+err.Error())
		return
	}
	_ = db.WriteAudit(deps.DB, msg.From.ID, "admin_addpoints", "user", itoa64s(tgID), fmt.Sprintf("调整果果币 %d", delta))
	sendText(ctx, deps, msg.ChatID, fmt.Sprintf("✅ 已为用户 tg=%d 调整 %d 果果币。", tgID, delta))
}

// ---------------------------------------------------------------------------
// 2) 查询用户信息
// ---------------------------------------------------------------------------

// handleAdminQueryUserStep 查询用户向导：收 tg_id → 展示。
func (r *Router) handleAdminQueryUserStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消查询用户。")
		return
	}
	tgID := parseInt64Safe(msg.Text)
	if tgID == 0 {
		sendText(ctx, deps, msg.ChatID, "请输入合法的用户 tg_id（数字）。")
		return
	}
	deps.Sessions.Clear(msg.From.ID)
	var u db.User
	if err := deps.DB.Where("telegram_id = ?", tgID).First(&u).Error; err != nil {
		sendText(ctx, deps, msg.ChatID, "未找到该用户（tg_id="+itoa64s(tgID)+"）。")
		return
	}
	isAdmin := deps.IsSuper != nil && deps.IsSuper(u.TelegramID)
	perm := "否"
	if u.IsPermanent {
		perm = "是（白名单）"
	}
	expire := "无"
	if u.ExpireAt != nil {
		expire = u.ExpireAt.Format("2006-01-02")
	}
	sec := "否"
	if u.SecurityCodeHash != "" {
		sec = "是"
	}
	sendHTML(ctx, deps, msg.ChatID, fmt.Sprintf(
		"👤 <b>tg=%d</b>\n用户名：%s %s\nJellyfin：%s（%s）\n果果币：%d\n状态：%s\n白名单：%s\n到期：%s\n连签：%d 天\n安全码：%s\n管理员：%v",
		u.TelegramID, escapeHTML(u.FirstName), escapeHTML(u.LastName),
		escapeHTML(u.JellyfinUsername), escapeHTML(u.JellyfinUserID),
		u.GuoGuo, u.Status, perm, expire, u.SignStreak, sec, isAdmin))
}

// ---------------------------------------------------------------------------
// 3) 查询卡密（邀请码/续期码）溯源
// ---------------------------------------------------------------------------

// handleAdminQueryCodeStep 查询卡密向导：收卡密 → 查邀请码与续期码 → 展示状态与使用者。
func (r *Router) handleAdminQueryCodeStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消查询卡密。")
		return
	}
	code := strings.TrimSpace(msg.Text)
	if code == "" {
		sendText(ctx, deps, msg.ChatID, "卡密不能为空，请重新发送。")
		return
	}
	deps.Sessions.Clear(msg.From.ID)
	if deps.Pepper == "" {
		sendText(ctx, deps, msg.ChatID, "SECURITY_PEPPER 未配置，无法查询。")
		return
	}
	clean, err := codes.ValidateCodeFormat(code)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "卡密格式非法："+err.Error())
		return
	}
	hash, err := codes.HashCode(clean, deps.Pepper)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "卡密哈希失败："+err.Error())
		return
	}

	// 邀请码
	var inv db.InviteCode
	invFound := deps.DB.Where("code_hash = ?", hash).First(&inv).Error == nil
	// 续期码
	var ren db.RenewalCode
	renFound := deps.DB.Where("code_hash = ?", hash).First(&ren).Error == nil

	if !invFound && !renFound {
		sendText(ctx, deps, msg.ChatID, "❌ 未找到该卡密（邀请码或续期码），可能从未生成。")
		return
	}

	var b strings.Builder
	b.WriteString("🔍 <b>卡密溯源</b>\n\n")
	b.WriteString("<code>" + escapeHTML(clean) + "</code>\n\n")
	if invFound {
		b.WriteString("类型：🎟 邀请码\n")
		b.WriteString("状态：" + codeStatusText(inv.Status) + "\n")
		b.WriteString("批次：#" + itoa(int(inv.BatchID)) + "\n")
		if inv.UsedBy != nil {
			b.WriteString("使用者：" + userRef(deps, *inv.UsedBy) + "\n")
			if inv.UsedAt != nil {
				b.WriteString("使用时间：" + inv.UsedAt.Format("2006-01-02 15:04:05") + "\n")
			}
		}
		b.WriteString("\n")
	}
	if renFound {
		b.WriteString("类型：⏳ 续期码（" + itoa(ren.Days) + " 天）\n")
		b.WriteString("状态：" + codeStatusText(ren.Status) + "\n")
		b.WriteString("批次：#" + itoa(int(ren.BatchID)) + "\n")
		if ren.UsedBy != nil {
			b.WriteString("使用者：" + userRef(deps, *ren.UsedBy) + "\n")
			if ren.UsedAt != nil {
				b.WriteString("使用时间：" + ren.UsedAt.Format("2006-01-02 15:04:05") + "\n")
			}
		}
	}
	sendHTML(ctx, deps, msg.ChatID, b.String())
}

// codeStatusText 卡密状态中文。
func codeStatusText(s string) string {
	switch s {
	case db.CodeStatusUnused:
		return "🟢 未使用"
	case db.CodeStatusUsed:
		return "🔴 已使用"
	case db.CodeStatusRevoked:
		return "⚫ 已作废"
	default:
		return s
	}
}

// userRef 展示使用者：tg 昵称 + ID。
func userRef(deps *HandlerDeps, tgID int64) string {
	var u db.User
	if err := deps.DB.Where("telegram_id = ?", tgID).First(&u).Error; err == nil {
		return fmt.Sprintf("%s（tg=%d）", escapeHTML(u.DisplayName()), tgID)
	}
	return fmt.Sprintf("tg=%d", tgID)
}

// ---------------------------------------------------------------------------
// 4) 白名单（永久有效、不受规则约束、无需保号）
// ---------------------------------------------------------------------------

// handleAdminWLStep 白名单向导：Data.mode=add/del，收 tg_id。
func (r *Router) handleAdminWLStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消白名单操作。")
		return
	}
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	tgID := parseInt64Safe(msg.Text)
	if tgID == 0 {
		sendText(ctx, deps, msg.ChatID, "请输入合法的用户 tg_id（数字）。")
		return
	}
	mode, _ := sess.Data["mode"].(string)
	deps.Sessions.Clear(msg.From.ID)
	var u db.User
	if err := deps.DB.Where("telegram_id = ?", tgID).First(&u).Error; err != nil {
		sendText(ctx, deps, msg.ChatID, "未找到该用户（tg_id="+itoa64s(tgID)+"）。")
		return
	}
	switch mode {
	case "del":
		err := deps.DB.Model(&u).Update("is_permanent", false).Error
		if err != nil {
			sendText(ctx, deps, msg.ChatID, "移除白名单失败："+err.Error())
			return
		}
		_ = db.WriteAudit(deps.DB, msg.From.ID, "admin_whitelist_del", "user", itoa64s(tgID), "移除白名单")
		sendText(ctx, deps, msg.ChatID, fmt.Sprintf("✅ 已移除白名单：tg=%d（恢复受规则约束）", tgID))
	default: // add
		err := deps.DB.Model(&u).Updates(map[string]any{
			"is_permanent": true,
			"status":       db.UserStatusActive,
		}).Error
		if err != nil {
			sendText(ctx, deps, msg.ChatID, "添加白名单失败："+err.Error())
			return
		}
		_ = db.WriteAudit(deps.DB, msg.From.ID, "admin_whitelist_add", "user", itoa64s(tgID), "添加白名单")
		sendText(ctx, deps, msg.ChatID, fmt.Sprintf("✅ 已添加白名单：tg=%d（永久有效，不受规则约束，无需保号）", tgID))
	}
}

// ---------------------------------------------------------------------------
// 5) 设置邀请码/续期码积分价
// ---------------------------------------------------------------------------

// handleAdminPriceStep 定价向导：Data.kind=invite/renewal，收积分价。
func (r *Router) handleAdminPriceStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消定价设置。")
		return
	}
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	p := parseInt64Safe(msg.Text)
	if p < 0 {
		sendText(ctx, deps, msg.ChatID, "积分价不能为负，请重新输入（0=禁止兑换）。")
		return
	}
	kind, _ := sess.Data["kind"].(string)
	deps.Sessions.Clear(msg.From.ID)
	key := cfgKeyInvitePrice
	label := "邀请码"
	if kind == "renewal" {
		key = cfgKeyRenewalPrice
		label = "续期码"
	}
	if err := configSet(deps, key, itoa64s(p)); err != nil {
		sendText(ctx, deps, msg.ChatID, "保存定价失败："+err.Error())
		return
	}
	_ = db.WriteAudit(deps.DB, msg.From.ID, "admin_set_price", "system_config", key, fmt.Sprintf("%s 积分价=%d", label, p))
	sendText(ctx, deps, msg.ChatID, fmt.Sprintf("✅ 已设置 %s 积分价：🪙 %d 积分/张。", label, p))
}

// ---------------------------------------------------------------------------
// 6) Jellyfin 线路管理
// ---------------------------------------------------------------------------

// handleAdminLineAddStep 添加线路向导：收 URL（可选 名称|URL 或 名称空格URL）。
func (r *Router) handleAdminLineAddStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消添加线路。")
		return
	}
	deps.Sessions.Clear(msg.From.ID)
	raw := strings.TrimSpace(msg.Text)
	if raw == "" {
		sendText(ctx, deps, msg.ChatID, "线路不能为空。")
		return
	}
	// 支持「名称 URL」或「URL」「名称|URL」
	name, lineURL := parseLineInput(raw)
	if !isHTTPURL(lineURL) {
		sendText(ctx, deps, msg.ChatID, "线路地址必须是 http(s):// 开头的完整 URL。")
		return
	}
	row := db.JellyfinLine{Name: name, URL: lineURL, Order: 0}
	if err := deps.DB.Create(&row).Error; err != nil {
		if isUniqueViolation(err, "url") {
			sendText(ctx, deps, msg.ChatID, "该线路已存在。")
			return
		}
		sendText(ctx, deps, msg.ChatID, "添加线路失败："+err.Error())
		return
	}
	_ = db.WriteAudit(deps.DB, msg.From.ID, "admin_line_add", "jellyfin_line", itoa(int(row.ID)), lineURL)
	sendText(ctx, deps, msg.ChatID, fmt.Sprintf("✅ 已添加线路 #%d：%s %s", row.ID, name, lineURL))
}

// handleAdminLineDelStep 删除线路向导：收 id 或 URL。
func (r *Router) handleAdminLineDelStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消删除线路。")
		return
	}
	deps.Sessions.Clear(msg.From.ID)
	raw := strings.TrimSpace(msg.Text)
	if raw == "" {
		sendText(ctx, deps, msg.ChatID, "请输入线路编号或 URL。")
		return
	}
	var id uint
	if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
		id = uint(n)
	}
	q := deps.DB
	if id > 0 {
		q = q.Where("id = ?", id)
	} else {
		q = q.Where("url = ?", raw)
	}
	res := q.Delete(&db.JellyfinLine{})
	if res.Error != nil {
		sendText(ctx, deps, msg.ChatID, "删除线路失败："+res.Error.Error())
		return
	}
	if res.RowsAffected == 0 {
		sendText(ctx, deps, msg.ChatID, "未找到该线路。")
		return
	}
	_ = db.WriteAudit(deps.DB, msg.From.ID, "admin_line_del", "jellyfin_line", raw, "删除线路")
	sendText(ctx, deps, msg.ChatID, "✅ 已删除线路："+raw)
}

// sendLineList 发送线路列表（管理面板或用户查询共用）。
func sendLineList(ctx context.Context, deps *HandlerDeps, chatID int64) {
	var lines []db.JellyfinLine
	if err := deps.DB.Order("`order` asc, id asc").Find(&lines).Error; err != nil {
		sendText(ctx, deps, chatID, "查询线路失败："+err.Error())
		return
	}
	if len(lines) == 0 {
		sendText(ctx, deps, chatID, "暂无可用线路，请联系管理员配置。")
		return
	}
	var b strings.Builder
	b.WriteString("🌐 <b>Jellyfin 可用线路</b>\n\n")
	for _, l := range lines {
		name := strings.TrimSpace(l.Name)
		label := strings.TrimSpace(l.URL)
		if name != "" {
			label = name + "\n" + label
		}
		b.WriteString("• " + escapeHTML(label) + "\n")
	}
	sendHTML(ctx, deps, chatID, b.String())
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func itoa64s(v int64) string {
	return strconv.FormatInt(v, 10)
}

func isCancelText(s string) bool {
	return strings.EqualFold(strings.TrimSpace(s), "/cancel")
}

func parseLineInput(raw string) (name, lineURL string) {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, '|'); i >= 0 {
		return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+1:])
	}
	fields := strings.Fields(raw)
	if len(fields) >= 2 && strings.HasPrefix(fields[len(fields)-1], "http") {
		return strings.Join(fields[:len(fields)-1], " "), fields[len(fields)-1]
	}
	return "", raw
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func isUniqueViolation(err error, _ string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "constraint")
}

// ---------------------------------------------------------------------------
// 7) 注册与兑换：开注 / 积分兑换邀请码开关 / 兑换配额
// ---------------------------------------------------------------------------

// handleAdminQuotaStep 设置积分兑换邀请码配额向导（收整数，0=不限）。
func (r *Router) handleAdminQuotaStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if isCancelText(msg.Text) {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消设置配额。")
		return
	}
	q := parseInt64Safe(msg.Text)
	if q < 0 {
		sendText(ctx, deps, msg.ChatID, "配额必须是非负整数（0=不限），请重新输入。")
		return
	}
	deps.Sessions.Clear(msg.From.ID)
	if err := configSet(deps, cfgKeyExchangeQuota, itoa64s(q)); err != nil {
		sendText(ctx, deps, msg.ChatID, "保存配额失败："+err.Error())
		return
	}
	_ = db.WriteAudit(deps.DB, msg.From.ID, "admin_set_exchange_quota", "system_config", cfgKeyExchangeQuota, fmt.Sprintf("兑换邀请码配额=%d", q))
	sendText(ctx, deps, msg.ChatID, fmt.Sprintf("✅ 已设置积分兑换邀请码配额：%d（0=不限）。", q))
}

// toggleRegistrationOpen 切换开注状态，返回新状态文案。
func toggleRegistrationOpen(ctx context.Context, deps *HandlerDeps, adminID int64) (string, error) {
	now := "1"
	if registrationOpen(deps) {
		now = "0"
	}
	if err := configSet(deps, cfgKeyRegOpen, now); err != nil {
		return "", err
	}
	_ = db.WriteAudit(deps.DB, adminID, "admin_toggle_reg_open", "system_config", cfgKeyRegOpen, "开注="+now)
	if now == "1" {
		return "✅ 已开启开注：新用户注册免邀请码。", nil
	}
	return "❌ 已关闭开注：注册需邀请码。", nil
}

// toggleExchangeInvite 切换积分兑换邀请码开关，返回新状态文案。
func toggleExchangeInvite(ctx context.Context, deps *HandlerDeps, adminID int64) (string, error) {
	now := "1"
	if exchangeInviteEnabled(deps) {
		now = "0"
	}
	if err := configSet(deps, cfgKeyExchangeEnabled, now); err != nil {
		return "", err
	}
	_ = db.WriteAudit(deps.DB, adminID, "admin_toggle_exchange_invite", "system_config", cfgKeyExchangeEnabled, "兑换邀请码="+now)
	if now == "1" {
		return "✅ 已开启积分兑换邀请码。", nil
	}
	return "❌ 已关闭积分兑换邀请码。", nil
}
