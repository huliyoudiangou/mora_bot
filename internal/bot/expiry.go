package bot

import (
	"context"
	"time"

	"mora_bot/internal/db"
)

// ExpireSweep 一次到期巡检的结果（供日志/汇报）。
type ExpireSweep struct {
	Disabled []int64 // 本次被停用的 tg_id
	Restored []int64 // 本次被恢复的 tg_id
	Failed   int     // Jellyfin 调用失败数（本地状态未变，下次巡检重试）
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
		if err := setJFDisabled(ctx, deps, u.JellyfinUserID, true); err != nil {
			res.Failed++
			continue
		}
		r := deps.DB.Model(&db.User{}).
			Where("telegram_id = ? AND status = ?", u.TelegramID, db.UserStatusActive).
			Update("status", db.UserStatusExpired)
		if r.Error != nil || r.RowsAffected == 0 {
			// 状态已被并发改动（例如刚续期成功）：把 Jellyfin 侧改回启用，避免误停用。
			_ = setJFDisabled(ctx, deps, u.JellyfinUserID, false)
			continue
		}
		res.Disabled = append(res.Disabled, u.TelegramID)
		expireText := ""
		if u.ExpireAt != nil {
			expireText = "（" + u.ExpireAt.Format("2006-01-02") + "）"
		}
		notifyUserText(deps, u.TelegramID, "⛔ 你的 Jellyfin 账号已到期"+expireText+
			"，已暂停使用。\n用果果币续期：/shop buy 后再 /redeem，续期成功后自动恢复。")
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
		r := deps.DB.Model(&db.User{}).
			Where("telegram_id = ? AND status = ?", u.TelegramID, db.UserStatusExpired).
			Update("status", db.UserStatusActive)
		if r.Error != nil || r.RowsAffected == 0 {
			continue
		}
		res.Restored = append(res.Restored, u.TelegramID)
		notifyUserText(deps, u.TelegramID, "✅ 订阅已生效，你的 Jellyfin 账号已恢复使用。")
	}
	return res
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

// notifyUserText 私聊通知用户。用独立上下文发送：调用方的 ctx 可能已取消/超时，
// 但状态已经落地，通知必须尽力送达。
func notifyUserText(deps *HandlerDeps, tgID int64, text string) {
	if deps == nil || deps.Snd == nil {
		return
	}
	_ = deps.Snd.SendText(context.Background(), tgID, text)
}
