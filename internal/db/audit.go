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
