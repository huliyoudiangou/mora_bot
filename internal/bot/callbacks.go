package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mora_bot/internal/db"
)

// dispatchCallback 是所有 inline keyboard callback_query 的统一入口。
// 回调数据格式："domain:action[:arg...]"。
func dispatchCallback(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery) {
	if cq == nil || cq.Data == "" || cq.ID == "" || deps == nil || deps.Snd == nil {
		return
	}
	domain, action, args := ParseCallbackData(cq.Data)
	// 立刻 ACK，避免 TG 客户端转圈。
	// drama 与 admin 域的处理分支各自应答一次（带权限/结果文案），这里跳过，
	// 否则同一 callbackID 会被二次 ACK，Telegram 会拒绝且丢失反馈。
	if DomainKind(domain) != DKDrama && DomainKind(domain) != DKAdmin {
		_ = deps.Snd.AnswerCallback(ctx, cq.ID, "", false)
	}

	switch DomainKind(domain) {
	case DKMenu:
		handleMenuAction(ctx, deps, cq, action)
	case DKProfile:
		if action == "view" {
			m := &Message{From: cq.From, ChatID: cq.ChatID}
			(&Router{deps: deps}).cmdProfile(ctx, m, nil)
		}
	case DKSign:
		m := &Message{From: cq.From, ChatID: cq.ChatID, Text: "/signin"}
		(&Router{deps: deps}).cmdSignin(ctx, m, nil)
	case DKShop:
		switch action {
		case "view":
			u, err := ensureUser(ctx, deps, cq.From)
			if err != nil {
				sendText(ctx, deps, cq.ChatID, "查询失败，请稍后再试。")
				return
			}
			text, rows := shopPanel(deps, u)
			sendPanel(ctx, deps, cq.ChatID, messageIDOf(cq), text, rows)
		case "buy":
			m := &Message{From: cq.From, ChatID: cq.ChatID, Text: "/shop"}
			(&Router{deps: deps}).cmdShopBuy(ctx, m)
		case "invite":
			m := &Message{From: cq.From, ChatID: cq.ChatID, Text: "/shop invite"}
			(&Router{deps: deps}).cmdShopBuyInvite(ctx, m)
		}
	case DKBind:
		m := &Message{From: cq.From, ChatID: cq.ChatID}
		switch action {
		case "start":
			(&Router{deps: deps}).cmdBind(ctx, m, nil)
		case "register":
			(&Router{deps: deps}).cmdRegister(ctx, m, nil)
		}
	case DKAccount:
		handleAccountAction(ctx, deps, cq, action, args)
	case DKAdmin:
		handleAdminCallback(ctx, deps, cq, action, args)
	case DKDrama:
		handleDramaCallback(ctx, deps, cq, action, args)
	}
}

// handleMenuAction 处理主菜单导航按钮（menu:home / menu:redeem / menu:help）。
func handleMenuAction(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, action string) {
	u, err := ensureUser(ctx, deps, cq.From)
	if err != nil {
		sendText(ctx, deps, cq.ChatID, "查询失败，请稍后再试。")
		return
	}
	msgID := messageIDOf(cq)
	switch action {
	case "home":
		text, rows := mainPanel(deps, u)
		sendPanel(ctx, deps, cq.ChatID, msgID, text, rows)
	case "redeem":
		// 开始「使用续期码」会话：下一步收卡密
		deps.Sessions.Begin(cq.From.ID, sessRedeem)
		sendHTML(ctx, deps, cq.ChatID, "🎟 使用续期码\n请直接发送你的续期码（如 <code>R30-A1B2C3D4E5F6G7H8J9K2</code>）。\n\n回复 /cancel 可取消。")
	case "lines":
		// 用户面板「查询线路」：列出可用 Jellyfin 线路
		sendLineList(ctx, deps, cq.ChatID)
	case "help":
		sendText(ctx, deps, cq.ChatID, helpText)
		// 帮助后可回到面板
		text, rows := mainPanel(deps, u)
		sendPanel(ctx, deps, cq.ChatID, msgID, text, rows)
	}
}

