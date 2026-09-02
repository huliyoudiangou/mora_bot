package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
	"gorm.io/gorm"

	"mora_bot/internal/bot"
	"mora_bot/internal/config"
	"mora_bot/internal/db"
)

// buildSuper 把列表变成 set。
func buildSuper(ids []int64) map[int64]bool {
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// tgSender Bot API 适配器。
type tgSender struct {
	bot *tgbotapi.Bot
}

// SendText 普通文本。
func (s *tgSender) SendText(ctx context.Context, chatID int64, text string) error {
	_, err := s.bot.SendMessage(ctx, &tgbotapi.SendMessageParams{ChatID: chatID, Text: text})
	return err
}

// SendTextHTML HTML 消息。
func (s *tgSender) SendTextHTML(ctx context.Context, chatID int64, html string) error {
	_, err := s.bot.SendMessage(ctx, &tgbotapi.SendMessageParams{
		ChatID:    chatID,
		Text:      html,
		ParseMode: "HTML",
	})
	return err
}

// AnswerCallback inline 按钮应答。
func (s *tgSender) AnswerCallback(ctx context.Context, id, text string, alert bool) error {
	_, err := s.bot.AnswerCallbackQuery(ctx, &tgbotapi.AnswerCallbackQueryParams{
		CallbackQueryID: id,
		Text:            text,
		ShowAlert:       alert,
	})
	return err
}

// EditText 编辑消息。
func (s *tgSender) EditText(ctx context.Context, chatID int64, messageID int, text string) error {
	_, err := s.bot.EditMessageText(ctx, &tgbotapi.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      text,
	})
	return err
}

// SendKeyboard 发送带内联键盘的消息。
func (s *tgSender) SendKeyboard(ctx context.Context, chatID int64, text string, rows [][]bot.KeyboardButton) error {
	_, err := s.bot.SendMessage(ctx, &tgbotapi.SendMessageParams{
		ChatID:      chatID,
		Text:        text,
		ParseMode:   "HTML",
		ReplyMarkup: toInlineKeyboard(rows),
	})
	return err
}

// EditKeyboard 原地编辑消息文本与内联键盘（rows 为空时移除键盘）。
func (s *tgSender) EditKeyboard(ctx context.Context, chatID int64, messageID int, text string, rows [][]bot.KeyboardButton) error {
	params := &tgbotapi.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      text,
		ParseMode: "HTML",
	}
	if len(rows) > 0 {
		params.ReplyMarkup = toInlineKeyboard(rows)
	}
	_, err := s.bot.EditMessageText(ctx, params)
	return err
}

// toInlineKeyboard 把业务按钮行转成 Telegram InlineKeyboardMarkup。
func toInlineKeyboard(rows [][]bot.KeyboardButton) *models.InlineKeyboardMarkup {
	out := &models.InlineKeyboardMarkup{InlineKeyboard: make([][]models.InlineKeyboardButton, 0, len(rows))}
	for _, row := range rows {
		var kb []models.InlineKeyboardButton
		for _, b := range row {
			btn := models.InlineKeyboardButton{Text: b.Text}
			if b.URL != "" {
				btn.URL = b.URL
			} else if b.Data != "" {
				btn.CallbackData = b.Data
			}
			kb = append(kb, btn)
		}
		if len(kb) > 0 {
			out.InlineKeyboard = append(out.InlineKeyboard, kb)
		}
	}
	return out
}

// ------------------------ 启动通知 ------------------------

// warnConfig 启动时对安全敏感的配置项给出告警（不阻断启动，便于先修复再重启）。
func warnConfig(lg *slog.Logger, cfg *config.Config) {
	if len(cfg.SecurityPepper) > 0 && len(cfg.SecurityPepper) < 32 {
		lg.Warn("SECURITY_PEPPER 过短（建议 ≥32 字符随机串）：过短的密钥可被离线爆破，危及卡密与安全码哈希")
	}
	if cfg.SecurityPepper == "" {
		lg.Warn("SECURITY_PEPPER 未配置：卡密生成/核销与安全码校验将不可用")
	}
	if cfg.BackupEncryptKey != "" && len(cfg.BackupEncryptKey) < 32 {
		lg.Warn("BACKUP_ENCRYPT_KEY 过短（建议 ≥32 字符随机串）：加密备份可被离线爆破")
	}
	if len(cfg.SuperAdminTgIDs) == 0 {
		lg.Warn("未配置 SUPER_ADMIN_TG_IDS：管理面板/工单通知/开注自动关闭通知均不可用")
	}
	if cfg.JellyfinURL != "" && cfg.JellyfinTemplateUserID == "" {
		lg.Warn("未配置 JELLYFIN_TEMPLATE_USER_ID：新注册账号将使用 Jellyfin 默认权限而非模板策略")
	}
}

