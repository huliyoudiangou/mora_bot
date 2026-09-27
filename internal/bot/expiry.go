package bot

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"mora_bot/internal/db"
)

// ExpireSweep 一次到期巡检的结果（供日志/汇报）。
type ExpireSweep struct {
	Disabled  []int64 // 本次被停用的 tg_id
	Restored  []int64 // 本次被恢复的 tg_id
	Failed    int     // Jellyfin 调用失败数（本地状态未变，下次巡检重试）
	LoggedOut int     // 本次成功断开在线会话的账号数
}

// SweepExpiredAccounts 巡检一次订阅到期状态：
//  1. 停用：非白名单、expire_at 已过、状态仍为 active 的账号 —— Jellyfin 侧禁用 +
//     本地 status=expired，并私聊告知用户（续期后自动恢复）；
//  2. 恢复：状态为 expired 但已续期（expire_at 在未来）或已成为白名单的账号 ——
//     Jellyfin 侧启用 + 本地 status=active，并私聊告知用户。
//
// 幂等性：状态转换（active↔expired）只发生一次，通知因此天然不重复；条件更新
// （WHERE status = 旧值）保证与并发续期/提权不打架。Jellyfin 调用失败的用户不做
// 本地状态变更，留待下次巡检重试；管理员手动停用（inactive）与已注销
// （deleted）的账号一概不碰 —— inactive 只能由管理员解封，巡检无权恢复。
func SweepExpiredAccounts(ctx context.Context, deps *HandlerDeps) ExpireSweep {
	var res ExpireSweep
	if deps == nil || deps.DB == nil {
		return res
	}
	now := time.Now()

	var due []db.User
	if err := deps.DB.
		Where("is_permanent = ? AND expire_at IS NOT NULL AND expire_at <= ? AND status = ?",
			false, now, db.UserStatusActive).
		Find(&due).Error; err != nil {
		return res
	}
	for _, u := range due {
		// 竞态防护：名单是巡检开始时读出来的，之后逐个处理要调用 Jellyfin（每次最多 15s）。
		// 用户完全可能在这中间刚好续期/重新注册（expire_at 被推到未来）或被加白名单 ——
		// 此时若照旧停用，会把他禁用并踢掉全部在线设备，等他付费后才发现状态变了再改回，
		// 白挨一次掉线。真正动手前重新读一次最新状态，不再满足条件就跳过。
		if !stillDueForDisable(deps, u.TelegramID, now) {
			continue
		}
		if err := setJFDisabled(ctx, deps, u.JellyfinUserID, true); err != nil {
			res.Failed++
			continue
		}
		// 禁用只拦新登录：已签发的 token 仍可继续播放，必须同时踢掉在线会话，
		// 停用才算真正落地。踢会话失败不影响停用本身（账号已禁用）。
		kicked := false
		if k, err := kickSessions(ctx, deps, u.JellyfinUserID); err == nil && k {
			kicked = true
		}
		// 条件更新带上与查询一致的完整条件：即使上面复核之后又发生并发改动，
		// RowsAffected==0 也会让这次停用作废（并把 Jellyfin 侧改回启用）。
		r := deps.DB.Model(&db.User{}).
			Where("telegram_id = ? AND status = ? AND is_permanent = ? AND expire_at IS NOT NULL AND expire_at <= ?",
				u.TelegramID, db.UserStatusActive, false, now).
			Update("status", db.UserStatusExpired)
		if r.Error != nil || r.RowsAffected == 0 {
			// 状态已被并发改动（刚续期成功 / 刚被管理员停用 / 已注销…）。这里**不能**盲目
			// "改回启用"：若期间管理员手动停用了该账号，盲目启用等于把停用击穿。
			// 按当前本地状态重新对齐才是唯一安全的回滚。
			if err := reconcileJFDisabled(ctx, deps, u.TelegramID, u.JellyfinUserID); err != nil {
				res.Failed++
			}
			continue
		}
		res.Disabled = append(res.Disabled, u.TelegramID)
		// 只在停用真正落定后计数：上面被回滚掉的那次踢会话不算进统计。
		if kicked {
			res.LoggedOut++
		}
		notifyUserText(deps, u.TelegramID, expireDisabledNotice(u))
	}

	var back []db.User
	if err := deps.DB.
		Where("status = ? AND (is_permanent = ? OR (expire_at IS NOT NULL AND expire_at > ?))",
			db.UserStatusExpired, true, now).
		Find(&back).Error; err != nil {
		return res
	}
	for _, u := range back {
		if err := setJFDisabled(ctx, deps, u.JellyfinUserID, false); err != nil {
			res.Failed++
			continue
		}
		// 同样带全条件：期间又被改回 expired 之外的状态（如管理员处置）时不覆盖。
		r := deps.DB.Model(&db.User{}).
			Where("telegram_id = ? AND status = ? AND (is_permanent = ? OR (expire_at IS NOT NULL AND expire_at > ?))",
				u.TelegramID, db.UserStatusExpired, true, now).
			Update("status", db.UserStatusActive)
		if r.Error != nil || r.RowsAffected == 0 {
			// 期间状态被改动（管理员停用 / 重新注册换了账号 / 已注销）：刚写下的"启用"
			// 必须按最新本地状态收回，否则账号在 Jellyfin 侧可用、本地却不可用。
			if err := reconcileJFDisabled(ctx, deps, u.TelegramID, u.JellyfinUserID); err != nil {
				res.Failed++
			}
			continue
		}
		res.Restored = append(res.Restored, u.TelegramID)
		notifyUserText(deps, u.TelegramID, expireRestoredNotice(u))
	}
	return res
}

