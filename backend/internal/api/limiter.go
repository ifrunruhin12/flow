package api

import (
	"sync"
	"time"
)

type rateEntry struct {
	count int
	reset time.Time
}

type fixedWindowLimiter struct {
	mu      sync.Mutex
	entries map[string]rateEntry
	limit   int
	window  time.Duration
	calls   uint64
	now     func() time.Time
}

func newFixedWindowLimiter(limit int, window time.Duration) *fixedWindowLimiter {
	return &fixedWindowLimiter{entries: make(map[string]rateEntry), limit: limit, window: window, now: time.Now}
}

func (l *fixedWindowLimiter) Allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.calls%256 == 0 {
		for candidate, entry := range l.entries {
			if !now.Before(entry.reset) {
				delete(l.entries, candidate)
			}
		}
	}
	entry := l.entries[key]
	if entry.reset.IsZero() || !now.Before(entry.reset) {
		l.entries[key] = rateEntry{count: 1, reset: now.Add(l.window)}
		return true
	}
	if entry.count >= l.limit {
		return false
	}
	entry.count++
	l.entries[key] = entry
	return true
}