// handleAccountAction 账号管理面板动作。
func handleAccountAction(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, action string, args []string) {
	m := &Message{From: cq.From, ChatID: cq.ChatID}
	switch action {
	case "view":
		u, err := ensureUser(ctx, deps, cq.From)
		if err != nil {
			sendText(ctx, deps, cq.ChatID, "查询失败，请稍后再试。")
			return
		}
		text, rows := accountPanel(deps, u)
		sendPanel(ctx, deps, cq.ChatID, messageIDOf(cq), text, rows)
	case "pwd":
		// 修改密码向导：安全码 → 旧密码 → 新密码
		(&Router{deps: deps}).cmdAccountPwd(ctx, m, nil)
	case "unbind":
		// 解绑必须走安全码向导；不提供可直接确认的回调，防止绕过身份校验。
		(&Router{deps: deps}).cmdAccountUnbind(ctx, m)
	case "security":
		(&Router{deps: deps}).cmdAccountSecurity(ctx, m, nil)
	case "delete":
		// 注销是高风险不可逆操作：不提供跳过二次确认的快捷回调。
		(&Router{deps: deps}).cmdAccountDelete(ctx, m, nil)
	case "devices":
		// 登录设备：列表 → 确认 → 执行。DKAccount 走 dispatchCallback 的全局 ACK，
		// 这里不要再 AnswerCallback（同一 callbackID 二次应答会被 Telegram 拒绝）。
		sub := ""
		if len(args) > 0 {
			sub = args[0]
		}
		switch sub {
		case "clear":
			cmdAccountDevicesConfirm(ctx, deps, cq)
		case "clearok":
			cmdAccountDevicesClear(ctx, deps, cq)
		default:
			cmdAccountDevices(ctx, deps, cq)
		}
	}
}

