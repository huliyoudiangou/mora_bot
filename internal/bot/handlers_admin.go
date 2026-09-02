package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"mora_bot/internal/db"
)

// cmdAdmin /admin 管理面板（仅超管）。
func (r *Router) cmdAdmin(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		return
	}
	if len(args) == 0 {
		u, err := getLocal(ctx, deps, msg.From)
		if err != nil {
			sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
			return
		}
		text, rows := adminPanel(deps, u)
		sendPanel(ctx, deps, msg.ChatID, 0, text, rows)
		return
	}
	switch args[0] {
	case "stats":
		r.handleAdminStats(ctx, msg)
	case "gencode":
		r.handleAdminGenCode(ctx, msg, args[1:])
	case "addpoints":
		r.handleAdminAddPoints(ctx, msg, args[1:])
	case "user":
		r.handleAdminUser(ctx, msg, args[1:])
	default:
		sendText(ctx, deps, msg.ChatID, "不认识的 /admin 子命令。")
	}
}

// handleAdminStats /admin stats 汇总。
func (r *Router) handleAdminStats(ctx context.Context, msg *Message) {
	deps := r.deps
	var total, bound, pointsUsers int64
	deps.DB.Model(&db.User{}).Count(&total)
	deps.DB.Model(&db.User{}).Where("jellyfin_user_id <> ''").Count(&bound)
	deps.DB.Model(&db.User{}).Where("guo_guo > 0").Count(&pointsUsers)
	var inviteUnused, renewalUnused int64
	deps.DB.Model(&db.InviteCode{}).Where("used_by IS NULL").Count(&inviteUnused)
	deps.DB.Model(&db.RenewalCode{}).Where("used_by IS NULL").Count(&renewalUnused)
	sendHTML(ctx, deps, msg.ChatID, fmt.Sprintf(
		"<b>全局统计</b>\n\n用户：%d（已绑定 %d / 有余额 %d）\n邀请码未用：%d\n续期码未用：%d",
		total, bound, pointsUsers, inviteUnused, renewalUnused))
}

// handleAdminGenCode /admin gencode <数量> [invite|renewal] [天数]
// 例如：/admin gencode 5            → 生成 5 张邀请码
//       /admin gencode 3 renewal 30 → 生成 3 张 30 天续期码
func (r *Router) handleAdminGenCode(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	if len(args) == 0 {
		sendText(ctx, deps, msg.ChatID, "用法：/admin gencode <数量> [invite|renewal] [天数]")
		return
	}
	n := resolveUserID(args, 0)
	if n <= 0 || n > 200 {
		sendText(ctx, deps, msg.ChatID, "请提供 1-200 之间的数量。")
		return
	}
	kind := "invite"
	days := 0
	if len(args) >= 2 {
		switch args[1] {
		case "invite", "inv":
			kind = "invite"
		case "renewal", "renew", "ren":
			kind = "renewal"
			if len(args) >= 3 {
				days = int(resolveUserID(args, 2))
			}
			if days <= 0 {
				days = deps.RenewalDays
			}
			if days <= 0 {
				days = 30
			}
		default:
			sendText(ctx, deps, msg.ChatID, "类型只能是 invite 或 renewal。")
			return
		}
	}
	if deps.Pepper == "" {
		sendText(ctx, deps, msg.ChatID, "管理员未配置 SECURITY_PEPPER，卡密功能不可用。")
		return
	}
	generateCodeBatchAndSend(ctx, r.deps, msg, kind, int(n), days)
}

// generateCodeBatchAndSend 建批次、逐张生成卡密并落库，然后分批发给管理员。
func generateCodeBatchAndSend(ctx context.Context, deps *HandlerDeps, msg *Message, kind string, count, days int) {
	// 建批次
	batch := db.CodeBatch{CodeType: kind, Count: count, Days: days, Note: "admin gencode", OperatorID: msg.From.ID}
	if err := deps.DB.Create(&batch).Error; err != nil {
		sendText(ctx, deps, msg.ChatID, "创建批次失败。")
		return
	}
	_ = db.WriteAudit(deps.DB, msg.From.ID, "admin_gencode", "code_batch", itoa(int(batch.ID)), fmt.Sprintf("生成 %s %d 张，%d 天", kind, count, days))
	// 生成卡密并落库
	plains := make([]string, 0, count)
	for i := 0; i < count; i++ {
		var plain string
		var err error
		if kind == "renewal" {
			plain, err = generateRenewalCode(deps, batch.ID, days, 0, "admin gencode")
		} else {
			plain, err = generateInviteCode(deps, batch.ID, "admin gencode")
		}
		if err != nil {
			sendText(ctx, deps, msg.ChatID, "生成卡密失败（第 "+itoa(i+1)+" 张）："+err.Error())
			return
		}
		plains = append(plains, plain)
	}
	// 发送：正文 <code> 展示 + 每张码一个点击复制按钮
	title := "🎟 邀请码（批次 #" + itoa(int(batch.ID)) + "）\n"
	if kind == "renewal" {
		title = "⏳ 续期码（批次 #" + itoa(int(batch.ID)) + "，" + itoa(days) + " 天）\n"
	}
	sendCodesWithCopy(ctx, deps, msg.ChatID, title, plains)
}

