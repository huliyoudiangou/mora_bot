package db

import (
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Seed 在空库时写入默认 SystemConfig；使用 FirstOrCreate，重复运行安全。
func Seed(gdb *gorm.DB, defaults map[string]string) error {
	for k, v := range defaults {
		var c SystemConfig
		if err := gdb.Where("`key` = ?", k).FirstOrCreate(&c, SystemConfig{Key: k, Value: v}).Error; err != nil {
			return err
		}
	}
	return nil
}

// OpenMem 开箱即用的内存 sqlite，仅供测试。
func OpenMem() (*gorm.DB, error) {
	return gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
}