// stillDueForDisable 复核用户此刻是否仍处于"应当停用"的状态：非白名单、仍活跃、
// 且到期时间存在并已过。读不到记录（已注销等）一律视为无需处理。
func stillDueForDisable(deps *HandlerDeps, tgID int64, now time.Time) bool {
	var cur struct {
		IsPermanent bool
		ExpireAt    *time.Time
		Status      string
	}
	if err := deps.DB.Model(&db.User{}).
		Select("is_permanent", "expire_at", "status").
		Where("telegram_id = ?", tgID).Scan(&cur).Error; err != nil {
		return false // 查询失败不冒险动手，留给下次巡检
	}
	return !cur.IsPermanent && cur.Status == db.UserStatusActive &&
		cur.ExpireAt != nil && !cur.ExpireAt.After(now)
}

// expireDisabledNotice 到期停用通知。
// 注意：用户可能已用 /account unbind 解绑（本地档案保留 expire_at 但清空了 Jellyfin 绑定），
// 此时 bot 并没有停用任何账号 —— 不能声称"你的 Jellyfin 账号已暂停使用"，那是虚假陈述。
func expireDisabledNotice(u db.User) string {
	date := ""
	if u.ExpireAt != nil {
		date = "（" + u.ExpireAt.Format("2006-01-02") + "）"
	}
	if u.JellyfinUserID == "" {
		return "⛔ 你的订阅已到期" + date + "。\n用果果币续期：/shop buy 后再 /redeem，续期成功后即可继续使用。"
	}
	return "⛔ 你的 Jellyfin 账号已到期" + date + "，已暂停使用。\n用果果币续期：/shop buy 后再 /redeem，续期成功后自动恢复。"
}

// expireRestoredNotice 到期恢复通知（同样区分是否绑定 Jellyfin）。
func expireRestoredNotice(u db.User) string {
	if u.JellyfinUserID == "" {
		return "✅ 订阅已生效，你可以继续使用服务了。"
	}
	return "✅ 订阅已生效，你的 Jellyfin 账号已恢复使用。"
}

// restoreIfExpired 就地恢复"被到期停用"的账号：续期成功、白名单提权后立即调用，
// 不必等次日巡检。Jellyfin 侧启用失败则保持 expired 状态，由巡检兜底重试。
func restoreIfExpired(ctx context.Context, deps *HandlerDeps, tgID int64) {
	if deps == nil || deps.DB == nil {
		return
	}
	var u db.User
	if err := deps.DB.
		Where("telegram_id = ? AND status = ?", tgID, db.UserStatusExpired).
		First(&u).Error; err != nil {
		return
	}
	if err := setJFDisabled(ctx, deps, u.JellyfinUserID, false); err != nil {
		return
	}
	r := deps.DB.Model(&db.User{}).
		Where("telegram_id = ? AND status = ?", tgID, db.UserStatusExpired).
		Update("status", db.UserStatusActive)
	if r.Error != nil || r.RowsAffected == 0 {
		// 期间状态被改动（例如管理员刚停用该账号）：收回刚写下的"启用"，
		// 否则账号在 Jellyfin 侧可用、本地却是停用/已注销。
		_ = reconcileJFDisabled(ctx, deps, tgID, u.JellyfinUserID)
	}
}

