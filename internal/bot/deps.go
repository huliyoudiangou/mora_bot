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
	Sessions      *SessionStore
	Lockers       *UserLocker
	SessionLocks  *UserLocker // 与 Lockers 分离，专门串行化同一用户的会话/消息处理
	Snd           Sender

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
// 每个条目带引用计数，GC 只清理没有被任何 goroutine 引用/等待的空闲锁，
// 避免“旧请求仍持旧锁、新请求拿到新锁”导致同用户互斥失效。
type lockEntry struct {
	mu   sync.Mutex
	refs int
}

type UserLocker struct {
	mu    sync.Mutex
	locks map[int64]*lockEntry
}

// NewUserLocker 创建空锁表。
func NewUserLocker() *UserLocker {
	return &UserLocker{locks: make(map[int64]*lockEntry)}
}

// WithUser 在用户锁内执行 fn。
func (l *UserLocker) WithUser(userID int64, fn func()) {
	if l == nil {
		fn()
		return
	}
	l.mu.Lock()
	e, ok := l.locks[userID]
	if !ok {
		e = &lockEntry{}
		l.locks[userID] = e
	}
	e.refs++
	l.mu.Unlock()

	e.mu.Lock()
	defer func() {
		e.mu.Unlock()
		l.mu.Lock()
		e.refs--
		l.mu.Unlock()
	}()
	fn()
}

// GC 清理长期未用的锁条目。
// 仅在条目引用数为 0 且锁未被持有时删除；删除时不存在任何正在使用该锁的 goroutine。
func (l *UserLocker) GC(_ time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, e := range l.locks {
		if e.refs == 0 && e.mu.TryLock() {
			e.mu.Unlock()
			delete(l.locks, id)
		}
	}
}