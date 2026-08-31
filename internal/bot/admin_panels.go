package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"mora_bot/internal/db"
)

// ---------------------------------------------------------------------------
// 运行时配置（SystemConfig 表）：邀请码/续期码积分价等
// ---------------------------------------------------------------------------

const (
	cfgKeyInvitePrice     = "invite_code_price"         // 邀请码积分价
	cfgKeyRenewalPrice    = "renewal_code_price"        // 续期码积分价
	cfgKeyRegOpen         = "registration_open"         // "1"=开注（免邀请码注册）
	cfgKeyRegQuota        = "registration_quota"        // 开注名额（0=不限）
	cfgKeyRegUsed         = "registration_open_used"    // 本轮开注已用名额
	cfgKeyExchangeEnabled = "exchange_invite_enabled"   // "1"=允许积分兑换邀请码（默认开）
	cfgKeyExchangeQuota   = "exchange_invite_quota"     // 积分兑换邀请码配额，0=不限
)

// configGet 读 system_configs 文本值；不存在返回默认值。
func configGet(deps *HandlerDeps, key, def string) string {
	if deps == nil || deps.DB == nil {
		return def
	}
	var c db.SystemConfig
	if err := deps.DB.Where("`key` = ?", key).First(&c).Error; err != nil {
		return def
	}
	if strings.TrimSpace(c.Value) == "" {
		return def
	}
	return c.Value
}

// configSet 写 system_configs（upsert）。
func configSet(deps *HandlerDeps, key, val string) error {
	if deps == nil || deps.DB == nil {
		return gorm.ErrInvalidDB
	}
	val = strings.TrimSpace(val)
	row := db.SystemConfig{Key: key, Value: val, UpdatedAt: time.Now()}
	// upsert
	return deps.DB.Where("`key` = ?", key).
		Assign(db.SystemConfig{Value: val, UpdatedAt: time.Now()}).
		FirstOrCreate(&row).Error
}

// configGetInt 读整数配置；非法或缺失返回默认值。
func configGetInt(deps *HandlerDeps, key string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(configGet(deps, key, "")))
	if err != nil || v < 0 {
		return def
	}
	return v
}

// inviteCodePrice 邀请码当前积分价（数据库配置优先，回退环境变量）。
func inviteCodePrice(deps *HandlerDeps) int {
	if p := configGetInt(deps, cfgKeyInvitePrice, -1); p >= 0 {
		return p
	}
	if deps != nil && deps.PriceInviteCode > 0 {
		return deps.PriceInviteCode
	}
	return defaultInvitePrice
}

// renewalCodePrice 续期码当前积分价（数据库配置优先，回退环境变量）。
func renewalCodePrice(deps *HandlerDeps) int {
	if p := configGetInt(deps, cfgKeyRenewalPrice, -1); p >= 0 {
		return p
	}
	if deps != nil && deps.RenewalPrice > 0 {
		return deps.RenewalPrice
	}
	return defaultShopPrice
}

// registrationOpen 开注状态：开注时新用户无需邀请码即可注册。
func registrationOpen(deps *HandlerDeps) bool {
	return configGet(deps, cfgKeyRegOpen, "0") == "1"
}

// exchangeInviteEnabled 是否允许用积分兑换邀请码（独立于开注，默认开）。
func exchangeInviteEnabled(deps *HandlerDeps) bool {
	return configGet(deps, cfgKeyExchangeEnabled, "1") != "0"
}

// exchangeInviteQuota 积分兑换邀请码配额（0=不限）。
func exchangeInviteQuota(deps *HandlerDeps) int {
	return configGetInt(deps, cfgKeyExchangeQuota, 0)
}

// exchangedInviteCount 已通过积分兑换发放的邀请码数量（配额统计）。
func exchangedInviteCount(deps *HandlerDeps) int64 {
	if deps == nil || deps.DB == nil {
		return 0
	}
	var n int64
	deps.DB.Model(&db.InviteCode{}).Where("source = ?", "exchange").Count(&n)
	return n
}