// setJFDisabled 同步 Jellyfin 侧启用/禁用；未配置 Jellyfin 或用户未绑定 Jellyfin 时
// 视为无需同步（本地状态照常流转）。
//
// 持 libApplyMu：媒体库批量应用/白名单覆盖同样走"读当前 Policy → 改几个字段 → 写回"，
// 与这里的禁用位写回是同一份 Policy。不互斥的话，批量应用先读后写会把刚刚写下的
// IsDisabled 覆盖回旧值 —— 被停用/到期的账号凭空恢复可登录。
func setJFDisabled(ctx context.Context, deps *HandlerDeps, jfUserID string, disabled bool) error {
	if deps.JF == nil || jfUserID == "" {
		return nil
	}
	libApplyMu.Lock()
	defer libApplyMu.Unlock()
	return deps.JF.SetUserDisabled(ctx, jfUserID, disabled)
}

// reconcileJFDisabled 按"当前本地状态"重新对齐 Jellyfin 侧的启用/禁用，返回是否写成功。
//
// 用于条件更新落空（RowsAffected==0）后的回滚。此时本地状态已被并发改动，把 Jellyfin
// 盲目翻到另一侧是错的：
//   - 巡检刚禁用账号、管理员随即手动停用 → 条件更新失败后若"回滚"成启用，被停用的
//     账号反而能登录，停用被击穿；
//   - 巡检刚启用账号（用户已续期）、管理员随即停用 → 同样是"启用"留在远端。
//
// 唯一正确的做法是重新读一次本地状态，按它算出应有的禁用位再写回（幂等，不依赖先后）。
//
// jfID 是本次操作的账号：若本地绑定已换成别的账号（重新注册/重新绑定），说明这个旧
// 账号不再对应用户的当前订阅，一律禁用，绝不因换绑而让旧账号"复活"。
func reconcileJFDisabled(ctx context.Context, deps *HandlerDeps, tgID int64, jfID string) error {
	if deps == nil || deps.DB == nil || jfID == "" {
		return nil
	}
	var cur struct {
		Status         string     `gorm:"column:status"`
		IsPermanent    bool       `gorm:"column:is_permanent"`
		ExpireAt       *time.Time `gorm:"column:expire_at"`
		JellyfinUserID string     `gorm:"column:jellyfin_user_id"`
	}
	if err := deps.DB.Model(&db.User{}).
		Select("status", "is_permanent", "expire_at", "jellyfin_user_id").
		Where("telegram_id = ?", tgID).Scan(&cur).Error; err != nil {
		return err
	}
	if cur.JellyfinUserID != jfID {
		return setJFDisabled(ctx, deps, jfID, true)
	}
	// 只有"活跃且订阅仍有效（白名单或未到期）"才应处于启用状态，其余
	// （expired / inactive / deleted）一律禁用。
	enable := cur.Status == db.UserStatusActive &&
		(cur.IsPermanent || (cur.ExpireAt != nil && cur.ExpireAt.After(time.Now())))
	return setJFDisabled(ctx, deps, jfID, !enable)
}

