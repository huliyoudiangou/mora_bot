package db

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// GetOrCreateUser 按 telegram_id 查或建用户档案。
func GetOrCreateUser(gdb *gorm.DB, telegramID int64, username, firstName, lastName string) (*User, error) {
	var u User
	err := gdb.Where("telegram_id = ?", telegramID).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		u = User{
			TelegramID: telegramID,
			TgUsername: username,
			FirstName:  firstName,
			LastName:   lastName,
			Status:     UserStatusActive,
		}
		if e2 := gdb.Create(&u).Error; e2 != nil {
			var u2 User
			if e3 := gdb.Where("telegram_id = ?", telegramID).First(&u2).Error; e3 == nil {
				// 并发创建冲突时，也把本次消息里的用户名/姓名补写到已有档案。
				updates := map[string]any{}
				if username != "" && u2.TgUsername != username {
					updates["tg_username"] = username
				}
				if firstName != "" && u2.FirstName != firstName {
					updates["first_name"] = firstName
				}
				if lastName != "" && u2.LastName != lastName {
					updates["last_name"] = lastName
				}
				if len(updates) > 0 {
					_ = gdb.Model(&u2).Updates(updates).Error
				}
				return &u2, nil
			}
			return nil, e2
		}
		return &u, nil
	}
	if err != nil {
		return nil, err
	}
	if username != "" && u.TgUsername != username {
		_ = gdb.Model(&u).Update("tg_username", username).Error
		u.TgUsername = username
	}
	if firstName != "" && u.FirstName != firstName {
		u.FirstName = firstName
	}
	if lastName != "" && u.LastName != lastName {
		u.LastName = lastName
	}
	if u.FirstName != "" || u.LastName != "" || (username != "" && u.TgUsername != username) {
		_ = gdb.Model(&u).Updates(map[string]any{
			"tg_username": u.TgUsername,
			"first_name":  u.FirstName,
			"last_name":   u.LastName,
		}).Error
	}
	return &u, nil
}

// TouchUserActive 更新最近活跃锚点（用 UpdatedAt 近似）。
func TouchUserActive(gdb *gorm.DB, telegramID int64) {
	gdb.Model(&User{}).Where("telegram_id = ?", telegramID).Update("updated_at", time.Now())
}
