package bot

import (
	"context"
	"fmt"
	"time"

	"mora_bot/internal/db"
)

// ---------------------------------------------------------------------------
// 面板按钮 UI：所有功能通过内联键盘按钮操作，命令仅作兼容入口。
// 回调数据统一用 BuildCallbackData(domain, action, args...) 生成。
// ---------------------------------------------------------------------------

// mainPanel 主页面板。
func mainPanel(deps *HandlerDeps, u *db.User) (string, [][]KeyboardButton) {
	text := fmt.Sprintf(
		"🍯 <b>果果屋 · 主菜单</b>\n\n"+
			"👤 %s　🪙 %d 果果币\n\n"+
			"请选择下方按钮：",
		escapeHTML(u.DisplayName()), u.GuoGuo)

	rows := [][]KeyboardButton{
		{
			{Text: "✅ 每日签到", Data: BuildCallbackData(DKSign, "today")},
			{Text: "👤 我的账号", Data: BuildCallbackData(DKProfile, "view")},
		},
		{
			{Text: "🛒 果果币商店", Data: BuildCallbackData(DKShop, "view")},
			{Text: "🎬 追剧中心", Data: BuildCallbackData(DKDrama, "view")},
		},
		{
			{Text: "🔗 绑定已有账号", Data: BuildCallbackData(DKBind, "start")},
			{Text: "📝 注册新账号", Data: BuildCallbackData(DKBind, "register")},
		},
		{
			{Text: "🎟 使用续期码", Data: BuildCallbackData(DKMenu, "redeem")},
			{Text: "⚙️ 账号管理", Data: BuildCallbackData(DKAccount, "view")},
		},
		{
			{Text: "🌐 查询线路", Data: BuildCallbackData(DKMenu, "lines")},
		},
	}
	// 管理面板只能通过 /admin 调出（用户面板不展示管理入口）
	return text, rows
}

// shopPanel 商店子面板。
func shopPanel(deps *HandlerDeps, u *db.User) (string, [][]KeyboardButton) {
	price := renewalCodePrice(deps)
	days := deps.RenewalDays
	if days <= 0 {
		days = defaultShopDays
	}
	invPrice := inviteCodePrice(deps)
	invLine := fmt.Sprintf("• 邀请码：%d 果果币", invPrice)
	if !exchangeInviteEnabled(deps) {
		invLine = "• 邀请码：❌ 兑换已关闭（管理员已暂停积分兑换）"
	} else if invPrice <= 0 {
		invLine = "• 邀请码：暂未开放"
	} else if r := exchangeInviteRemaining(deps); r >= 0 {
		invLine = fmt.Sprintf("• 邀请码：%d 果果币（剩余名额 %d）", invPrice, r)
	}
	text := fmt.Sprintf(
		"🛒 <b>果果币商店</b>\n\n"+
			"当前余额：🪙 %d\n\n"+
			"• 续期码：%d 果果币 / %d 天\n"+
			"%s\n\n"+
			"购买后获得卡密码，用于续期或邀请新用户。",
		u.GuoGuo, price, days, invLine)
	rows := [][]KeyboardButton{
		{
			{Text: "💳 购买续期码", Data: BuildCallbackData(DKShop, "buy")},
		},
		{
			{Text: "🎫 购买邀请码", Data: BuildCallbackData(DKShop, "invite")},
		},
		{
			{Text: "↩️ 返回主菜单", Data: BuildCallbackData(DKMenu, "home")},
		},
	}
	return text, rows
}

// accountPanel 账号管理子面板。
func accountPanel(deps *HandlerDeps, u *db.User) (string, [][]KeyboardButton) {
	text := fmt.Sprintf(
		"⚙️ <b>账号管理</b>\n\n"+
			"绑定状态：%s\n账号识别：%s",
		accountBindStatus(u), escapeHTML(u.DisplayName()))
	rows := [][]KeyboardButton{
		{
			{Text: "🔑 修改密码", Data: BuildCallbackData(DKAccount, "pwd")},
			{Text: "🔗 解除绑定", Data: BuildCallbackData(DKAccount, "unbind")},
		},
		{
			{Text: "🔐 设置安全码", Data: BuildCallbackData(DKAccount, "security")},
			{Text: "🗑 注销账号", Data: BuildCallbackData(DKAccount, "delete")},
		},
		{
			{Text: "↩️ 返回主菜单", Data: BuildCallbackData(DKMenu, "home")},
		},
	}
	return text, rows
}

