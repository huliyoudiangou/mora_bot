package bot

import (
	"context"
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
// 本地状态变更，留待下次巡检重试；管理员手动停用（inactive/disabled）与已注销
// （deleted）的账号一概不碰。
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
			// 状态已被并发改动（例如刚续期成功）：把 Jellyfin 侧改回启用，避免误停用。
			_ = setJFDisabled(ctx, deps, u.JellyfinUserID, false)
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
	deps.DB.Model(&db.User{}).
		Where("telegram_id = ? AND status = ?", tgID, db.UserStatusExpired).
		Update("status", db.UserStatusActive)
}

// setJFDisabled 同步 Jellyfin 侧启用/禁用；未配置 Jellyfin 或用户未绑定 Jellyfin 时
// 视为无需同步（本地状态照常流转）。
func setJFDisabled(ctx context.Context, deps *HandlerDeps, jfUserID string, disabled bool) error {
	if deps.JF == nil || jfUserID == "" {
		return nil
	}
	return deps.JF.SetUserDisabled(ctx, jfUserID, disabled)
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
