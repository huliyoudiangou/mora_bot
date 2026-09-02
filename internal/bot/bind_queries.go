package bot

import (
	"errors"

	"mora_bot/internal/codes"
	"mora_bot/internal/db"

	"gorm.io/gorm"
)

// dbFindInviteCode 按明文卡密找未使用的邀请码。
// 使用与生成端（codes.HashCode）完全相同的 HMAC 算法，否则永远查不到。
func dbFindInviteCode(gdb *gorm.DB, code, pepper string) (*db.InviteCode, bool, error) {
	if gdb == nil {
		return nil, false, db.ErrNilDB
	}
	clean, err := codes.ValidateCodeFormat(code)
	if err != nil {
		return nil, false, nil
	}
	hash, err := codes.HashCode(clean, pepper)
	if err != nil {
		return nil, false, err
	}
	var ic db.InviteCode
	err = gdb.Where("code_hash = ? AND status = ? AND used_by IS NULL", hash, db.CodeStatusUnused).First(&ic).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &ic, true, nil
}