// dramaPanel 追剧中心子面板。
func dramaPanel(deps *HandlerDeps, u *db.User) (string, [][]KeyboardButton) {
	text := "🎬 <b>追剧中心</b>\n\n求剧请发送<b>红果短剧分享链接</b>，没有链接可补剧名+主演名。"
	if deps.DramaDailyLimit > 0 {
		text += fmt.Sprintf("\n今日剩余次数：%d 次", dramaRemaining(deps, u.TelegramID))
	}
	rows := [][]KeyboardButton{
		{
			{Text: "➕ 我要求剧", Data: BuildCallbackData(DKDrama, "create")},
			{Text: "📋 我的记录", Data: BuildCallbackData(DKDrama, "list")},
		},
		{
			{Text: "↩️ 返回主菜单", Data: BuildCallbackData(DKMenu, "home")},
		},
	}
	return text, rows
}

// adminPanel 管理面板子面板（仅超管，只能 /admin 调出）。
func adminPanel(deps *HandlerDeps, u *db.User) (string, [][]KeyboardButton) {
	text := "🛠 <b>管理面板</b>\n\n请选择操作："
	rows := [][]KeyboardButton{
		{
			{Text: "📊 全局统计", Data: BuildCallbackData(DKAdmin, "stats")},
		},
		{
			{Text: "🎟 生成邀请码", Data: BuildCallbackData(DKAdmin, "gencode")},
			{Text: "⏳ 生成续期码", Data: BuildCallbackData(DKAdmin, "genrenew")},
		},
		{
			{Text: "🔍 查询卡密", Data: BuildCallbackData(DKAdmin, "qcode")},
			{Text: "🪙 调整积分", Data: BuildCallbackData(DKAdmin, "adjpoints")},
		},
		{
			{Text: "👤 查询用户", Data: BuildCallbackData(DKAdmin, "quser")},
			{Text: "✅ 白名单", Data: BuildCallbackData(DKAdmin, "whitelist")},
		},
		{
			{Text: "🏷 卡密定价", Data: BuildCallbackData(DKAdmin, "prices")},
			{Text: "🌐 线路管理", Data: BuildCallbackData(DKAdmin, "lines")},
		},
		{
			{Text: "🔓 注册与兑换", Data: BuildCallbackData(DKAdmin, "reg")},
			{Text: "🎫 工单管理", Data: BuildCallbackData(DKAdmin, "tickets")},
		},
		{
			{Text: "↩️ 返回主菜单", Data: BuildCallbackData(DKMenu, "home")},
		},
	}
	return text, rows
}

// sendPanel 发送面板（messageID<=0 发送新消息，否则原地编辑实现"面板导航"）。
func sendPanel(ctx context.Context, deps *HandlerDeps, chatID int64, messageID int, text string, rows [][]KeyboardButton) {
	if deps == nil || deps.Snd == nil {
		return
	}
	if messageID > 0 {
		_ = deps.Snd.EditKeyboard(ctx, chatID, messageID, text, rows)
		return
	}
	_ = deps.Snd.SendKeyboard(ctx, chatID, text, rows)
}

// dramaRemaining 今天剩余求剧次数（-1 表示不限）。
func dramaRemaining(deps *HandlerDeps, userID int64) int {
	if deps == nil || deps.DB == nil || deps.DramaDailyLimit <= 0 {
		return -1
	}
	from := time.Now().In(db.ChinaLoc).Format("2006-01-02 00:00:00")
	var cnt int64
	deps.DB.Model(&db.DramaRequest{}).
		Where("user_id = ? AND created_at >= ?", userID, from).
		Count(&cnt)
	left := deps.DramaDailyLimit - int(cnt)
	if left < 0 {
		left = 0
	}
	return left
}
