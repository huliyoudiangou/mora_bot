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

// SendMenuButton 发送带一个按钮的文本（按钮回调数据 url）。
func (s *tgSender) SendMenuButton(ctx context.Context, chatID int64, text, buttonText, url string) error {
	// 简单策略：纯文本 + 链接，按钮在非 miniapp 场景相当于明文。
	if url == "" {
		return s.SendText(ctx, chatID, text)
	}
	return s.SendText(ctx, chatID, fmt.Sprintf("%s\n\n[%s](%s)", text, buttonText, url))
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
//  1. wal_checkpoint 合并 WAL；
//  2. 复制主库文件到 data/backups/；
//  3. 配置了 BACKUP_ENCRYPT_KEY 则加密为 .enc；
//  4. 保留 BACKUP_KEEP_COUNT 份，删除更旧的；
//  5. 配置了 BACKUP_GROUP_ID 则通过 Bot API 发送给目标群/频道。
//
// 返回生成的备份文件名（未生成返回空串）。
func backupDatabase(ctx context.Context, gdb *gorm.DB, bot *tgbotapi.Bot, dbPath, encKey string, keepCount int, groupID int64) (string, error) {
	if gdb == nil || dbPath == "" {
		return "", nil
	}
	// 1) 合并 WAL，确保只复制一个文件就完整
	if sqlDB, err := gdb.DB(); err == nil {
		_, _ = sqlDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	}

	dir := filepath.Join(filepath.Dir(dbPath), "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ts := time.Now().Format("2006-01-02-150405.000")
	dst := filepath.Join(dir, ts+".db")
	data, err := os.ReadFile(dbPath)
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(encKey) != "" {
		// 加密备份
		enc, err := encryptBytes(data, encKey)
		if err != nil {
			return "", err
		}
		dst = dst + ".enc"
		if err := os.WriteFile(dst, []byte("v1:"+base64.RawURLEncoding.EncodeToString(enc)), 0o600); err != nil {
			return "", err
		}
	} else {
		// 明文备份
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return "", err
		}
	}

	// 2) 修剪旧的（按文件名时间戳排序，保留最新 keepCount 份）
	if keepCount > 0 {
		entries, err := os.ReadDir(dir)
		if err == nil {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				names = append(names, e.Name())
			}
			sort.Strings(names)
			// 从最旧开始删
			for i := 0; i+keepCount < len(names); i++ {
				_ = os.Remove(filepath.Join(dir, names[i]))
			}
		}
	}

	// 3) 发送到群/频道（可选）
	if bot != nil && groupID != 0 {
		f, err := os.Open(dst)
		if err == nil {
			_, _ = bot.SendDocument(ctx, &tgbotapi.SendDocumentParams{
				ChatID:  groupID,
				Document: &models.InputFileUpload{Filename: filepath.Base(dst), Data: f},
				Caption: "📦 mora_bot 数据库备份 " + ts,
			})
			_ = f.Close()
		}
	}
	return filepath.Base(dst), nil
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
			// 等待到下一个目标小时
			now := time.Now()
			next := now.Truncate(time.Hour).Add(time.Duration(cfgBackupHour) * time.Hour)
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
			}
			name, err := backupDatabase(ctx, gdb, bot, dbPath, encKey, keepCount, groupID)
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
			now := time.Now()
			next := now.Truncate(time.Hour).Add(time.Duration(hour) * time.Hour)
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
			}
			today := now.Format("2006-01-02")
			// 即将到期：expire_at 在 [now, now+notifyBeforeDays] 区间，且未永久
			from := now
			to := now.AddDate(0, 0, notifyBeforeDays)
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
				_, _ = bot.SendMessage(ctx, &tgbotapi.SendMessageParams{ChatID: u.TelegramID, Text: msg})
				sent[u.TelegramID] = today
				lg.Info("已发送到期提醒", "user", u.TelegramID, "days_left", d)
			}
		}
	}()
}
