package bot

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"gorm.io/gorm"

	"mora_bot/internal/db"
)

var errQuotaExceeded = errors.New("邀请码兑换名额已满")

// exchangeQuotaMu 串行化全局积分兑换邀请码名额检查+创建，防止多用户同时通过
// Count 校验后都插入导致超配额。
var exchangeQuotaMu sync.Mutex

const (
	defaultShopPrice     = 100
	defaultShopDays      = 30
)

// cmdShop /shop：展示时长商店，用果果币换续期码（面板按钮）。
func (r *Router) cmdShop(ctx context.Context, msg *Message, args []string) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		return
	}
	if len(args) > 0 && args[0] == "buy" {
		r.cmdShopBuy(ctx, msg)
		return
	}
	if len(args) > 0 && args[0] == "invite" {
		r.cmdShopBuyInvite(ctx, msg)
		return
	}
	text, rows := shopPanel(deps, u)
	sendPanel(ctx, deps, msg.ChatID, 0, text, rows)
}

// cmdShopBuy /shop buy：扣果果币（禁止负余额） -> 同一事务内生成一张续期码落库。
func (r *Router) cmdShopBuy(ctx context.Context, msg *Message) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		return
	}
	price := renewalCodePrice(deps)
	days := deps.RenewalDays
	if days <= 0 {
		days = defaultShopDays
	}
	if price <= 0 {
		sendText(ctx, deps, msg.ChatID, "续期码暂未开放兑换。")
		return
	}
	if u.GuoGuo < price {
		sendText(ctx, deps, msg.ChatID, "果果币不足，先去 /signin 签到攒一波。")
		return
	}
	if deps.Pepper == "" {
		sendText(ctx, deps, msg.ChatID, "管理员未配置 SECURITY_PEPPER，卡密功能暂不可用。")
		return
	}

	var plain string
	var ve error
	deps.Lockers.WithUser(msg.From.ID, func() {
		ve = deps.DB.Transaction(func(tx *gorm.DB) error {
			// 1) 扣费（事务内，负余额被 AddPoints 拦下）
			if err := db.AddPointsTx(tx, u.TelegramID, -price, "shop_buy", "购买续期码", 0); err != nil {
				return err
			}
			// 2) 记录批次
			batch := db.CodeBatch{CodeType: "renewal", Count: 1, Days: days, Note: "shop buy", OperatorID: u.TelegramID}
			if err := tx.Create(&batch).Error; err != nil {
				return err
			}
			// 3) 生成卡密（pepper 加密后存 Hash+Enc）
			secret, err := generateRenewalSecret(deps.Pepper, days, u.TelegramID, "shop buy")
			if err != nil {
				return err
			}
			rec := db.RenewalCode{
				CodeHash:  secret.Hash,
				CodeEnc:   secret.Enc,
				Days:      days,
				BatchID:   batch.ID,
				Status:    db.CodeStatusUnused,
				CreatedBy: u.TelegramID,
			}
			if err := tx.Create(&rec).Error; err != nil {
				return err
			}
			plain = secret.Plain
			return nil
		})
	})
	if ve != nil {
		sendText(ctx, deps, msg.ChatID, "交易失败："+ve.Error())
		return
	}
	sendHTML(ctx, deps, msg.ChatID, fmt.Sprintf(
		"✅ 已扣除 %d 果果币，成功购买续期码（%d 天）：\n<code>%s</code>\n\n点击卡密即可复制；发送 /redeem %s 即可核销续期。",
		price, days, plain, plain))
}

// cmdShopBuyInvite /shop invite：扣果果币 -> 生成一张邀请码落库。
func (r *Router) cmdShopBuyInvite(ctx context.Context, msg *Message) {
	deps := r.deps
	u, err := getLocal(ctx, deps, msg.From)
	if err != nil {
		sendText(ctx, deps, msg.ChatID, "查询失败，请稍后再试。")
		return
	}
	if !exchangeInviteEnabled(deps) {
		sendText(ctx, deps, msg.ChatID, "积分兑换邀请码已关闭，请直接使用管理员发放的邀请码。")
		return
	}
	price := inviteCodePrice(deps)
	if price <= 0 {
		sendText(ctx, deps, msg.ChatID, "邀请码暂未开放兑换。")
		return
	}
	if remaining := exchangeInviteRemaining(deps); remaining == 0 {
		sendText(ctx, deps, msg.ChatID, "积分兑换邀请码名额已满（管理员设置了配额）。")
		return
	}
	if u.GuoGuo < price {
		sendText(ctx, deps, msg.ChatID, "果果币不足，先去 /signin 签到攒一波。")
		return
	}
	if deps.Pepper == "" {
		sendText(ctx, deps, msg.ChatID, "管理员未配置 SECURITY_PEPPER，卡密功能暂不可用。")
		return
	}

	var plain string
	var ve error
	exchangeQuotaMu.Lock()
	defer exchangeQuotaMu.Unlock()
	deps.Lockers.WithUser(msg.From.ID, func() {
		ve = deps.DB.Transaction(func(tx *gorm.DB) error {
			// 0) 配额再校验（全局互斥下核减，防多用户并发超发）
			if q := exchangeInviteQuota(deps); q > 0 {
				var used int64
				tx.Model(&db.InviteCode{}).Where("source = ?", "exchange").Count(&used)
				if used >= int64(q) {
					return errQuotaExceeded
				}
			}
			// 1) 扣费
			if err := db.AddPointsTx(tx, u.TelegramID, -price, "exchange_invite", "购买邀请码", 0); err != nil {
				return err
			}
			// 2) 批次
			batch := db.CodeBatch{CodeType: "invite", Count: 1, Days: 0, Note: "shop invite", OperatorID: u.TelegramID}
			if err := tx.Create(&batch).Error; err != nil {
				return err
			}
			// 3) 生成邀请码（事务内落库）——标记来源 exchange 用于配额统计
			secret, err := generateInviteSecret(deps.Pepper, u.TelegramID, "shop invite")
			if err != nil {
				return err
			}
			rec := db.InviteCode{
				CodeHash:  secret.Hash,
				CodeEnc:   secret.Enc,
				BatchID:   batch.ID,
				Status:    db.CodeStatusUnused,
				CreatedBy: u.TelegramID,
				Source:    "exchange",
			}
			if err := tx.Create(&rec).Error; err != nil {
				return err
			}
			plain = secret.Plain
			return nil
		})
	})
	if ve != nil {
		if ve == errQuotaExceeded {
			sendText(ctx, deps, msg.ChatID, "积分兑换邀请码名额已满（管理员设置了配额）。")
			return
		}
		sendText(ctx, deps, msg.ChatID, "交易失败："+ve.Error())
		return
	}
	sendHTML(ctx, deps, msg.ChatID, fmt.Sprintf(
		"✅ 已扣除 %d 果果币，成功购买邀请码：\n<code>%s</code>\n\n点击卡密即可复制；邀请好友注册时把此码给对方即可。",
		price, plain))
}