// handleAdminCallback 管理面板动作。
func handleAdminCallback(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, action string, args []string) {
	if !deps.IsSuper(cq.From.ID) {
		_ = deps.Snd.AnswerCallback(ctx, cq.ID, "您不是管理员，无法使用。", true)
		return
	}
	// 已授权：此处统一 ACK 一次；子分支不要再 AnswerCallback（避免重复应答）。
	_ = deps.Snd.AnswerCallback(ctx, cq.ID, "", false)
	m := &Message{From: cq.From, ChatID: cq.ChatID}
	switch action {
	case "view":
		u, err := ensureUser(ctx, deps, cq.From)
		if err != nil {
			sendText(ctx, deps, cq.ChatID, "查询失败，请稍后再试。")
			return
		}
		text, rows := adminPanel(deps, u)
		sendPanel(ctx, deps, cq.ChatID, messageIDOf(cq), text, rows)
	case "stats":
		(&Router{deps: deps}).handleAdminStats(ctx, m)
	case "gencode":
		// 面板「生成邀请码」：向导第一步收数量
		deps.Sessions.Begin(cq.From.ID, sessAdminGenInvite)
		sendHTML(ctx, deps, cq.ChatID, "🎟 生成邀请码\n请输入要生成的<b>数量</b>（1-200），例如：<code>10</code>\n\n回复 /cancel 可取消。")
	case "genrenew":
		// 面板「生成续期码」：向导第一步先收天数
		deps.Sessions.Begin(cq.From.ID, sessAdminGenRenew)
		sendHTML(ctx, deps, cq.ChatID, "⏳ 生成续期码 · 第 1 步\n请输入<b>续期天数</b>（1-3650），例如：<code>30</code>\n\n回复 /cancel 可取消。")
	case "qcode":
		deps.Sessions.Begin(cq.From.ID, sessAdminQueryCode)
		sendHTML(ctx, deps, cq.ChatID, "🔍 查询卡密\n请输入要查询的<b>邀请码或续期码</b>（原样粘贴）：\n\n回复 /cancel 可取消。")
	case "adjpoints":
		deps.Sessions.Begin(cq.From.ID, sessAdminAdjPoints)
		sendHTML(ctx, deps, cq.ChatID, "🪙 调整积分 · 第 1 步\n请输入目标用户的 <b>tg_id</b>：\n\n回复 /cancel 可取消。")
	case "quser":
		deps.Sessions.Begin(cq.From.ID, sessAdminQueryUser)
		sendHTML(ctx, deps, cq.ChatID, "👤 查询用户\n请输入要查询的用户的 <b>tg_id</b>：\n\n回复 /cancel 可取消。")
	case "whitelist":
		// 主面板入口：admin:whitelist → 打开白名单子面板
		sendAdminSub(ctx, deps, cq, "whitelist")
	case "wl":
		// 实际回调 admin:wl:add / admin:wl:del / admin:wl:list
		// ParseCallbackData 拆成 action="wl", args=["add"|"del"|"list"]（无 args 时是进入白名单子面板）
		switch {
		case len(args) == 0:
			sendAdminSub(ctx, deps, cq, "whitelist")
		case args[0] == "add":
			deps.Sessions.Begin(cq.From.ID, sessAdminWL)
			deps.Sessions.Advance(cq.From.ID, map[string]any{"mode": "add"})
			sendHTML(ctx, deps, cq.ChatID, "➕ 添加白名单\n请输入用户的 <b>tg_id</b>（将永久有效、不受规则约束、无需保号）：\n\n回复 /cancel 可取消。")
		case args[0] == "del":
			deps.Sessions.Begin(cq.From.ID, sessAdminWL)
			deps.Sessions.Advance(cq.From.ID, map[string]any{"mode": "del"})
			sendHTML(ctx, deps, cq.ChatID, "➖ 移除白名单\n请输入用户的 <b>tg_id</b>（将恢复受规则约束）：\n\n回复 /cancel 可取消。")
		case args[0] == "list":
			handleAdminWLList(ctx, deps, cq)
		}
	case "prices":
		// 主面板入口：admin:prices → 打开卡密定价子面板
		sendAdminSub(ctx, deps, cq, "prices")
	case "price":
		// 实际回调 admin:price:invite / admin:price:renewal
		switch {
		case len(args) == 0:
			sendAdminSub(ctx, deps, cq, "prices")
		case args[0] == "invite":
			deps.Sessions.Begin(cq.From.ID, sessAdminPrice)
			deps.Sessions.Advance(cq.From.ID, map[string]any{"kind": "invite"})
			sendHTML(ctx, deps, cq.ChatID, "🎫 设置邀请码积分价\n请输入每张邀请码所需 <b>积分</b>（0=禁止兑换），例如：<code>300</code>\n\n回复 /cancel 可取消。")
		case args[0] == "renewal":
			deps.Sessions.Begin(cq.From.ID, sessAdminPrice)
			deps.Sessions.Advance(cq.From.ID, map[string]any{"kind": "renewal"})
			sendHTML(ctx, deps, cq.ChatID, "💳 设置续期码积分价\n请输入每张续期码所需 <b>积分</b>（0=禁止兑换），例如：<code>150</code>\n\n回复 /cancel 可取消。")
		}
	case "lines":
		// 实际回调 admin:lines:list / admin:lines:add / admin:lines:del
		switch {
		case len(args) == 0:
			sendAdminSub(ctx, deps, cq, "lines")
		case args[0] == "list":
			sendLineList(ctx, deps, cq.ChatID)
		case args[0] == "add":
			deps.Sessions.Begin(cq.From.ID, sessAdminLineAdd)
			sendHTML(ctx, deps, cq.ChatID, "➕ 添加 Jellyfin 线路\n请输入线路地址（http/https），格式：<code>https://jf.example.com</code>\n如需名称：<code>主线路 https://jf.example.com</code> 或 <code>主线路|https://jf.example.com</code>\n\n回复 /cancel 可取消。")
		case args[0] == "del":
			deps.Sessions.Begin(cq.From.ID, sessAdminLineDel)
			sendHTML(ctx, deps, cq.ChatID, "🗑 删除 Jellyfin 线路\n请输入线路 <b>编号</b>（/admin lines:list 查看）或完整 URL：\n\n回复 /cancel 可取消。")
		}
	case "reg":
		// 实际回调 admin:reg:open / admin:reg:regquota / admin:reg:exchange / admin:reg:quota
		switch {
		case len(args) == 0:
			sendAdminSub(ctx, deps, cq, "reg")
		case args[0] == "open":
			if _, err := toggleRegistrationOpen(ctx, deps, cq.From.ID); err != nil {
				sendText(ctx, deps, cq.ChatID, "切换失败")
				return
			}
			sendAdminSub(ctx, deps, cq, "reg")
		case args[0] == "regquota":
			deps.Sessions.Begin(cq.From.ID, sessAdminRegQuota)
			sendText(ctx, deps, cq.ChatID,
				"🎯 设置开注名额\n请输入本轮开注的注册名额（非负整数）：\n"+
					"例如 <code>10</code> = 开注 10 个名额（自动开启开注，每注册 1 人扣 1 个，用完自动关闭）\n"+
					"<code>0</code> = 不限名额（不改变开注状态）\n\n回复 /cancel 可取消。")
		case args[0] == "exchange":
			if _, err := toggleExchangeInvite(ctx, deps, cq.From.ID); err != nil {
				sendText(ctx, deps, cq.ChatID, "切换失败")
				return
			}
			sendAdminSub(ctx, deps, cq, "reg")
		case args[0] == "quota":
			deps.Sessions.Begin(cq.From.ID, sessAdminQuota)
			sendHTML(ctx, deps, cq.ChatID, "🎟 设置积分兑换邀请码配额\n请输入允许兑换的邀请码<b>总数</b>（0=不限），例如：<code>10</code>\n\n回复 /cancel 可取消。")
		}
	case "user":
		// admin:user:disable:1001 等（兼容旧回调）
		handleAdminUserCallback(ctx, deps, cq, action, args)
	case "tickets":
		// admin:tickets → 工单子面板；admin:tickets:list:<status> → 按状态列表
		switch {
		case len(args) == 0:
			sendAdminSub(ctx, deps, cq, "tickets")
		case args[0] == "list" && len(args) >= 2:
			sendDramaTicketList(ctx, deps, cq, args[1])
		}
	case "tcard":
		// admin:tcard:<id> → 发送单个工单的操作卡片（含接单/完成/驳回按钮）
		if len(args) == 0 {
			return
		}
		id := parseInt64Safe(args[0])
		if id <= 0 {
			return
		}
		var req db.DramaRequest
		if err := deps.DB.First(&req, uint(id)).Error; err != nil {
			sendText(ctx, deps, cq.ChatID, "工单不存在")
			return
		}
		sendDramaTicketCard(ctx, deps, cq.ChatID, &req)
	}
}

