// Package bot Telegram 消息处理：路由、会话、发送抽象。
package bot

import (
	"context"
	"sync"
	"time"

	"gorm.io/gorm"

	"mora_bot/internal/jellyfin"
)

// KeyboardButton 内联键盘按钮（callback_data 或 url 二选一）。
type KeyboardButton struct {
	Text string // 按钮显示文字
	Data string // 回调数据（CallbackData 格式 "domain:action:..."）
	URL  string // 若非空则为 url 按钮（与 Data 互斥）
}

// Sender Telegram 发送抽象（便于测试 mock）。
type Sender interface {
	SendText(ctx context.Context, chatID int64, text string) error
	SendTextHTML(ctx context.Context, chatID int64, html string) error
	AnswerCallback(ctx context.Context, callbackID, text string, alert bool) error
	SendMenuButton(ctx context.Context, chatID int64, text, buttonText, url string) error
	EditText(ctx context.Context, chatID int64, messageID int, text string) error
	// SendKeyboard 发送带内联键盘的消息（面板）。
	SendKeyboard(ctx context.Context, chatID int64, text string, rows [][]KeyboardButton) error
	// EditKeyboard 原地更新消息文本与内联键盘（面板导航）。
	EditKeyboard(ctx context.Context, chatID int64, messageID int, text string, rows [][]KeyboardButton) error
}

// HandlerDeps 所有业务处理所需的依赖聚合。
type HandlerDeps struct {
	DB           *gorm.DB
	JF           *jellyfin.Client
	JFServerBase string // 用于克隆策略时的默认模板用户 ID（demo 传 ""）
	Sessions     *SessionStore
	Lockers      *UserLocker
	Snd          Sender

	// Pepper 卡密签名/加密主密钥（SECURITY_PEPPER）。为空时卡密功能禁用。
	Pepper string

	RenewalPrice int
	RenewalDays  int
	SignInReward int

	// PriceInviteCode 邀请码价格（果果币）（PRICE_INVITE_CODE）。
	PriceInviteCode int

	// SignStreakBonus 每满 7 天连续签到额外奖励果果币（SIGN_STREAK_BONUS）。
	SignStreakBonus int
	// SignStreakBonusCap 连续签到达成的总加成上限（0=不限）。
	SignStreakBonusCap int
	// NewAccountValidDays 新注册账号默认有效天数（0=永久）。
	NewAccountValidDays int
	// NotifyBeforeDays 到期前多少天开始提醒（0=关闭）。
	NotifyBeforeDays int
	// DramaDailyLimit 每用户每天最多提交求剧数（0=不限）。
	DramaDailyLimit int

	// IsSuper 判定管理员；由 main 注入（env SUPER_ADMIN_TG_IDS）。
	IsSuper func(tgID int64) bool
	// SuperAdminIDs 管理员 ID 列表（用于新工单等私聊通知）。
	SuperAdminIDs []int64
}

// TGUser Telegram 用户最小视图。
type TGUser struct {
	ID        int64
	Username  string
	FirstName string
	LastName  string
}

// Message 本地消息视图。
type Message struct {
	From      TGUser
	ChatID    int64
	Text      string
	MessageID int
}

// CallbackQuery 本地 inline 回调视图。
type CallbackQuery struct {
	ID        string
	Data      string
	From      TGUser
	ChatID    int64
	MessageID int // 关联消息 ID（面板原地编辑用）
}

// UserLocker 按用户串行化（防并发双签/双扣）。
type UserLocker struct {
	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

// NewUserLocker 创建空锁表。
func NewUserLocker() *UserLocker {
	return &UserLocker{locks: make(map[int64]*sync.Mutex)}
}

// WithUser 在用户锁内执行 fn。
func (l *UserLocker) WithUser(userID int64, fn func()) {
	if l == nil {
		fn()
		return
	}
	l.mu.Lock()
	m, ok := l.locks[userID]
	if !ok {
		m = &sync.Mutex{}
		l.locks[userID] = m
	}
	l.mu.Unlock()
	m.Lock()
	defer m.Unlock()
	fn()
}

// GC 清理长期未用的锁条目。
func (l *UserLocker) GC(_ time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	// 锁表很小，直接重建惰性表即可；活跃锁由 WithUser 懒加载。
	l.locks = make(map[int64]*sync.Mutex)
	l.mu.Unlock()
}