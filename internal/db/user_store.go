package db

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"strconv"
	"strings"
)

// GetOrCreateUser 按 telegram_id 查或建用户档案。
func GetOrCreateUser(gdb *gorm.DB, telegramID int64, username, _, _ string) (*User, error) {
	var u User
	err := gdb.Where("telegram_id = ?", telegramID).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		u = User{
			TelegramID: telegramID,
			TgUsername: username,
			Status:     UserStatusActive,
		}
		if e2 := gdb.Create(&u).Error; e2 != nil {
			var u2 User
			if e3 := gdb.Where("telegram_id = ?", telegramID).First(&u2).Error; e3 == nil {
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
	return &u, nil
}

// TouchUserActive 更新最近活跃锚点（用 UpdatedAt 近似）。
func TouchUserActive(gdb *gorm.DB, telegramID int64) {
	gdb.Model(&User{}).Where("telegram_id = ?", telegramID).Update("updated_at", time.Now())
}

// IsSuperAdmin 依 system_configs.key='super_admins'（逗号分隔） 检查。
func IsSuperAdmin(gdb *gorm.DB, telegramID int64) (bool, error) {
	var c SystemConfig
	err := gdb.Where("`key` = 'super_admins'").First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	target := strconv.FormatInt(telegramID, 10)
	for _, v := range strings.Split(c.Value, ",") {
		if strings.TrimSpace(v) == target {
			return true, nil
		}
	}
	return false, nil
}