// sendAdminSub 发送管理面板子面板（白名单/定价/线路）。
func sendAdminSub(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, which string) {
	if !deps.IsSuper(cq.From.ID) {
		return
	}
	u, err := ensureUser(ctx, deps, cq.From)
	if err != nil {
		sendText(ctx, deps, cq.ChatID, "查询失败，请稍后再试。")
		return
	}
	msgID := messageIDOf(cq)
	switch which {
	case "whitelist":
		text, rows := whitelistPanel(deps, u)
		sendPanel(ctx, deps, cq.ChatID, msgID, text, rows)
	case "prices":
		text, rows := pricesPanel(deps, u)
		sendPanel(ctx, deps, cq.ChatID, msgID, text, rows)
	case "lines":
		text, rows := linesPanel(deps, u)
		sendPanel(ctx, deps, cq.ChatID, msgID, text, rows)
	case "reg":
		text, rows := regPanel(deps, u)
		sendPanel(ctx, deps, cq.ChatID, msgID, text, rows)
	case "tickets":
		text, rows := ticketsPanel(deps, u)
		sendPanel(ctx, deps, cq.ChatID, msgID, text, rows)
	}
}

// handleAdminWLList 列出全部白名单用户。
func handleAdminWLList(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery) {
	if !deps.IsSuper(cq.From.ID) {
		return
	}
	var users []db.User
	if err := deps.DB.Where("is_permanent = ?", true).Find(&users).Error; err != nil {
		sendText(ctx, deps, cq.ChatID, "查询失败："+err.Error())
		return
	}
	if len(users) == 0 {
		sendText(ctx, deps, cq.ChatID, "当前暂无白名单用户。")
		return
	}
	var b strings.Builder
	b.WriteString("✅ <b>白名单用户</b>\n\n")
	for _, u := range users {
		line := fmt.Sprintf("• %s（tg=%d）", escapeHTML(u.DisplayName()), u.TelegramID)
		if u.JellyfinUsername != "" {
			line += " · JF:" + escapeHTML(u.JellyfinUsername)
		}
		b.WriteString(line + "\n")
	}
	sendHTML(ctx, deps, cq.ChatID, b.String())
}

