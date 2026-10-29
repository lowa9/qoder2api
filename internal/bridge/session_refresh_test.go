package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsLoginExpired(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"403 code 105", NewUpstreamError(403, `{"code":"105","message":"Login expired"}`), true},
		{"401 code 105", NewUpstreamError(401, `{"code":"105"}`), true},
		{"403 quota", NewUpstreamError(403, `{"code":"112","message":"need upgrade"}`), false},
		{"500", NewUpstreamError(500, "Login expired"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isLoginExpired(c.err); got != c.want {
			t.Errorf("%s: isLoginExpired=%v want %v", c.name, got, c.want)
		}
	}
}

// TestCallQoderRefreshesSessionOnLoginExpired 验证 403 code 105 时先续期并
// 原地重试一次：成功则不冷却、不换号。
func TestCallQoderRefreshesSessionOnLoginExpired(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			fmt.Fprint(w, sseEnvelope(403, `{"code":"105","message":"Login expired"}`))
			return
		}
		fmt.Fprint(w, sseContent("ok"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	b, pool := poolBridge(t, srv.URL, "a", "b")
	var refreshed int32
	b.refreshSlotFn = func(slot *Slot) error {
		atomic.AddInt32(&refreshed, 1)
		return nil
	}

	err := b.CallQoderWithOpts(context.Background(), "codex", testMessages(), "auto", nil, CallOpts{}, func(Delta) {})
	if err != nil {
		t.Fatalf("expected success after refresh, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 (403 then retry)", got)
	}
	if got := atomic.LoadInt32(&refreshed); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	// 账号不应被冷却（仍可被选中）
	if _, ok := pool.Pick(nil); !ok {
		t.Fatal("account should not be cooling after successful refresh")
	}
}

// TestEnsureSlotFresh 验证到期前主动续期与最小间隔保护。
func TestEnsureSlotFresh(t *testing.T) {
	b, pool := poolBridge(t, "http://127.0.0.1:1", "a")
	slot := pool.snapshot()[0]

	var calls int32
	b.refreshSlotFn = func(s *Slot) error {
		atomic.AddInt32(&calls, 1)
		s.sess.ExpiresAt = time.Now().Add(24 * time.Hour)
		return nil
	}

	// 1) 1 小时后到期 → 应续期
	slot.sess.ExpiresAt = time.Now().Add(time.Hour)
	b.ensureSlotFresh(slot)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}

	// 2) 刚续期过（10 分钟间隔内）→ 不再重复
	slot.sess.ExpiresAt = time.Now().Add(time.Hour)
	b.ensureSlotFresh(slot)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1 (min interval)", got)
	}

	// 3) 距到期 12 小时 → 不需要续期（重置间隔后验证）
	slot.lastRefreshAttempt = time.Time{}
	slot.sess.ExpiresAt = time.Now().Add(12 * time.Hour)
	b.ensureSlotFresh(slot)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1 (outside window)", got)
	}

	// 4) 已过期 → 立即续期
	slot.lastRefreshAttempt = time.Time{}
	slot.sess.ExpiresAt = time.Now().Add(-time.Minute)
	b.ensureSlotFresh(slot)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("refresh calls = %d, want 2 (expired)", got)
	}
}