// exchangeInviteRemaining 剩余积分兑换名额（-1 表示不限）。
func exchangeInviteRemaining(deps *HandlerDeps) int {
	q := exchangeInviteQuota(deps)
	if q <= 0 {
		return -1
	}
	r := q - int(exchangedInviteCount(deps))
	if r < 0 {
		r = 0
	}
	return r
}

// openRegQuota 开注名额（0=不限）。
func openRegQuota(deps *HandlerDeps) int {
	return configGetInt(deps, cfgKeyRegQuota, 0)
}

// openRegUsed 本轮开注已注册名额数。
func openRegUsed(deps *HandlerDeps) int {
	return configGetInt(deps, cfgKeyRegUsed, 0)
}

// openRegRemaining 剩余开注名额（-1 表示不限）。
func openRegRemaining(deps *HandlerDeps) int {
	q := openRegQuota(deps)
	if q <= 0 {
		return -1
	}
	r := q - openRegUsed(deps)
	if r < 0 {
		r = 0
	}
	return r
}

// openRegHasSlot 开注模式下是否还有剩余名额（名额不限时恒为 true）。
func openRegHasSlot(deps *HandlerDeps) bool {
	rem := openRegRemaining(deps)
	return rem < 0 || rem > 0
}

// ensureOpenRegUsedRow 保证开注已用计数行存在（初值 0）。
func ensureOpenRegUsedRow(deps *HandlerDeps) {
	if deps == nil || deps.DB == nil {
		return
	}
	deps.DB.Where("`key` = ?", cfgKeyRegUsed).
		FirstOrCreate(&db.SystemConfig{Key: cfgKeyRegUsed, Value: "0"})
}

// consumeOpenRegSlot 开注模式下占用一个名额（设有名额上限时）。
// 通过单条条件 UPDATE 保证并发下不超发；名额耗尽时自动关闭开注并通知管理员。
// 返回是否成功占用（名额不限时恒成功）。
func consumeOpenRegSlot(ctx context.Context, deps *HandlerDeps) bool {
	if deps == nil || deps.DB == nil {
		return false
	}
	q := openRegQuota(deps)
	if q <= 0 {
		return true // 不限名额
	}
	ensureOpenRegUsedRow(deps)
	res := deps.DB.Model(&db.SystemConfig{}).
		Where("`key` = ? AND CAST(value AS INTEGER) < ?", cfgKeyRegUsed, q).
		Update("value", gorm.Expr("CAST(value AS INTEGER) + 1"))
	if res.Error != nil || res.RowsAffected == 0 {
		// 名额已用尽（写库异常也按用尽处理）：自动关闭开注
		if registrationOpen(deps) {
			closeOpenRegistration(ctx, deps, "名额耗尽")
		}
		return false
	}
	if openRegUsed(deps) >= q {
		// 刚好占用最后一个名额：自动关闭
		closeOpenRegistration(ctx, deps, "名额耗尽")
	}
	return true
}

// refundOpenRegSlot 归还一个开注名额（注册中途失败回退计数；不限名额时无操作）。
func refundOpenRegSlot(deps *HandlerDeps) {
	if deps == nil || deps.DB == nil || openRegQuota(deps) <= 0 {
		return
	}
	deps.DB.Model(&db.SystemConfig{}).
		Where("`key` = ? AND CAST(value AS INTEGER) > 0", cfgKeyRegUsed).
		Update("value", gorm.Expr("CAST(value AS INTEGER) - 1"))
}

// resetOpenRegRound 开启新一轮开注：已用名额计数清零。
func resetOpenRegRound(deps *HandlerDeps) {
	if deps == nil || deps.DB == nil {
		return
	}
	_ = configSet(deps, cfgKeyRegUsed, "0")
}