// handleDramaCallback 追剧面板动作。
// 注意：本函数内每个分支必须恰好 AnswerCallback 一次（dispatchCallback 对 drama 域不做全局 ACK）。
func handleDramaCallback(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, action string, args []string) {
	ack := func(text string, alert bool) {
		_ = deps.Snd.AnswerCallback(ctx, cq.ID, text, alert)
	}
	m := &Message{From: cq.From, ChatID: cq.ChatID}
	switch action {
	case "view":
		ack("", false)
		u, err := ensureUser(ctx, deps, cq.From)
		if err != nil {
			sendText(ctx, deps, cq.ChatID, "查询失败，请稍后再试。")
			return
		}
		text, rows := dramaPanel(deps, u)
		sendPanel(ctx, deps, cq.ChatID, messageIDOf(cq), text, rows)
	case "create":
		// 开启会话：要求发送红果短剧分享链接（无链接则补剧名+主演名）
		ack("", false)
		deps.Sessions.Begin(cq.From.ID, sessDramaFeedback)
		sendHTML(ctx, deps, cq.ChatID, "🎬 求剧 · 第 1 步\n请发送 <b>红果短剧分享链接</b>（App 内「分享 → 复制链接」粘贴过来）。\n\n如果没有链接，请直接发送：剧名 主演名\n（例如：双面人生 / 杨幂）\n\n回复 /cancel 可取消。")
	case "list":
		ack("", false)
		(&Router{deps: deps}).cmdListDrama(ctx, m)
	case "claim", "resolve", "reject":
		// 管理员处理动作：接单 / 完成 / 驳回
		if !deps.IsSuper(cq.From.ID) {
			ack("无权限", true)
			return
		}
		if len(args) == 0 {
			ack("参数错误", true)
			return
		}
		id := parseInt64Safe(args[0])
		if id <= 0 {
			ack("参数错误", true)
			return
		}
		switch action {
		case "claim":
			req, res := claimDramaRequest(ctx, deps, uint(id), cq.From)
			switch res {
			case claimOK:
				ack("已接单 ✔ 处理完后点「完成」或「驳回」", false)
				editAdminDramaCard(ctx, deps, cq, req,
					fmt.Sprintf("🙋 已由 %s 接单（%s）",
						escapeHTML(req.ClaimedByName),
						time.Now().In(db.ChinaLoc).Format("01-02 15:04")))
			case claimAlreadyMine:
				ack("你已接单，处理完后点「完成」或「驳回」", false)
			case claimTaken:
				ack("该工单已被 "+req.ClaimedByName+" 接单，请勿重复处理", true)
			case claimSettled:
				ack("该工单已处理完成", true)
			default:
				ack("接单失败，请稍后再试", true)
			}
		case "resolve":
			req, res := dramaSettleRequest(ctx, deps, uint(id), db.DramaStatusCompleted, cq.From, "")
			switch res {
			case settleOK:
				ack("已标记完成 ✔ 已私信通知用户", false)
				editAdminDramaCard(ctx, deps, cq, req,
					fmt.Sprintf("✅ 已完成（%s，%s）",
						escapeHTML(req.ClaimedByName),
						time.Now().In(db.ChinaLoc).Format("01-02 15:04")))
			case settleAlready:
				ack("该工单已处理完成", true)
			case settleTaken:
				ack("该工单已被 "+req.ClaimedByName+" 接单，请勿重复处理", true)
			default:
				ack("处理失败，请稍后再试", true)
			}
		case "reject":
			// 先做状态守卫，再进入理由收集会话（真正驳回在收到理由后执行）
			var req db.DramaRequest
			if err := deps.DB.First(&req, uint(id)).Error; err != nil {
				ack("工单不存在", true)
				return
			}
			switch req.Status {
			case db.DramaStatusCompleted, db.DramaStatusRejected, db.DramaStatusCancelled:
				ack("该工单已处理完成", true)
				return
			case db.DramaStatusClaimed:
				if req.ClaimedBy == nil || *req.ClaimedBy != cq.From.ID {
					ack("该工单已被 "+req.ClaimedByName+" 接单，请勿重复处理", true)
					return
				}
			}
			ack("", false)
			deps.Sessions.Begin(cq.From.ID, sessAdminDramaRej)
			deps.Sessions.Advance(cq.From.ID, map[string]any{"req_id": id, "msg_id": messageIDOf(cq)})
			sendHTML(ctx, deps, cq.ChatID, fmt.Sprintf(
				"❌ 驳回求剧工单 #%d《%s》\n请直接回复<b>驳回理由</b>（将私信通知提交用户）。\n例如：链接失效 / 该剧已上架 / 内容不符合收录范围。\n\n回复 /cancel 可取消。",
				req.ID, escapeHTML(req.Title)))
		}
	case "next":
		// 取下一条待处理工单，发送操作卡片；没有则提示
		var req db.DramaRequest
		if err := deps.DB.Where("status = ?", db.DramaStatusPending).Order("id asc").First(&req).Error; err != nil {
			ack("没有待处理工单 ✔", false)
			return
		}
		ack("", false)
		sendDramaTicketCard(ctx, deps, cq.ChatID, &req)
	default:
		ack("", false)
	}
}

