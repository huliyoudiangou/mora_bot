// Package config loads .env (if present) into the process environment and
// exposes a typed application configuration. Optional features are enabled
// only when their required fields are non-empty.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is a snapshot of process configuration.
type Config struct {
	TgBotToken  string
	DatabaseURL string // sqlite 文件路径，为纯文件名时会自动落到 ./data 下

	SuperAdminTgIDs []int64

	// SecurityPepper 卡密 HMAC/加密的主密钥（SECURITY_PEPPER，至少 32 字节）。
	SecurityPepper string

	DbMaxOpenConns  int
	DbMaxIdleConns  int
	DbBusyTimeoutMs int

	JellyfinURL            string
	JellyfinAPIKey         string
	JellyfinTemplateUserID string

	MetricsEnabled bool
	MetricsAddr    string

	Brand   string
	Verbose bool

	// ---------- 经济 / 签到 ----------
	// 每日签到基础果果币（SIGN_BASE_REWARD）
	SignBaseReward int
	// 连续签到加成：每满 SignStreakBonusEvery 天额外奖励 bonus 枚（SIGN_STREAK_BONUS）
	SignStreakBonus int
	// 连续签到加成上限：总加成不超过该值（SIGN_STREAK_BONUS_CAP，0=不限）
	SignStreakBonusCap int
	// 续期码价格（果果币）（PRICE_RENEWAL_CODE）
	PriceRenewalCode int
	// 续期码默认天数（DEFAULT_RENEWAL_DAYS）
	DefaultRenewalDays int
	// 邀请码价格（果果币）（PRICE_INVITE_CODE，预留）
	PriceInviteCode int

	// ---------- 账号有效期 ----------
	// 新注册账号默认有效天数（NEW_ACCOUNT_VALID_DAYS；0=永久不过期）
	NewAccountValidDays int
	// 到期前提醒天数（NOTIFY_BEFORE_DAYS）
	NotifyBeforeDays int

	// ---------- 追剧 ----------
	// 每用户每天最多求剧数（DRAMA_REQUEST_DAILY_LIMIT；0=不限）
	DramaDailyLimit int

	// ---------- 通知 ----------
	// 求剧工单/系统通知推送群 ID（NOTICE_GROUP_ID；0=只私聊管理员）
	NoticeGroupID int64
	// 启动时向管理员发送启动通知（BOT_STARTUP_NOTIFY_ADMINS）
	StartupNotifyAdmins bool

	// ---------- 备份 ----------
	// 每日自动备份小时（BACKUP_DAILY_HOUR；-1=关闭）
	BackupDailyHour int
	// 备份保留份数（BACKUP_KEEP_COUNT）
	BackupKeepCount int
	// 备份加密密钥（BACKUP_ENCRYPT_KEY）
	BackupEncryptKey string
	// 加密备份发送到的群/频道 ID（BACKUP_GROUP_ID；0=仅本地保存）
	BackupGroupID int64

	// ---------- 性能 ----------
	// go-telegram/bot 轮询并发（WORKER_COUNT）
	WorkerCount int
	// 更新队列容量（QUEUE_CAPACITY）
	QueueCapacity int
	// 注册 handler 超时秒（BOT_ADD_HANDLER_TIMEOUT_SECONDS）
	BotAddHandlerTimeout int
}