// closeOpenRegistration 关闭开注（恢复需邀请码），写审计并私聊通知管理员。
func closeOpenRegistration(ctx context.Context, deps *HandlerDeps, reason string) {
	if deps == nil || deps.DB == nil {
		return
	}
	if err := configSet(deps, cfgKeyRegOpen, "0"); err != nil {
		return
	}
	_ = db.WriteAudit(deps.DB, 0, "reg_auto_close", "system_config", cfgKeyRegOpen, "开注自动关闭："+reason)
	if deps.Snd == nil {
		return
	}
	for _, id := range deps.SuperAdminIDs {
		sendText(ctx, deps, id,
			"📢 开注名额已用完，注册已自动关闭（恢复需邀请码）。\n"+
				"如需继续开注：/admin → 🔓 注册与兑换 → 🎯 设置开注名额。")
	}
}

// registrationAvailable 开注是否对用户可用：开注开启且有剩余名额。
// 名额已用尽时自动关闭开注并通知管理员，返回 false。
func registrationAvailable(ctx context.Context, deps *HandlerDeps) bool {
	if !registrationOpen(deps) {
		return false
	}
	if openRegHasSlot(deps) {
		return true
	}
	closeOpenRegistration(ctx, deps, "名额耗尽")
	return false
}

// ---------------------------------------------------------------------------
// 白名单子面板
// ---------------------------------------------------------------------------

const defaultInvitePrice = 300

// whitelistPanel 白名单管理子面板：白名单用户不受规则约束、永久有效、无需保号。
func whitelistPanel(deps *HandlerDeps, u *db.User) (string, [][]KeyboardButton) {
	var perms int64
	deps.DB.Model(&db.User{}).Where("is_permanent = ?", true).Count(&perms)
	text := fmt.Sprintf(
		"✅ <b>白名单管理</b>\n\n"+
			"白名单用户不受规则约束：永久有效、无需保号、不提醒续费。\n\n"+
			"当前白名单：%d 人",
		perms)
	rows := [][]KeyboardButton{
		{
			{Text: "➕ 添加白名单", Data: BuildCallbackData(DKAdmin, "wl:add")},
			{Text: "➖ 移除白名单", Data: BuildCallbackData(DKAdmin, "wl:del")},
		},
		{
			{Text: "📋 查看白名单", Data: BuildCallbackData(DKAdmin, "wl:list")},
		},
		{
			{Text: "↩️ 返回管理面板", Data: BuildCallbackData(DKAdmin, "view")},
		},
	}
	return text, rows
}

// pricesPanel 卡密定价子面板。
func pricesPanel(deps *HandlerDeps, u *db.User) (string, [][]KeyboardButton) {
	text := fmt.Sprintf(
		"🏷 <b>卡密积分定价</b>\n\n"+
			"邀请码：🪙 %d 积分/张\n续期码：🪙 %d 积分/张\n（非 0 表示可用果果币在商店兑换）",
		inviteCodePrice(deps), renewalCodePrice(deps))
	rows := [][]KeyboardButton{
		{
			{Text: "🎫 设置邀请码价", Data: BuildCallbackData(DKAdmin, "price:invite")},
			{Text: "💳 设置续期码价", Data: BuildCallbackData(DKAdmin, "price:renewal")},
		},
		{
			{Text: "↩️ 返回管理面板", Data: BuildCallbackData(DKAdmin, "view")},
		},
	}
	return text, rows
}

// linesPanel 线路管理子面板。
func linesPanel(deps *HandlerDeps, u *db.User) (string, [][]KeyboardButton) {
	var count int64
	deps.DB.Model(&db.JellyfinLine{}).Count(&count)
	text := fmt.Sprintf("🌐 <b>Jellyfin 线路管理</b>\n\n当前线路：%d 条", count)
	rows := [][]KeyboardButton{
		{
			{Text: "📋 查看线路", Data: BuildCallbackData(DKAdmin, "lines:list")},
		},
		{
			{Text: "➕ 添加线路", Data: BuildCallbackData(DKAdmin, "lines:add")},
			{Text: "🗑 删除线路", Data: BuildCallbackData(DKAdmin, "lines:del")},
		},
		{
			{Text: "↩️ 返回管理面板", Data: BuildCallbackData(DKAdmin, "view")},
		},
	}
	return text, rows
}