// handleAdminGenInviteStep 面板「生成邀请码」向导：收数量 → 生成。
func (r *Router) handleAdminGenInviteStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if strings.EqualFold(strings.TrimSpace(msg.Text), "/cancel") {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消生成邀请码。")
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(msg.Text))
	if err != nil || n <= 0 || n > 200 {
		sendText(ctx, deps, msg.ChatID, "请输入 1-200 之间的整数数量，例如 10。")
		return
	}
	if deps.Pepper == "" {
		sendText(ctx, deps, msg.ChatID, "管理员未配置 SECURITY_PEPPER，卡密功能不可用。")
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	deps.Sessions.Clear(msg.From.ID)
	generateCodeBatchAndSend(ctx, deps, msg, "invite", n, 0)
}

// handleAdminGenRenewStep 面板「生成续期码」向导：
// Step 1 收天数 → Step 2 收数量 → 生成 R<天数>-XXXX... 续期码。
func (r *Router) handleAdminGenRenewStep(ctx context.Context, msg *Message) {
	deps := r.deps
	if !r.ensureAdmin(ctx, msg) {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if strings.EqualFold(strings.TrimSpace(msg.Text), "/cancel") {
		deps.Sessions.Clear(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "已取消生成续期码。")
		return
	}
	if deps.Pepper == "" {
		sendText(ctx, deps, msg.ChatID, "管理员未配置 SECURITY_PEPPER，卡密功能不可用。")
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	sess := deps.Sessions.Current(msg.From.ID)
	if sess == nil {
		deps.Sessions.Clear(msg.From.ID)
		return
	}
	if sess.Step == 0 {
		// 第 1 步：收天数
		days, err := strconv.Atoi(strings.TrimSpace(msg.Text))
		if err != nil || days <= 0 || days > 3650 {
			sendText(ctx, deps, msg.ChatID, "请输入 1-3650 之间的天数，例如 30。")
			return
		}
		s2 := deps.Sessions.Advance(msg.From.ID, map[string]any{"days": days})
		s2.Step = 1
		sendText(ctx, deps, msg.ChatID, "请输入要生成的续期码数量（1-200），例如 10。")
		return
	}
	// 第 2 步：收数量
	n, err := strconv.Atoi(strings.TrimSpace(msg.Text))
	if err != nil || n <= 0 || n > 200 {
		sendText(ctx, deps, msg.ChatID, "请输入 1-200 之间的整数数量，例如 10。")
		return
	}
	days, _ := sess.Data["days"].(int)
	if days <= 0 {
		days = 30
	}
	deps.Sessions.Clear(msg.From.ID)
	generateCodeBatchAndSend(ctx, deps, msg, "renewal", n, days)
}

// handleAdminAddPoints /admin addpoints <tg_id> <delta>
func (r *Router) handleAdminAddPoints(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	tgID := resolveUserID(args, 0)
	delta := resolveUserID(args, 1)
	if tgID == 0 || delta == 0 {
		sendText(ctx, deps, msg.ChatID, "用法：/admin addpoints <tg_id> <数量>")
		return
	}
	var u db.User
	if err := deps.DB.Where("telegram_id = ?", tgID).First(&u).Error; err != nil {
		sendText(ctx, deps, msg.ChatID, "未找到该用户。")
		return
	}
	if err := db.AddPoints(deps.DB, u.TelegramID, int(delta), "admin_adjust", "AdminCmd", 0); err != nil {
		sendText(ctx, deps, msg.ChatID, "调整失败："+err.Error())
		return
	}
	_ = db.WriteAudit(deps.DB, msg.From.ID, "admin_addpoints", "user", itoa64s(tgID), fmt.Sprintf("调整果果币 %d（delta=%d）", delta, delta))
	sendText(ctx, deps, msg.ChatID, fmt.Sprintf("✅ 已为用户 tg=%d 调整 %d 果果币。", tgID, delta))
}

// handleAdminUser /admin user <tg_id>
func (r *Router) handleAdminUser(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	tgID := resolveUserID(args, 0)
	if tgID == 0 {
		sendText(ctx, deps, msg.ChatID, "用法：/admin user <tg_id>")
		return
	}
	var u db.User
	if err := deps.DB.Where("telegram_id = ?", tgID).First(&u).Error; err != nil {
		sendText(ctx, deps, msg.ChatID, "未找到该用户。")
		return
	}
	isAdmin := deps.IsSuper != nil && deps.IsSuper(u.TelegramID)
	sendHTML(ctx, deps, msg.ChatID, fmt.Sprintf(
		"👤 tg=%d\n用户名：%s %s\nJF：%s（%s）\n果果币：%d\n状态：%s\n管理员：%v",
		u.TelegramID, escapeHTML(u.TgUsername), escapeHTML(u.FirstName), escapeHTML(u.JellyfinUsername), escapeHTML(u.JellyfinUserID), u.GuoGuo, u.Status, isAdmin))
}