// messageIDOf 取出回调关联消息 ID（面板原地编辑用；0 表示无）。
func messageIDOf(cq *CallbackQuery) int {
	if cq == nil {
		return 0
	}
	return cq.MessageID
}

// handleAdminUserCallback 管理员对用户卡片的 inline 动作。
// 回调数据实际格式：admin:user:disable:1001
// 即 ParseCallbackData 拆出 domain="admin", action="user", args=["disable","1001"]，
// 这里把 action 与 args[0] 重组为子动作（user:disable / user:enable）。
func handleAdminUserCallback(ctx context.Context, deps *HandlerDeps, cq *CallbackQuery, action string, args []string) {
	if !deps.IsSuper(cq.From.ID) {
		return
	}
	// 重组：admin:user:disable:1001 → subAction="user:disable", args=["1001"]
	subAction := action
	if action == "user" && len(args) >= 1 {
		subAction = "user:" + args[0]
		args = args[1:]
	}
	if len(args) == 0 {
		return
	}
	tgID := parseInt64Safe(args[0])
	if tgID == 0 {
		return
	}
	var u db.User
	if err := deps.DB.Where("telegram_id = ?", tgID).First(&u).Error; err != nil {
		return
	}
	switch subAction {
	case "user:enable":
		if deps.JF != nil && u.JellyfinUserID != "" {
			if err := deps.JF.SetUserDisabled(ctx, u.JellyfinUserID, false); err != nil {
				sendText(ctx, deps, cq.ChatID, "启用 Jellyfin 账号失败："+err.Error())
				return
			}
		}
		if err := deps.DB.Model(&u).Updates(map[string]any{
			"status":         db.UserStatusActive,
			"is_suspended":   false,
			"suspend_reason": "",
		}).Error; err != nil {
			sendText(ctx, deps, cq.ChatID, "启用本地档案失败："+err.Error())
			return
		}
		_ = db.WriteAudit(deps.DB, cq.From.ID, "admin_user_enable", "user", itoa(int(u.TelegramID)), "启用用户（Jellyfin 同步）")
	case "user:disable":
		if deps.JF != nil && u.JellyfinUserID != "" {
			if err := deps.JF.SetUserDisabled(ctx, u.JellyfinUserID, true); err != nil {
				sendText(ctx, deps, cq.ChatID, "停用 Jellyfin 账号失败："+err.Error())
				return
			}
		}
		if err := deps.DB.Model(&u).Updates(map[string]any{
			"status":         db.UserStatusDisabled,
			"is_suspended":   true,
			"suspend_reason": "管理员停用",
		}).Error; err != nil {
			sendText(ctx, deps, cq.ChatID, "停用本地档案失败："+err.Error())
			return
		}
		_ = db.WriteAudit(deps.DB, cq.From.ID, "admin_user_disable", "user", itoa(int(u.TelegramID)), "停用用户（Jellyfin 同步）")
	}
}

// parseInt64Safe 解析宽字符/digits int64；失败或溢出返回 0。
func parseInt64Safe(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}
