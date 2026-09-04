package bot

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"
	"sync"
	"time"

	"mora_bot/internal/codes"
)

// ---------------------------------------------------------------------------
// 安全码失败尝试限制：防止对低熵安全码（最短 4 位）的在线暴力枚举。
// 同一用户连续失败达上限后锁定一段时间，成功校验即清零。
// ---------------------------------------------------------------------------

const (
	secCodeMaxFails     = 5               // 连续失败上限
	secCodeLockDuration = 15 * time.Minute // 锁定时长
)

type secAttempt struct {
	fails     int
	lockUntil time.Time
}

// secCodeGuard 按用户记录安全码失败次数与锁定状态。
type secCodeGuard struct {
	mu    sync.Mutex
	state map[int64]*secAttempt
}

var secGuard = &secCodeGuard{state: map[int64]*secAttempt{}}

// bindGuard 绑定流程密码验证失败限制：
// /bind 用 AuthenticateByName 自证密码，若不设上限，任意 TG 用户都能把 bot
// 当作对任意 Jellyfin 账号的在线密码爆破代理。与安全码同一阈值：连续失败
// 5 次锁定 15 分钟（AuthBlocked 是"密码可能正确但被拒登"，不计失败）。
var bindGuard = &secCodeGuard{state: map[int64]*secAttempt{}}

// allowed 返回当前是否允许尝试；被锁定时返回剩余等待时长。
func (g *secCodeGuard) allowed(userID int64) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.state[userID]
	if !ok {
		return true, 0
	}
	if remaining := time.Until(a.lockUntil); remaining > 0 {
		return false, remaining
	}
	return true, 0
}

// fail 记录一次失败；达到上限时进入锁定。
func (g *secCodeGuard) fail(userID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.state[userID]
	if a == nil {
		a = &secAttempt{}
		g.state[userID] = a
	}
	a.fails++
	if a.fails >= secCodeMaxFails {
		a.lockUntil = time.Now().Add(secCodeLockDuration)
		a.fails = 0
	}
}

// reset 校验成功后清零失败计数。
func (g *secCodeGuard) reset(userID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.state, userID)
}

// GC 清理过期条目（随会话/锁的周期 GC 一起调用）。
func (g *secCodeGuard) GC(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, a := range g.state {
		if now.After(a.lockUntil) && a.fails == 0 {
			delete(g.state, id)
		}
	}
}

// GCSecurityGuard 包级 GC 入口（供 main 的周期任务调用）。
func GCSecurityGuard(now time.Time) {
	secGuard.GC(now)
	bindGuard.GC(now)
}

// verifySecurityCode 安全码统一校验入口：
// 先查锁定状态，再比对 HMAC；失败计数、成功清零。
// 返回 false 表示校验未通过（已向用户发送提示，调用方直接返回）。
func (r *Router) verifySecurityCode(ctx context.Context, deps *HandlerDeps, msg *Message, expectHash string) bool {
	if ok, remaining := secGuard.allowed(msg.From.ID); !ok {
		sendText(ctx, deps, msg.ChatID, fmt.Sprintf(
			"❌ 尝试次数过多，安全码校验已临时锁定，请约 %d 分钟后再试。", int(remaining.Minutes())+1))
		return false
	}
	hash, err := codes.HashSecurityCode(strings.TrimSpace(msg.Text), deps.Pepper)
	// 常数时间比较：避免逐字节短路比较对哈希串产生理论上的时序侧信道。
	if err != nil || subtle.ConstantTimeCompare([]byte(hash), []byte(expectHash)) != 1 {
		secGuard.fail(msg.From.ID)
		sendText(ctx, deps, msg.ChatID, "❌ 安全码错误，请重新输入（或输入 /cancel 放弃）。")
		return false
	}
	secGuard.reset(msg.From.ID)
	return true
}
