package api

import (
	"testing"
	"time"
)

func TestFixedWindowLimiter(t *testing.T) {
	limiter := newFixedWindowLimiter(2, time.Minute)
	now := time.Unix(100, 0)
	limiter.now = func() time.Time { return now }
	if !limiter.Allow("client") || !limiter.Allow("client") || limiter.Allow("client") {
		t.Fatal("unexpected rate-limit decision")
	}
	now = now.Add(time.Minute)
	if !limiter.Allow("client") {
		t.Fatal("window did not reset")
	}
}