// regPanel 注册与兑换子面板：开注（含名额）/ 积分兑换邀请码开关与配额。
func regPanel(deps *HandlerDeps, u *db.User) (string, [][]KeyboardButton) {
	open := "❌ 关闭"
	if registrationOpen(deps) {
		open = "✅ 开启"
	}
	quota := "不限"
	if q := openRegQuota(deps); q > 0 {
		quota = fmt.Sprintf("%d（已用 %d，剩余 %d）", q, openRegUsed(deps), openRegRemaining(deps))
	}
	exch := "✅ 开启"
	if !exchangeInviteEnabled(deps) {
		exch = "❌ 关闭"
	}
	equota := "不限"
	remaining := exchangeInviteRemaining(deps)
	if q := exchangeInviteQuota(deps); q > 0 {
		equota = fmt.Sprintf("%d（剩余 %d）", q, remaining)
	}
	text := fmt.Sprintf(
		"🔓 <b>注册与兑换</b>\n\n"+
			"开注（免邀请码注册）：%s\n"+
			"开注名额：%s\n"+
			"积分兑换邀请码：%s\n"+
			"兑换邀请码配额：%s\n\n"+
			"（开注时新用户无需邀请码；设有名额时每注册成功 1 人扣 1 个，用完自动关闭）",
		open, quota, exch, equota)
	rows := [][]KeyboardButton{
		{
			{Text: "📢 开注：切换", Data: BuildCallbackData(DKAdmin, "reg:open")},
			{Text: "🎯 设置开注名额", Data: BuildCallbackData(DKAdmin, "reg:regquota")},
		},
		{
			{Text: "💱 兑换邀请码：切换", Data: BuildCallbackData(DKAdmin, "reg:exchange")},
		},
		{
			{Text: "🎟 设置兑换配额", Data: BuildCallbackData(DKAdmin, "reg:quota")},
		},
		{
			{Text: "↩️ 返回管理面板", Data: BuildCallbackData(DKAdmin, "view")},
		},
	}
	return text, rows
}

// ---------------------------------------------------------------------------
// 求剧工单管理子面板
// ---------------------------------------------------------------------------

// ticketsPanel 求剧工单管理子面板：按状态查询工单。
func ticketsPanel(deps *HandlerDeps, u *db.User) (string, [][]KeyboardButton) {
	countByStatus := func(status string) int64 {
		var n int64
		deps.DB.Model(&db.DramaRequest{}).Where("status = ?", status).Count(&n)
		return n
	}
	text := fmt.Sprintf(
		"🎫 <b>求剧工单管理</b>\n\n"+
			"🕐 未处理：%d\n"+
			"🧑‍🔧 已接单（处理中）：%d\n"+
			"✅ 已完成：%d\n"+
			"❌ 已驳回：%d",
		countByStatus(db.DramaStatusPending),
		countByStatus(db.DramaStatusClaimed),
		countByStatus(db.DramaStatusCompleted),
		countByStatus(db.DramaStatusRejected))
	rows := [][]KeyboardButton{
		{
			{Text: "🕐 未处理", Data: BuildCallbackData(DKAdmin, "tickets:list", db.DramaStatusPending)},
			{Text: "🧑‍🔧 已接单", Data: BuildCallbackData(DKAdmin, "tickets:list", db.DramaStatusClaimed)},
		},
		{
			{Text: "✅ 已完成", Data: BuildCallbackData(DKAdmin, "tickets:list", db.DramaStatusCompleted)},
			{Text: "❌ 已驳回", Data: BuildCallbackData(DKAdmin, "tickets:list", db.DramaStatusRejected)},
		},
		{
			{Text: "↩️ 返回管理面板", Data: BuildCallbackData(DKAdmin, "view")},
		},
	}
	return text, rows
}