// notifyAdminsOnStartup 进程启动后向管理员私聊发送启动通知（含日志）。
func notifyAdminsOnStartup(ctx context.Context, lg *slog.Logger, bot *tgbotapi.Bot, adminIDs []int64, version string) {
	if bot == nil || len(adminIDs) == 0 {
		return
	}
	msg := fmt.Sprintf("🟢 mora_bot 已启动\n版本：%s\n时间：%s", version, time.Now().Format("2006-01-02 15:04:05"))
	for _, id := range adminIDs {
		_, err := bot.SendMessage(ctx, &tgbotapi.SendMessageParams{ChatID: id, Text: msg})
		if err != nil {
			lg.Warn("启动通知发送失败", "admin", id, "err", err)
		} else {
			lg.Info("启动通知已发送", "admin", id)
		}
	}
}

// ------------------------ 数据库备份 ------------------------

// backupDatabase 执行一次 SQLite 一致性备份：
//  1. VACUUM INTO 生成原子一致性快照（替代 wal_checkpoint+ReadFile 组合——
//     两者之间新落 WAL 的写入会从备份中丢失）；
//  2. 配置了 BACKUP_ENCRYPT_KEY 则加密为 .enc；
//  3. 保留 BACKUP_KEEP_COUNT 份，删除更旧的；
//  4. 配置了 BACKUP_GROUP_ID 则通过 Bot API 发送给目标群/频道。
//
// 返回生成的备份文件名（未生成返回空串）。
func backupDatabase(ctx context.Context, lg *slog.Logger, gdb *gorm.DB, bot *tgbotapi.Bot, dbPath, encKey string, keepCount int, groupID int64) (string, error) {
	if gdb == nil || dbPath == "" {
		return "", nil
	}

	dir := filepath.Join(filepath.Dir(dbPath), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	ts := time.Now().Format("2006-01-02-150405.000")
	dst := filepath.Join(dir, ts+".db")
	// VACUUM INTO 要求目标文件不存在；先写临时名，成功后改名/加密。
	tmp := dst + ".tmp"
	_ = os.Remove(tmp)
	sqlDB, err := gdb.DB()
	if err != nil {
		return "", err
	}
	if _, err := sqlDB.Exec("VACUUM INTO ?", tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}

	if strings.TrimSpace(encKey) != "" {
		// 加密备份
		data, err := os.ReadFile(tmp)
		if err != nil {
			_ = os.Remove(tmp)
			return "", err
		}
		enc, err := encryptBytes(data, encKey)
		if err != nil {
			_ = os.Remove(tmp)
			return "", err
		}
		dst = dst + ".enc"
		if err := os.WriteFile(dst, []byte("v1:"+base64.RawURLEncoding.EncodeToString(enc)), 0o600); err != nil {
			_ = os.Remove(tmp)
			return "", err
		}
		_ = os.Remove(tmp)
	} else {
		// 明文备份
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Remove(tmp)
			return "", err
		}
	}

	// 修剪旧的（按文件名时间戳排序，保留最新 keepCount 份）
	if keepCount > 0 {
		entries, err := os.ReadDir(dir)
		if err == nil {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				// 只清理本程序生成的备份文件，避免误删目录中其它文件。
				if !strings.HasSuffix(name, ".db") && !strings.HasSuffix(name, ".db.enc") {
					continue
				}
				names = append(names, name)
			}
			sort.Strings(names)
			// 从最旧开始删
			for i := 0; i+keepCount < len(names); i++ {
				_ = os.Remove(filepath.Join(dir, names[i]))
			}
		}
	}

	// 发送到群/频道（可选；失败记日志，不影响本地备份的有效性）
	if bot != nil && groupID != 0 {
		f, err := os.Open(dst)
		if err != nil {
			lg.Warn("备份文件打开发送失败", "file", dst, "err", err)
			return filepath.Base(dst), nil
		}
		_, serr := bot.SendDocument(ctx, &tgbotapi.SendDocumentParams{
			ChatID:   groupID,
			Document: &models.InputFileUpload{Filename: filepath.Base(dst), Data: f},
			Caption:  "📦 mora_bot 数据库备份 " + ts,
		})
		_ = f.Close()
		if serr != nil {
			lg.Warn("备份推送群/频道失败", "file", dst, "err", serr)
		}
	}
	return filepath.Base(dst), nil
}

