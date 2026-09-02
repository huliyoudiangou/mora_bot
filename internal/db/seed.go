package db

import "gorm.io/gorm"

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