// Load reads an optional .env next to the working directory and builds Config.
func Load() (*Config, error) {
	if dir, err := os.Getwd(); err == nil {
		_ = loadEnvFile(filepath.Join(dir, ".env"))
	}
	c := &Config{
		TgBotToken:             env("TELEGRAM_BOT_TOKEN", ""),
		DatabaseURL:            env("DATABASE_PATH", env("DATABASE_URL", "data/mora_bot.db")),
		SuperAdminTgIDs:        parseIDList(env("SUPER_ADMIN_TG_IDS", env("ADMIN_TELEGRAM_IDS", ""))),
		SecurityPepper:         env("SECURITY_PEPPER", ""),
		DbMaxOpenConns:         envInt(orEnv("DB_MAX_OPEN_CONNS", "DATABASE_MAX_OPEN_CONNS"), 16),
		DbMaxIdleConns:         envInt(orEnv("DB_MAX_IDLE_CONNS", "DATABASE_MAX_IDLE_CONNS"), 8),
		DbBusyTimeoutMs:        envInt(orEnv("DB_BUSY_TIMEOUT_MS", "DATABASE_BUSY_TIMEOUT_MS"), 5000),
		JellyfinURL:            env("JELLYFIN_URL", ""),
		JellyfinAPIKey:         env("JELLYFIN_API_KEY", ""),
		JellyfinTemplateUserID: env("JELLYFIN_TEMPLATE_USER_ID", ""),
		MetricsEnabled:         envBool("METRICS_ENABLED", false),
		MetricsAddr:            env("METRICS_ADDR", ":9095"),
		Brand:                  env("BRAND", "mora"),
		Verbose:                envBool("VERBOSE", true),
		SignBaseReward:         envInt("SIGN_BASE_REWARD", 5),
		SignStreakBonus:        envInt("SIGN_STREAK_BONUS", 1),
		SignStreakBonusCap:     envInt("SIGN_STREAK_BONUS_CAP", 0),
		PriceRenewalCode:       envInt("PRICE_RENEWAL_CODE", 150),
		DefaultRenewalDays:     envInt("DEFAULT_RENEWAL_DAYS", 30),
		PriceInviteCode:        envInt("PRICE_INVITE_CODE", 300),
		NewAccountValidDays:    envInt("NEW_ACCOUNT_VALID_DAYS", 0),
		NotifyBeforeDays:       envInt("NOTIFY_BEFORE_DAYS", 3),
		DramaDailyLimit:        envInt("DRAMA_REQUEST_DAILY_LIMIT", 5),
		NoticeGroupID:          envInt64("NOTICE_GROUP_ID", 0),
		StartupNotifyAdmins:    envBool("BOT_STARTUP_NOTIFY_ADMINS", false),
		BackupDailyHour:        envIntOr("BACKUP_DAILY_HOUR", -1),
		BackupKeepCount:        envInt("BACKUP_KEEP_COUNT", 7),
		BackupEncryptKey:       env("BACKUP_ENCRYPT_KEY", ""),
		BackupGroupID:          envInt64("BACKUP_GROUP_ID", 0),
		WorkerCount:            envInt("WORKER_COUNT", 32),
		QueueCapacity:          envInt("QUEUE_CAPACITY", 512),
		BotAddHandlerTimeout:   envInt("BOT_ADD_HANDLER_TIMEOUT_SECONDS", 30),
	}
	return c, nil
}

// envInt 读取 int 环境变量（必须为正数，否则用默认值）。
func envInt(k string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k))); err == nil && v > 0 {
		return v
	}
	return def
}

// envIntOr 读取可负的 int（例如 BACKUP_DAILY_HOUR=-1 表示关闭）。
func envIntOr(k string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k))); err == nil {
		return v
	}
	return def
}

// envInt64 读取 int64 环境变量（0=未配置时返回 def）。
func envInt64(k string, def int64) int64 {
	if v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(k)), 10, 64); err == nil && v != 0 {
		return v
	}
	return def
}

// Watch 热更新占位：mora_bot 当前以进程重启方式应用配置变化。
func Watch(_ func(*Config)) {}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// orEnv 返回第一个非空的环境变量值（兼容新旧键名）。
func orEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func envBool(k string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(strings.TrimSpace(os.Getenv(k))); err == nil && v > 0 {
		return v
	}
	return def
}

func parseIDList(s string) []int64 {
	var out []int64
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if id, err := strconv.ParseInt(p, 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// loadEnvFile 极简 .env 解析（KEY=VALUE，# 注释，可带引号）。
func loadEnvFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		key = strings.TrimPrefix(key, "export")
		key = strings.TrimSpace(key)
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
	return nil
}
