package db

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// SignRewardDefault 默认每日签到奖励果果币。
const SignRewardDefault = 10

// SignStreakBonusEvery 每连续 N 天签到再奖励 1 枚。
const SignStreakBonusEvery = 7

// ChinaLoc 东八区固定时区（容器无 tzdata 时也能工作）。
var ChinaLoc = time.FixedZone("CST", 8*3600)

// chinaToday 返回东八区"今天"标识 (YYYY-MM-DD)。
func chinaToday() string {
	tn := time.Now().In(ChinaLoc)
	return tn.Format("2006-01-02")
}

// DoSignIn 完成签到：幂等（同一天仅一条），含连签加成（使用默认加成 1/7 天，无上限）。
func DoSignIn(gdb *gorm.DB, userID int64, reward int) (*SignInRecord, error) {
	return DoSignInWithBonus(gdb, userID, reward, 1, 0)
}

// DoSignInWithBonus 与 DoSignIn 相同，但允许配置连签加成与上限。
// bonus: 每满 SignStreakBonusEvery 天额外奖励的果果币（<=0 用默认 1）
// bonusCap: 总加成上限（<=0 表示不限）
func DoSignInWithBonus(gdb *gorm.DB, userID int64, reward, bonus, bonusCap int) (*SignInRecord, error) {
	if gdb == nil {
		return nil, ErrNilDB
	}
	if reward <= 0 {
		reward = SignRewardDefault
	}
	if bonus <= 0 {
		bonus = 1
	}
	day := chinaToday()
	var rec SignInRecord
	err := gdb.Transaction(func(tx *gorm.DB) error {
		var u User
		if err := tx.Where("telegram_id = ?", userID).First(&u).Error; err != nil {
			return err
		}
		// 连签：昨天已签 = streak+1，否则重置
		yesterday := time.Now().In(ChinaLoc).Add(-24 * time.Hour).Format("2006-01-02")
		var yCnt int64
		if err := tx.Model(&SignInRecord{}).
			Where("user_id = ? AND sign_day = ?", userID, yesterday).
			Count(&yCnt).Error; err != nil {
			return err
		}
		streak := u.SignStreak
		if yCnt > 0 {
			streak++
		} else {
			streak = 1
		}
		// 加成：每满 bonusEvery 天 +bonus，且不超过 cap
		addon := 0
		if streak >= SignStreakBonusEvery {
			addon = (streak / SignStreakBonusEvery) * bonus
			if bonusCap > 0 && addon > bonusCap {
				addon = bonusCap
			}
		}
		realReward := reward + addon
		rec = SignInRecord{UserID: userID, SignDay: day, Reward: realReward, Streak: streak}
		if err := tx.Create(&rec).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrAlreadySignedIn
			}
			return err
		}
		newBal := u.GuoGuo + realReward
		if err := tx.Model(&u).Updates(map[string]any{
			"guo_guo":      newBal,
			"sign_streak":  streak,
			"last_sign_at": time.Now(),
		}).Error; err != nil {
			return err
		}
		txn := PointTransaction{
			UserID:       userID,
			Change:       realReward,
			BalanceAfter: newBal,
			Type:         TxSignInDaily,
			Remark:       "每日签到",
		}
		return tx.Create(&txn).Error
	})
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// AddPoints 果果币加/扣：绝不允许变负数。delta 可正可负。
func AddPoints(gdb *gorm.DB, userID int64, delta int, txType, remark string, operatorID int64) error {
	if gdb == nil {
		return ErrNilDB
	}
	return gdb.Transaction(func(tx *gorm.DB) error {
		return AddPointsTx(tx, userID, delta, txType, remark, operatorID)
	})
}

// AddPointsTx 在已开启的事务内扣/加果果币（禁止负余额）。供复合事务复用。
func AddPointsTx(tx *gorm.DB, userID int64, delta int, txType, remark string, operatorID int64) error {
	if tx == nil {
		return ErrNilDB
	}
	var u User
	if err := tx.Select("telegram_id", "guo_guo").Where("telegram_id = ?", userID).First(&u).Error; err != nil {
		return err
	}
	newBal := u.GuoGuo + delta
	if newBal < 0 {
		return ErrInsufficientPoints
	}
	res := tx.Model(&User{}).
		Where("telegram_id = ? AND guo_guo = ?", userID, u.GuoGuo).
		Update("guo_guo", newBal)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrOptimisticLock
	}
	return tx.Create(&PointTransaction{
		UserID:       userID,
		Change:       delta,
		BalanceAfter: newBal,
		Type:         txType,
		Remark:       remark,
		OperatorID:   operatorID,
	}).Error
}

// isUniqueViolation sqlite unique 约束冲突（纯 Go 驱动的错误文本特征）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return errors.Is(err, gorm.ErrDuplicatedKey) ||
		(contains(s, "UNIQUE constraint failed") || contains(s, "constraint failed"))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
