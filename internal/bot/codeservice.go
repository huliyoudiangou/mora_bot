package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"mora_bot/internal/codes"
	"mora_bot/internal/db"
)

// errPepperMissing 卡密主密钥未配置。
var errPepperMissing = errors.New("SECURITY_PEPPER 未配置，卡密功能不可用")

// codesPerMessage 每条消息最多展示的卡密数：
// 每行 = 25 字符卡密 + 13 字符 <code> 标签 + 换行 ≈ 39，100 行加标题约 3960 < 4096 上限。
const codesPerMessage = 100

// sendCodesWithCopy 分批发送卡密明文：正文用等宽 <code> 展示，
// Telegram 客户端点击等宽文本即可复制到剪切板。
func sendCodesWithCopy(ctx context.Context, deps *HandlerDeps, chatID int64, title string, plains []string) {
	if deps == nil || deps.Snd == nil {
		return
	}
	total := len(plains)
	for start := 0; start < total; start += codesPerMessage {
		end := start + codesPerMessage
		if end > total {
			end = total
		}
		var b strings.Builder
		b.WriteString(title)
		if total > codesPerMessage {
			b.WriteString(fmt.Sprintf("（第 %d-%d 张 / 共 %d 张）\n", start+1, end, total))
		}
		b.WriteString("点击卡密即可复制\n\n")
		for _, p := range plains[start:end] {
			b.WriteString("<code>" + p + "</code>\n")
		}
		sendHTML(ctx, deps, chatID, b.String())
	}
}

// generateInviteCode 生成并落库一张邀请码（核对 pepper 存在）。
func generateInviteCode(deps *HandlerDeps, batchID uint, remark string) (string, error) {
	if deps == nil || deps.Pepper == "" {
		return "", errPepperMissing
	}
	batch, err := codes.GenerateCodeBatchInMemory(codes.CodeKindInvite, 1, deps.Pepper, 0, remark)
	if err != nil {
		return "", err
	}
	sc := batch[0]
	rec := db.InviteCode{
		CodeHash:  sc.Hash,
		CodeEnc:   sc.Enc,
		BatchID:   batchID,
		Status:    db.CodeStatusUnused,
		CreatedBy: 0,
	}
	if err := deps.DB.Create(&rec).Error; err != nil {
		return "", err
	}
	return sc.Plain, nil
}

// generateRenewalSecret 纯生成（不落库）一张续期码密钥，供调用方在事务内自行落库。
func generateRenewalSecret(pepper string, days int, owner int64, remark string) (*codes.CodeSecret, error) {
	if pepper == "" {
		return nil, errPepperMissing
	}
	if days <= 0 {
		days = 30
	}
	batch, err := codes.GenerateCodeBatchInMemory(codes.CodeKindRenewal, 1, pepper, days, remark)
	if err != nil {
		return nil, err
	}
	return &batch[0], nil
}

// generateInviteSecret 纯生成（不落库）一张邀请码密钥，供调用方在事务内自行落库。
func generateInviteSecret(pepper string, owner int64, remark string) (*codes.CodeSecret, error) {
	if pepper == "" {
		return nil, errPepperMissing
	}
	batch, err := codes.GenerateCodeBatchInMemory(codes.CodeKindInvite, 1, pepper, 0, remark)
	if err != nil {
		return nil, err
	}
	return &batch[0], nil
}

// generateRenewalCode 生成并落库一张续期码。
func generateRenewalCode(deps *HandlerDeps, batchID uint, days int, owner int64, remark string) (string, error) {
	if deps == nil || deps.Pepper == "" {
		return "", errPepperMissing
	}
	if days <= 0 {
		days = 30
	}
	batch, err := codes.GenerateCodeBatchInMemory(codes.CodeKindRenewal, 1, deps.Pepper, days, remark)
	if err != nil {
		return "", err
	}
	sc := batch[0]
	rec := db.RenewalCode{
		CodeHash:  sc.Hash,
		CodeEnc:   sc.Enc,
		Days:      days,
		BatchID:   batchID,
		Status:    db.CodeStatusUnused,
		CreatedBy: owner,
	}
	if err := deps.DB.Create(&rec).Error; err != nil {
		return "", err
	}
	return sc.Plain, nil
}

// redeemRenewalCode 用户核销续期码：把天数叠加到本地用户到期时间。
// 成功返回新增天数与新的到期时间。
func redeemRenewalCode(deps *HandlerDeps, user *db.User, plainCode string) (int, *time.Time, error) {
	if deps == nil || deps.DB == nil || user == nil {
		return 0, nil, errors.New("参数错误")
	}
	clean, err := codes.ValidateCodeFormat(plainCode)
	if err != nil {
		return 0, nil, err
	}
	hash, err := codes.HashCode(clean, deps.Pepper)
	if err != nil {
		return 0, nil, err
	}
	var rec db.RenewalCode
	err = deps.DB.Where("code_hash = ? AND status = ?", hash, db.CodeStatusUnused).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil, codes.ErrCodeNotFound
	}
	if err != nil {
		return 0, nil, err
	}
	if rec.Status != db.CodeStatusUnused {
		return 0, nil, codes.ErrCodeUsed
	}

	var newExpire *time.Time
	days := rec.Days
	if days <= 0 {
		days = 30
	}
	now := time.Now()
	if user.IsPermanent {
		// 白名单账号不再叠加，但消耗卡密。
	} else if user.ExpireAt == nil || user.ExpireAt.Before(now) {
		t := now.AddDate(0, 0, days)
		newExpire = &t
	} else {
		t := user.ExpireAt.AddDate(0, 0, days)
		newExpire = &t
	}

	err = deps.DB.Transaction(func(tx *gorm.DB) error {
		// 幂等：仅当仍为 unused 才消费
		res := tx.Model(&db.RenewalCode{}).
			Where("id = ? AND status = ?", rec.ID, db.CodeStatusUnused).
			Updates(map[string]any{
				"status":  db.CodeStatusUsed,
				"used_by": user.TelegramID,
				"used_at": time.Now(),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return codes.ErrCodeUsed
		}
		if newExpire != nil {
			updates := map[string]any{"expire_at": *newExpire}
			if user.IsPermanent {
				updates["is_permanent"] = false
			}
			if err := tx.Model(&db.User{}).Where("telegram_id = ?", user.TelegramID).Updates(updates).Error; err != nil {
				return err
			}
		}
		// 续期审计
		return tx.Create(&db.RenewalRecord{
			UserID:     user.TelegramID,
			CodeID:     rec.ID,
			CodeHash:   rec.CodeHash,
			Days:       days,
			PrevExpire: user.ExpireAt,
			NewExpire:  newExpire,
		}).Error
	})
	if err != nil {
		return 0, nil, err
	}
	return days, newExpire, nil
}
