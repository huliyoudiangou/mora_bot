package db

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Options 打开数据库的全部参数。
type Options struct {
	Path          string // sqlite 文件路径
	MaxOpenConns  int
	MaxIdleConns  int
	BusyTimeoutMS int // sqlite busy_timeout，毫秒
}

// Open 建立 glebarez 纯 Go SQLite 连接（无需 CGO），启用 WAL，并自动建表：
// 任何模型字段改动都会在这里 AutoMigrate 对齐。
func Open(o Options) (*gorm.DB, error) {
	if o.Path == "" {
		o.Path = "data/mora_bot.db"
	}
	if o.MaxOpenConns <= 0 {
		o.MaxOpenConns = 16
	}
	if o.MaxIdleConns <= 0 {
		o.MaxIdleConns = 8
	}
	if o.BusyTimeoutMS <= 0 {
		o.BusyTimeoutMS = 5000
	}

	dsn := filepathJoinData(o.Path) +
		fmt.Sprintf("?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
			o.BusyTimeoutMS)

	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, err
	}

	sdbE, err := gdb.DB()
	if err != nil {
		return nil, err
	}
	sdbE.SetMaxOpenConns(o.MaxOpenConns)
	sdbE.SetMaxIdleConns(o.MaxIdleConns)
	sdbE.SetConnMaxLifetime(0)

	if err := AutoMigrate(gdb); err != nil {
		return nil, err
	}
	return gdb, nil
}

// filepathJoinData 把 db 文件的父目录确保存在后返回原路径。
func filepathJoinData(p string) string {
	if p == ":memory:" {
		return p
	}
	dir := filepath.Dir(p)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	return p
}

// AutoMigrate 建/改所有表。
func AutoMigrate(gdb *gorm.DB) error {
	return gdb.AutoMigrate(
		&User{},
		&PointTransaction{},
		&SignInRecord{},
		&CodeBatch{},
		&InviteCode{},
		&RenewalCode{},
		&RenewalRecord{},
		&DramaRequest{},
		&DramaRequestLog{},
		&AuditLog{},
		&SystemConfig{},
		&JellyfinLine{},
	)
}

// now 便于测试 mock。
var now = time.Now
