package bot

import (
	"sync"
	"time"
)

// Session 多步对话状态。
type Session struct {
	Kind    string
	Step    int
	Data    map[string]any
	Expires time.Time
}

// SessionStore 内存会话表（TTL 由 GC 清理）。
type SessionStore struct {
	mu sync.Mutex
	m  map[int64]*Session
}

// NewSessionStore 创建空会话表。
func NewSessionStore() *SessionStore {
	return &SessionStore{m: make(map[int64]*Session)}
}

// Current 取当前会话；无或过期返回 nil。
func (s *SessionStore) Current(userID int64) *Session {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[userID]
	if sess == nil {
		return nil
	}
	if !sess.Expires.IsZero() && time.Now().After(sess.Expires) {
		delete(s.m, userID)
		return nil
	}
	return sess
}

// Begin 开启新会话（Step=0）。
func (s *SessionStore) Begin(userID int64, kind string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[userID] = &Session{
		Kind:    kind,
		Step:    0,
		Data:    map[string]any{},
		Expires: time.Now().Add(30 * time.Minute),
	}
}

// Advance 步进会话并合并 payload，返回会话指针。
func (s *SessionStore) Advance(userID int64, data map[string]any) *Session {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[userID]
	if sess == nil {
		sess = &Session{Kind: "", Data: map[string]any{}, Expires: time.Now().Add(30 * time.Minute)}
		s.m[userID] = sess
	}
	// 每次推进会话都刷新过期时间，避免用户在多步向导中稍作停留就过期。
	sess.Expires = time.Now().Add(30 * time.Minute)
	sess.Step++
	for k, v := range data {
		sess.Data[k] = v
	}
	return sess
}

// Clear 删除会话。
func (s *SessionStore) Clear(userID int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, userID)
}

// GC 清理过期会话。
func (s *SessionStore) GC() {
	if s == nil {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.m {
		if !sess.Expires.IsZero() && now.After(sess.Expires) {
			delete(s.m, id)
		}
	}
}