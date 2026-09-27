package db

import (
	"strings"
	"time"

	"gorm.io/gorm"
)

// ConfigGet 读 system_configs 的文本值；不存在或为空时返回 def。
//
// 用 Limit(1).Find 而非 First：键不存在时 First 会返回 ErrRecordNotFound，
// GORM 会按 Warn 级别打一条 "record not found" 日志 —— 而"配置尚未写入"是
// 完全正常的初始状态，不该刷日志噪音。
func ConfigGet(gdb *gorm.DB, key, def string) string {
	if gdb == nil {
		return def
	}
	var c SystemConfig
	if err := gdb.Where("`key` = ?", key).Limit(1).Find(&c).Error; err != nil {
		return def
	}
	if c.Key == "" || strings.TrimSpace(c.Value) == "" {
		return def
	}
	return c.Value
}

// ConfigSet 写 system_configs（upsert）。
func ConfigSet(gdb *gorm.DB, key, val string) error {
	if gdb == nil {
		return ErrNilDB
	}
	val = strings.TrimSpace(val)
	row := SystemConfig{Key: key, Value: val, UpdatedAt: time.Now()}
	return gdb.Where("`key` = ?", key).
		Assign(SystemConfig{Value: val, UpdatedAt: time.Now()}).
		FirstOrCreate(&row).Error
}

// dayStamp 返回"今天"的日期串（本地时区，YYYY-MM-DD）。
func dayStamp() string { return time.Now().Format("2006-01-02") }

// RanToday 判断某个"每日一次"的后台任务今天是否已经执行过。
//
// 用途：每日定时任务（备份/提醒/巡检）若只在固定钟点触发，进程活不到那一刻
// 就永远不会执行；加启动补跑又会导致每次重启都重复执行。用"上次执行日期"
// 落库即可两者兼顾：今天没跑过才补跑，跑过就跳过。
func RanToday(gdb *gorm.DB, task string) bool {
	return ConfigGet(gdb, "last_run_"+task, "") == dayStamp()
}

// MarkRanToday 记录某个每日任务今天已执行（仅在真正执行成功后调用）。
func MarkRanToday(gdb *gorm.DB, task string) error {
	return ConfigSet(gdb, "last_run_"+task, dayStamp())
}