// kickSessions 踢掉该用户在 Jellyfin 的全部在线会话（返回是否确实清理了会话）。
// 未配置 Jellyfin 或用户未绑定时视为无需处理。
func kickSessions(ctx context.Context, deps *HandlerDeps, jfUserID string) (bool, error) {
	if deps.JF == nil || jfUserID == "" {
		return false, nil
	}
	n, err := deps.JF.LogoutAllDevices(ctx, jfUserID)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// shouldResetExpireForNewCycle 重新注册时是否要开启新的订阅周期：
// 从未开通（expire_at 为空）或已过期（此前被到期停用）都要重置，否则用户会带着
// 旧的过期时间上线，次日巡检立刻又把他停用（白付邀请码）。白名单账号无需处理。
func shouldResetExpireForNewCycle(u *db.User, now time.Time) bool {
	if u == nil || u.IsPermanent {
		return false
	}
	return u.ExpireAt == nil || u.ExpireAt.Before(now)
}

// resetExpireForNewCycle 按 shouldResetExpireForNewCycle 的判定重置订阅周期：
// NEW_ACCOUNT_VALID_DAYS>0 给一个新周期，=0（永久）则清空旧到期时间。
func resetExpireForNewCycle(deps *HandlerDeps, u *db.User) error {
	if deps == nil || deps.DB == nil || !shouldResetExpireForNewCycle(u, time.Now()) {
		return nil
	}
	var newExpire *time.Time
	if deps.NewAccountValidDays > 0 {
		t := time.Now().AddDate(0, 0, deps.NewAccountValidDays)
		newExpire = &t
	}
	return deps.DB.Model(u).Updates(map[string]any{"expire_at": newExpire}).Error
}

// bindExpiredText /bind 被订阅到期阻断时的提示（入口与写入前共用）。
const bindExpiredText = "你的订阅已到期（Jellyfin 账号处于停用状态），请先续期：/shop buy 后再 /redeem。"

// releaseExpiredBindingForReregister 到期用户重新注册前清理旧绑定。
//
// 只有旧账号是 bot 创建的（bind_type=registered）才删除远端账号：那种账号本就是本 bot
// 发放的、此刻又处于到期停用状态，留着只会占用 Jellyfin 用户名（用户重新注册往往想用回
// 同一个名字，同名会被判为"用户名已占用"）并变成孤儿。用户自己绑定的既有账号
// （bind_type=existing）绝不删除，本地关联交给注册成功后的写入覆盖。
//
// 返回 true 表示远端旧账号已删除、本地绑定已同步清空（与远端保持一致）。
// 远端删除失败时返回错误：此时不能当作已清理继续注册，否则同名创建必然失败。
func releaseExpiredBindingForReregister(ctx context.Context, deps *HandlerDeps, tgID int64) (bool, error) {
	if deps == nil || deps.DB == nil {
		return false, db.ErrNilDB
	}
	var u db.User
	if err := deps.DB.Where("telegram_id = ?", tgID).First(&u).Error; err != nil {
		return false, nil // 无档案：按全新注册处理
	}
	if u.JellyfinUserID == "" || u.BindType != db.BindTypeRegistered || deps.JF == nil {
		return false, nil
	}
	// 纵深守卫：只有"已到期"的账号才该被重新注册流程清理。入口已按同一判定拦过，
	// 这里再判一次，避免该函数被别处误用后删掉正常在用账号。
	if !expiryBlockedForBind(&u, time.Now()) {
		return false, nil
	}
	if err := deps.JF.DeleteUser(ctx, u.JellyfinUserID); err != nil {
		return false, err
	}
	if err := deps.DB.Model(&db.User{}).Where("telegram_id = ?", tgID).
		Updates(map[string]any{
			"jellyfin_user_id":  "",
			"jellyfin_username": "",
			"bind_type":         "",
		}).Error; err != nil {
		return false, err
	}
	return true, nil
}

// expiryBlockedForBind /bind 是否被订阅到期阻断。
// /bind 不需要邀请码，若放行则任何到期用户都能靠重新绑定免费重置订阅；且此时
// Jellyfin 侧账号本就处于停用状态，绑定也没有意义 —— 一律先续期。
//
// 除到期时间外还显式判 status=expired：状态机是"到期"的权威记录，万一历史数据里
// 出现 expired 但 expire_at 为空的行，也不能靠重新绑定把它放行。
func expiryBlockedForBind(u *db.User, now time.Time) bool {
	if u == nil || u.IsPermanent {
		return false
	}
	if u.Status == db.UserStatusExpired {
		return true
	}
	return u.ExpireAt != nil && u.ExpireAt.Before(now)
}

// notifyUserText 私聊通知用户。用独立上下文发送：调用方的 ctx 可能已取消/超时，
// 但状态已经落地，通知必须尽力送达。
func notifyUserText(deps *HandlerDeps, tgID int64, text string) {
	if deps == nil || deps.Snd == nil {
		return
	}
	_ = deps.Snd.SendText(context.Background(), tgID, text)
}

// ---------------------------------------------------------------------------
// 巡检调度：互斥执行 + 结果汇报
// ---------------------------------------------------------------------------

// sweepRunning 巡检互斥标志。自动巡检（每日定时 / 启动补跑）与管理员手动触发
// 共用，保证同一时刻只有一个巡检在跑 —— 重复跑不但白耗 Jellyfin 调用，
// 还会让同一个用户收到两条重复通知。
var sweepRunning atomic.Bool

// RunExpirySweep 执行一次到期巡检（自动/手动共用入口）。已有巡检在执行时
// 立即返回 ok=false，不阻塞调用方。调用方决定如何汇报（手动触发私聊发起者，
// 自动巡检走 NotifyAdminsSweepResult）。
func RunExpirySweep(ctx context.Context, deps *HandlerDeps) (ExpireSweep, bool) {
	if !sweepRunning.CompareAndSwap(false, true) {
		return ExpireSweep{}, false
	}
	defer sweepRunning.Store(false)
	return SweepExpiredAccounts(ctx, deps), true
}

// sweepListMax 汇报里名单最多列出的人数（防止消息超长被 Telegram 拒发）。
const sweepListMax = 20

// expirySweepReport 巡检结果文案（管理员视角）。
func expirySweepReport(res ExpireSweep) string {
	var b strings.Builder
	b.WriteString("· 停用：" + itoa64s(int64(len(res.Disabled))) + " 人\n")
	b.WriteString("· 恢复：" + itoa64s(int64(len(res.Restored))) + " 人\n")
	b.WriteString("· 断开在线会话：" + itoa64s(int64(res.LoggedOut)) + " 个\n")
	if res.Failed > 0 {
		b.WriteString("· ⚠️ 失败：" + itoa64s(int64(res.Failed)) +
			" 人（Jellyfin 调用失败，本地状态未变，下次巡检自动重试）\n")
	}
	if s := previewIDs("停用名单", res.Disabled); s != "" {
		b.WriteString(s + "\n")
	}
	if s := previewIDs("恢复名单", res.Restored); s != "" {
		b.WriteString(s + "\n")
	}
	return b.String()
}

// previewIDs 把 tg_id 列表渲染成一行明细（截前 sweepListMax 个）。
func previewIDs(label string, ids []int64) string {
	if len(ids) == 0 {
		return ""
	}
	n := len(ids)
	if n > sweepListMax {
		n = sweepListMax
	}
	parts := make([]string, 0, n)
	for _, id := range ids[:n] {
		parts = append(parts, itoa64s(id))
	}
	s := label + "：" + strings.Join(parts, ", ")
	if len(ids) > sweepListMax {
		s += " …（共 " + itoa64s(int64(len(ids))) + " 人）"
	}
	return s
}

// NotifyAdminsSweepResult 把自动巡检结果私聊汇报给全体管理员。
//
// 只在"有动静"（真的停用/恢复/失败）时发送 —— 无事发生就静默，避免每天一条
// "无变化"的骚扰。刻意**不做时间节流**：节流会连正常的每日汇报一起吞掉，
// 而"有动静才推"本身已经把绝大多数噪音挡掉了。
func NotifyAdminsSweepResult(ctx context.Context, deps *HandlerDeps, res ExpireSweep) {
	if deps == nil || deps.Snd == nil || len(deps.SuperAdminIDs) == 0 {
		return
	}
	if len(res.Disabled) == 0 && len(res.Restored) == 0 && res.Failed == 0 {
		return // 无事发生，不打扰
	}
	text := "🔄 <b>到期巡检完成</b>\n" + expirySweepReport(res)
	for _, id := range deps.SuperAdminIDs {
		_ = deps.Snd.SendTextHTML(ctx, id, text)
	}
}

// manualSweepTimeout 手动巡检的兜底超时。逐个用户调用 Jellyfin（每人最多 2 次、
// 每次 15s 超时），用户多时耗时可达分钟级，必须异步执行并给足超时。
const manualSweepTimeout = 30 * time.Minute

// StartManualExpirySweep 管理员手动触发一次巡检：先回执，再异步跑，
// 跑完私聊汇报发起者（无论有无变化 —— 是他主动要求的，必须给结果）。
func StartManualExpirySweep(ctx context.Context, deps *HandlerDeps, replyChatID, adminID int64) {
	sendText(ctx, deps, replyChatID, "🔄 已开始巡检，完成后会私聊汇报给你。")
	go func() {
		// 独立上下文：回调/命令处理返回后 ctx 会被取消，不能用于长任务。
		rctx, cancel := context.WithTimeout(context.Background(), manualSweepTimeout)
		defer cancel()
		res, ok := RunExpirySweep(rctx, deps)
		if !ok {
			sendDetachedText(deps, adminID, "⏳ 已有巡检正在执行，请稍候再试。")
			return
		}
		_ = db.WriteAudit(deps.DB, adminID, "admin_expiry_sweep", "system", "",
			"手动巡检：停用 "+itoa64s(int64(len(res.Disabled)))+
				"，恢复 "+itoa64s(int64(len(res.Restored)))+
				"，失败 "+itoa64s(int64(res.Failed)))
		sendDetachedHTML(deps, adminID, "🔄 <b>手动巡检完成</b>\n"+expirySweepReport(res))
	}()
}