// nextLocalHour 计算本地时区下一个 hour 点整的时间（今天已过则取明天）。
// 不能用 UTC 语义的 Truncate：跨时区会把目标小时偏移到错误时刻。
func nextLocalHour(hour int) time.Time {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// startDailyBackup 每日定时备份循环。hour=-1 关闭。
func startDailyBackup(ctx context.Context, lg *slog.Logger, gdb *gorm.DB, bot *tgbotapi.Bot, cfgBackupHour int, dbPath, encKey string, keepCount int, groupID int64) {
	if cfgBackupHour < 0 || cfgBackupHour > 23 {
		return
	}
	go func() {
		for {
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(nextLocalHour(cfgBackupHour))):
			}
			name, err := backupDatabase(ctx, lg, gdb, bot, dbPath, encKey, keepCount, groupID)
			if err != nil {
				lg.Error("自动备份失败", "err", err)
			} else if name != "" {
				lg.Info("自动备份完成", "file", name)
			}
		}
	}()
}

// encryptBytes 用 XChaCha20-Poly1305 加密文件内容。密钥由 hkdf(BACKUP_ENCRYPT_KEY) 派生。
func encryptBytes(data []byte, key string) ([]byte, error) {
	hk := hkdf.New(sha256.New, []byte(key), []byte("mora_bot-backup-v1"), nil)
	rk := make([]byte, 32)
	if _, err := io.ReadFull(hk, rk); err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(rk)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, data, nil), nil
}

// ------------------------ 到期提醒 ------------------------

// startExpiryNotifier 每日检查即将到期的用户，私聊提醒续费。
// notifyBeforeDays<=0 时关闭。同一进程内每天对同一用户只提醒一次。
func startExpiryNotifier(ctx context.Context, lg *slog.Logger, gdb *gorm.DB, bot *tgbotapi.Bot, notifyBeforeDays int, hour int) {
	if gdb == nil || bot == nil || notifyBeforeDays <= 0 {
		return
	}
	if hour < 0 || hour > 23 {
		hour = 10
	}
	go func() {
		sent := map[int64]string{} // userID -> date（本进程内去重）
		for {
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(nextLocalHour(hour))):
			}
			// 关键：扫描用的“当前时间”必须在唤醒后重新取。
			// 旧实现复用了休眠前捕获的 now，导致跨日后 today 永远是旧日期，
			// 去重表把第二天起的所有用户都当成“今日已提醒”，提醒只会发一次。
			scan := time.Now()
			today := scan.Format("2006-01-02")
			// 清理跨日旧记录，避免 sent 表无限增长
			for id, day := range sent {
				if day != today {
					delete(sent, id)
				}
			}
			// 即将到期：expire_at 在 [now, now+notifyBeforeDays] 区间，且未永久
			from := scan
			to := scan.AddDate(0, 0, notifyBeforeDays)
			var users []struct {
				TelegramID int64
				ExpireAt   *time.Time
			}
			_ = gdb.Model(&db.User{}).
				Where("is_permanent = ? AND expire_at >= ? AND expire_at <= ? AND status = ?", false, from, to, "active").
				Find(&users).Error
			for _, u := range users {
				if u.ExpireAt == nil {
					continue
				}
				if prev, ok := sent[u.TelegramID]; ok && prev == today {
					continue
				}
				d := int(time.Until(*u.ExpireAt).Hours() / 24)
				if d < 0 {
					d = 0
				}
				msg := fmt.Sprintf("⏰ 你的 Jellyfin 账号将在 %d 天后到期（%s）。\n用果果币续期：/shop buy 后再 /redeem。", d, u.ExpireAt.Format("2006-01-02"))
				if _, err := bot.SendMessage(ctx, &tgbotapi.SendMessageParams{ChatID: u.TelegramID, Text: msg}); err != nil {
					// 发送失败不记去重，明天同一用户会重试。
					lg.Warn("到期提醒发送失败", "user", u.TelegramID, "err", err)
					continue
				}
				sent[u.TelegramID] = today
				lg.Info("已发送到期提醒", "user", u.TelegramID, "days_left", d)
			}
		}
	}()
}
