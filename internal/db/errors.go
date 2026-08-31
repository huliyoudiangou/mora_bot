package db

import "errors"

// 常用业务错误；供上层 Is() 匹配。
var (
	// ErrNilDB 表示调用方传入了空 *gorm.DB。
	ErrNilDB = errors.New("db is nil")

	// ErrAlreadySignedIn 今天已签到。
	ErrAlreadySignedIn = errors.New("今日已签到")

	// ErrInsufficientPoints 果果币不足。
	ErrInsufficientPoints = errors.New("果果币不足")

	// ErrOptimisticLock 乐观锁冲突（并发改同一行）。
	ErrOptimisticLock = errors.New("concurrent update, please retry")
)
