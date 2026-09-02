// mora 是 mora_bot 的可执行入口：config → DB → Jellyfin → TG bot polling。
// 已按需求移除 Web/Mini App 面板，仅保留 Telegram Bot 核心功能。
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram/bot"

	"mora_bot/internal/bot"
	"mora_bot/internal/config"
	"mora_bot/internal/db"
	"mora_bot/internal/jellyfin"
)

func main() {
	lg := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		lg.Error("配置加载失败", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// -------- DB --------
	gdb, err := db.Open(db.Options{
		Path:          cfg.DatabaseURL,
		MaxOpenConns:  cfg.DbMaxOpenConns,
		MaxIdleConns:  cfg.DbMaxIdleConns,
		BusyTimeoutMS: cfg.DbBusyTimeoutMs,
	})
	if err != nil {
		lg.Error("数据库打开失败", "err", err)
		os.Exit(1)
	}
	if err := db.Seed(gdb, map[string]string{
		"mora_brand":   cfg.Brand,
		"super_admins": joinInt64(cfg.SuperAdminTgIDs),
	}); err != nil {
		lg.Error("系统配置播种失败", "err", err)
		os.Exit(1)
	}
	lg.Info("数据库已就绪", "path", cfg.DatabaseURL)

	// -------- Jellyfin --------
	var jf *jellyfin.Client
	if cfg.JellyfinURL != "" && cfg.JellyfinAPIKey != "" {
		jf, err = jellyfin.New(cfg.JellyfinURL, cfg.JellyfinAPIKey)
		if err != nil {
			lg.Error("Jellyfin 客户端创建失败", "err", err)
			os.Exit(1)
		}
		lg.Info("Jellyfin 客户端已就绪", "url", cfg.JellyfinURL)
	} else {
		lg.Info("Jellyfin 未配置（注册/绑定相关功能将不可用）")
	}

	// -------- TG Bot --------
	superSet := buildSuper(cfg.SuperAdminTgIDs)
	deps := &bot.HandlerDeps{
		DB:                  gdb,
		JF:                  jf,
		JFServerBase:        cfg.JellyfinTemplateUserID,
		Sessions:            bot.NewSessionStore(),
		Lockers:             bot.NewUserLocker(),
		Pepper:              cfg.SecurityPepper,
		RenewalPrice:        cfg.PriceRenewalCode,
		RenewalDays:         cfg.DefaultRenewalDays,
		SignInReward:        cfg.SignBaseReward,
		SignStreakBonus:     cfg.SignStreakBonus,
		SignStreakBonusCap:  cfg.SignStreakBonusCap,
		NewAccountValidDays: cfg.NewAccountValidDays,
		NotifyBeforeDays:    cfg.NotifyBeforeDays,
		DramaDailyLimit:     cfg.DramaDailyLimit,
		IsSuper: func(tgID int64) bool {
			return superSet[tgID]
		},
		SuperAdminIDs: cfg.SuperAdminTgIDs,
	}

	tg, err := tgbotapi.New(cfg.TgBotToken,
		tgbotapi.WithWorkers(cfg.WorkerCount),
		tgbotapi.WithUpdatesChannelCap(cfg.QueueCapacity),
	)
	if err != nil {
		lg.Error("TG bot 创建失败", "err", err)
		os.Exit(1)
	}
	deps.Snd = &tgSender{bot: tg}

	router := bot.NewRouter(deps)
	router.RegisterTG(tg)

	go func() {
		lg.Info("TG bot 开始 polling")
		tg.Start(ctx) // 感知 ctx 取消
	}()

	// -------- 后台任务 --------
	// 1) 启动通知
	if cfg.StartupNotifyAdmins {
		notifyAdminsOnStartup(ctx, lg, tg, cfg.SuperAdminTgIDs, "v1.1")
	}
	// 2) 每日自动备份（BACKUP_DAILY_HOUR=-1 关闭）
	startDailyBackup(ctx, lg, gdb, tg, cfg.BackupDailyHour, cfg.DatabaseURL, cfg.BackupEncryptKey, cfg.BackupKeepCount, cfg.BackupGroupID)
	// 3) 每日到期提醒
	startExpiryNotifier(ctx, lg, gdb, tg, cfg.NotifyBeforeDays, 10)

	// 会话/锁 GC
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				deps.Sessions.GC()
				deps.Lockers.GC(now)
			}
		}
	}()

	<-ctx.Done()
	lg.Info("接收到关闭信号，mora_bot 已停止")
}

// joinInt64 把管理员 ID 列表转成逗号分隔串（写入 system_configs.super_admins）。
func joinInt64(ids []int64) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += itoa64(id)
	}
	return out
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}