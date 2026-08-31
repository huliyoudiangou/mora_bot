package db

import (
	"time"

	"gorm.io/gorm"
)

// WriteAudit 写入一条管理员/高危操作审计。
// actorID: 操作者 TG ID；action: 动作名；targetType/targetID: 目标定位；detail: 简述。
func WriteAudit(gdb *gorm.DB, actorID int64, action, targetType, targetID, detail string) error {
	if gdb == nil {
		return ErrNilDB
	}
	return gdb.Create(&AuditLog{
		ActorID:    actorID,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Detail:     detail,
		CreatedAt:  time.Now(),
	}).Error
}

// LastAudit 最近 N 条审计（管理面板用）。
func LastAudit(gdb *gorm.DB, limit int) ([]AuditLog, error) {
	if gdb == nil {
		return nil, ErrNilDB
	}
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	var out []AuditLog
	if err := gdb.Order("id desc").Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}