// sendDramaTicketList 按状态列出最近 10 条求剧工单；
// 未结单条目附「🎬 处理」按钮，点击发该工单的操作卡片。
func sendDramaTicketList(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, status string) {
	var list []db.DramaRequest
	if err := deps.DB.Where("status = ?", status).Order("id desc").Limit(10).Find(&list).Error; err != nil {
		sendText(ctx, deps, cq.ChatID, "查询工单失败。")
		return
	}
	if len(list) == 0 {
		sendText(ctx, deps, cq.ChatID, dramaStatusText(status)+"：暂无工单。")
		return
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("🎫 <b>%s</b>（最近 %d 条）\n\n", dramaStatusText(status), len(list)))
	rows := [][]KeyboardButton{}
	for _, it := range list {
		who := "tg_" + itoa(int(it.UserID))
		if it.TgUsername != "" {
			who = "@" + it.TgUsername
		}
		b.WriteString(fmt.Sprintf("#%d《%s》\n　👤 %s · 🕐 %s",
			it.ID, escapeHTML(it.Title), who,
			it.CreatedAt.In(db.ChinaLoc).Format("01-02 15:04")))
		switch it.Status {
		case db.DramaStatusClaimed:
			b.WriteString("\n　└ 🙋 接单：" + escapeHTML(it.ClaimedByName))
		case db.DramaStatusCompleted:
			b.WriteString("\n　└ ✅ 处理人：" + escapeHTML(it.ClaimedByName))
		case db.DramaStatusRejected:
			b.WriteString("\n　└ 🚫 理由：" + escapeHTML(it.RejectReason))
		}
		b.WriteString("\n\n")
		if it.Status == db.DramaStatusPending || it.Status == db.DramaStatusClaimed {
			rows = append(rows, []KeyboardButton{
				{Text: fmt.Sprintf("🎬 处理 #%d", it.ID), Data: BuildCallbackData(DKAdmin, "tcard", itoa(int(it.ID)))},
			})
		}
	}
	rows = append(rows, []KeyboardButton{
		{Text: "🔄 刷新列表", Data: BuildCallbackData(DKAdmin, "tickets:list", status)},
		{Text: "↩️ 返回工单面板", Data: BuildCallbackData(DKAdmin, "tickets")},
	})
	sendPanel(ctx, deps, cq.ChatID, 0, b.String(), rows)
}

// dramaTicketFooter 已接单/已结单工单的状态行（待处理返回空）。
func dramaTicketFooter(req *db.DramaRequest) string {
	switch req.Status {
	case db.DramaStatusClaimed:
		return "🙋 已由 " + escapeHTML(req.ClaimedByName) + " 接单"
	case db.DramaStatusCompleted:
		return "✅ 已完成（" + escapeHTML(req.ClaimedByName) + "）"
	case db.DramaStatusRejected:
		return "❌ 已驳回（" + escapeHTML(req.ClaimedByName) + "）：" + escapeHTML(req.RejectReason)
	}
	return ""
}

// sendDramaTicketCard 给管理员发送单个工单的操作卡片（含链接与接单/完成/驳回按钮）。
// 发送的是新消息，卡片按钮回调会携带本消息 ID，处理结果原地更新。
func sendDramaTicketCard(ctx context.Context, deps *HandlerDeps, chatID int64, req *db.DramaRequest) {
	sendPanel(ctx, deps, chatID, 0,
		dramaAdminCard(req, dramaOwner(deps, req), dramaTicketFooter(req)),
		dramaAdminRows(req, req.Status == db.DramaStatusPending || req.Status == db.DramaStatusClaimed))
}
