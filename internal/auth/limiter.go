package auth

import (
	"sync"
	"time"
)

// Limiter 滑动窗口限流器（用于 SSO 入口，**独立于全局 API 限流**）。
//
// 默认 5 次/分钟/IP：SSO 是凭据入口，必须比普通 API 更严（docs/开发实施计划.md 附录 A）。
type Limiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	hits map[string][]time.Time
}

// NewLimiter 创建限流器；limit<=0 用 5，window<=0 用 1min。
func NewLimiter(limit int, window time.Duration) *Limiter {
	if limit <= 0 {
		limit = 5
	}
	if window <= 0 {
		window = time.Minute
	}
	return &Limiter{limit: limit, window: window, now: time.Now, hits: make(map[string][]time.Time)}
}

// Allow 判断该 key（通常为 IP）是否放行。
func (l *Limiter) Allow(key string) bool {
	now := l.now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// Reset 清空（测试用）。
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits = make(map[string][]time.Time)
}

// SetNow 注入时钟（测试用）。
func (l *Limiter) SetNow(fn func() time.Time) {
	if fn == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = fn
}
