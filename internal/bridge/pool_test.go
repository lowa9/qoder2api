package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"qoder2api/account"
	"qoder2api/internal/cosy"
)

func testSlot(t *testing.T, id, userType string) *Slot {
	t.Helper()
	account.SetDataRoot(t.TempDir())
	sess, err := cosy.NewSession(
		cosy.AuthIdentity{Uid: id, UserType: userType},
		"mid-"+id, "mtok-"+id, "mtype-"+id,
	)
	if err != nil {
		t.Fatalf("NewSession(%s): %v", id, err)
	}
	return NewSlot(id, id, account.RegionCN, sess, NewBearerClient(sess))
}

func TestPoolPickSkipsCooling(t *testing.T) {
	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	s1, ok := p.Pick(nil)
	if !ok {
		t.Fatal("first pick failed")
	}
	p.Cool(s1.ID, time.Minute, "quota")
	s2, ok := p.Pick(nil)
	if !ok || s2.ID == s1.ID {
		t.Fatalf("after cooling %s, pick = %v, want the other account", s1.ID, s2)
	}
	p.Cool(s2.ID, time.Minute, "quota")
	if _, ok := p.Pick(nil); ok {
		t.Fatal("expected no available account when all cooling")
	}
}

func TestPoolPickRespectsExclude(t *testing.T) {
	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	if s, ok := p.Pick(map[string]struct{}{"a": {}}); !ok || s.ID != "b" {
		t.Fatalf("exclude a -> %v", s)
	}
	if _, ok := p.Pick(map[string]struct{}{"a": {}, "b": {}}); ok {
		t.Fatal("all excluded should yield none")
	}
}

func TestPoolCooldownExpires(t *testing.T) {
	p := NewPool(testSlot(t, "a", "t"))
	p.Cool("a", 20*time.Millisecond, "transient")
	if _, ok := p.Pick(nil); ok {
		t.Fatal("should be cooling")
	}
	time.Sleep(30 * time.Millisecond)
	if _, ok := p.Pick(nil); !ok {
		t.Fatal("cooldown should have expired")
	}
}

func TestShouldFailoverClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"403 quota", NewUpstreamError(403, `{"code":"112"}`), true},
		{"429 rate", NewUpstreamError(429, "usage exceeds frequency limit"), true},
		{"502 transient", NewUpstreamError(502, "provider_error"), true},
		{"400 content policy", NewUpstreamError(400, "InternalError.Algo.DataInspectionFailed"), false},
		{"400 invalid param", NewUpstreamError(400, "invalid_parameter_error: Range of x"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := ShouldFailover(c.err); got != c.want {
			t.Errorf("%s: ShouldFailover=%v want %v", c.name, got, c.want)
		}
	}
}

func TestCooldownForClassification(t *testing.T) {
	if got := CooldownFor(NewUpstreamError(403, "quota")); got != CooldownQuota {
		t.Errorf("403 -> %v", got)
	}
	if got := CooldownFor(NewUpstreamError(429, "rate")); got != CooldownRate {
		t.Errorf("429 -> %v", got)
	}
	if got := CooldownFor(NewUpstreamError(502, "boom")); got != CooldownTransient {
		t.Errorf("502 -> %v", got)
	}
}

func poolBridge(t *testing.T, url string, ids ...string) (*Bridge, *Pool) {
	t.Helper()
	slots := make([]*Slot, 0, len(ids))
	for _, id := range ids {
		slots = append(slots, testSlot(t, id, "type_"+id))
	}
	pool := NewPool(slots...)
	b := NewPoolBridge(pool, map[string]interface{}{
		"model_config": map[string]interface{}{"key": "auto"},
		"messages":     []interface{}{},
	})
	b.chatStreamURL = url
	return b, pool
}

// 第一个账号 403（额度）→ 冷却换号 → 第二个账号 200 成功。
func TestCallQoderFailsOverToNextAccount(t *testing.T) {
	hits := 0
	var userTypes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		raw, _ := io.ReadAll(r.Body)
		if plain, err := cosy.Decode(string(raw)); err == nil {
			var m map[string]interface{}
			if json.Unmarshal(plain, &m) == nil {
				if ut, ok := m["aliyun_user_type"].(string); ok {
					userTypes = append(userTypes, ut)
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if hits == 1 {
			fmt.Fprint(w, sseEnvelope(403, `{"code":"112","message":"need upgrade"}`))
			return
		}
		fmt.Fprint(w, sseContent("ok"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	b, pool := poolBridge(t, srv.URL, "a", "b")
	var got []string
	err := b.CallQoderWithOpts(context.Background(), "codex", testMessages(), "auto", nil, CallOpts{},
		func(d Delta) {
			if d.Content != "" {
				got = append(got, d.Content)
			}
		})
	if err != nil {
		t.Fatalf("expected failover success, got %v", err)
	}
	if hits != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits)
	}
	if len(got) != 1 || got[0] != "ok" {
		t.Fatalf("deltas = %v", got)
	}
	// 冷启动起点随机，先撞哪个账号都可能；只要求两个账号都试过且顺序可辨
	if len(userTypes) != 2 {
		t.Fatalf("userTypes = %v, want two attempts", userTypes)
	}
	a2b := userTypes[0] == "type_a" && userTypes[1] == "type_b"
	b2a := userTypes[0] == "type_b" && userTypes[1] == "type_a"
	if !a2b && !b2a {
		t.Fatalf("expected failover between a and b, userTypes=%v", userTypes)
	}
	successID := userTypes[1][len("type_"):]
	if s, ok := pool.Pick(nil); !ok || s.ID != successID {
		t.Fatalf("pick = %v, want successful account %s", s, successID)
	}
	if _, ok := pool.Pick(map[string]struct{}{successID: {}}); ok {
		t.Fatal("the failed account should be cooling after 403")
	}
}

// 所有账号都 403 → 每个账号各试一次后返回错误，且全部进入冷却。
func TestCallQoderAllAccountsFail(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseEnvelope(403, `{"code":"112","message":"need upgrade"}`))
	}))
	defer srv.Close()

	b, pool := poolBridge(t, srv.URL, "a", "b")
	err := b.CallQoderWithOpts(context.Background(), "codex", testMessages(), "auto", nil, CallOpts{}, func(Delta) {})
	if err == nil {
		t.Fatal("expected error when all accounts fail")
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2 (one per account)", hits)
	}
	if _, ok := pool.Pick(nil); ok {
		t.Fatal("both accounts should be cooling")
	}
}

// 内容审核（不可换号）→ 只打一次上游，不换号。
func TestCallQoderContentPolicyNoFailover(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseEnvelope(500, "InternalError.Algo.DataInspectionFailed: bad content"))
	}))
	defer srv.Close()

	b, _ := poolBridge(t, srv.URL, "a", "b")
	err := b.CallQoderWithOpts(context.Background(), "codex", testMessages(), "auto", nil, CallOpts{}, func(Delta) {})
	if err == nil {
		t.Fatal("expected content policy error")
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (no failover on content policy)", hits)
	}
}

func TestNoAccountErrorMapsTo503(t *testing.T) {
	if got := ErrorStatus(ErrNoAccount); got != 503 {
		t.Errorf("ErrorStatus(ErrNoAccount) = %d, want 503", got)
	}
	msg, typ := FriendlyError(ErrNoAccount)
	if typ != ErrTypeNoAccount || msg == "" {
		t.Errorf("FriendlyError(ErrNoAccount) = (%q, %q)", msg, typ)
	}
}

func TestCooldownForRetryAfterSeconds(t *testing.T) {
	detail := `{"code":"403","message":"{\"code\":\"10605\",\"retryAfterSeconds\":30}"}`
	if got := CooldownFor(NewUpstreamError(403, detail)); got != 30*time.Second {
		t.Errorf("retryAfterSeconds=30 -> %v, want 30s", got)
	}
	if got := CooldownFor(NewUpstreamError(403, `{"code":"112"}`)); got != CooldownQuota {
		t.Errorf("plain 403 -> %v, want %v", got, CooldownQuota)
	}
	// 超大值被截到 5 分钟
	big := `{"retryAfterSeconds":99999}`
	if got := CooldownFor(NewUpstreamError(403, big)); got != 5*time.Minute {
		t.Errorf("huge retryAfter -> %v, want 5m", got)
	}
}

func TestPickSlotForModelRoutesByRegion(t *testing.T) {
	cn := testSlot(t, "cn-slot", "t")
	cn.catalog = map[string]bool{"gmodel": true, "qfmodel": true}
	cn.catalogAt = time.Now()
	gl := testSlot(t, "global-slot", "t")
	gl.catalog = map[string]bool{"qfmodel": true, "ultimate": true}
	gl.catalogAt = time.Now()

	br := NewPoolBridge(NewPool(cn, gl), nil)

	// 区域独占模型 → 必须落到能提供它的区域
	for i := 0; i < 3; i++ {
		s, ok := br.pickSlotForModel("gmodel", nil)
		if !ok || s.ID != "cn-slot" {
			t.Fatalf("gmodel should route to cn-slot, got %v", s)
		}
		s, ok = br.pickSlotForModel("ultimate", nil)
		if !ok || s.ID != "global-slot" {
			t.Fatalf("ultimate should route to global-slot, got %v", s)
		}
	}

	// 两区都有的模型 → 轮询两区
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		if s, ok := br.pickSlotForModel("qfmodel", nil); ok {
			seen[s.ID] = true
		}
	}
	if !seen["cn-slot"] || !seen["global-slot"] {
		t.Fatalf("qfmodel should rotate across both regions, seen=%v", seen)
	}

	// 谁都不认识的模型 → 兜底任意可用账号
	if s, ok := br.pickSlotForModel("nobody-has-this", nil); !ok || s == nil {
		t.Fatalf("unknown model should fall back to any slot, got %v", s)
	}
}

// TestPoolStickyRouting 验证粘性选号：成功账号被优先复用；
// 冷却后回落轮询；关闭粘性后恢复严格轮询。
func TestPoolStickyRouting(t *testing.T) {
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	// 冷启动起点是随机的，先取到哪个就粘哪个
	first, ok := p.Pick(nil)
	if !ok {
		t.Fatal("first pick failed")
	}
	p.NoteSuccess(first.ID)
	// 轮询本应换号，粘性应持续返回同一账号
	for i := 0; i < 3; i++ {
		s, ok := p.Pick(nil)
		if !ok || s.ID != first.ID {
			t.Fatalf("sticky pick #%d = %v, want %s", i, s, first.ID)
		}
	}
	// 该账号冷却 → 回落另一个
	p.Cool(first.ID, time.Minute, "test")
	if s, ok := p.Pick(nil); !ok || s.ID == first.ID {
		t.Fatalf("after cooling %s, pick = %v, want the other account", first.ID, s)
	}

	// 关闭粘性：新池严格轮询
	SetStickyPool(false)
	p2 := NewPool(testSlot(t, "c", "t"), testSlot(t, "d", "t"))
	p2.NoteSuccess("c")
	if s, _ := p2.Pick(nil); s.ID != "c" {
		t.Fatalf("first pick = %v, want c", s)
	}
	if s, _ := p2.Pick(nil); s.ID != "d" {
		t.Fatalf("second pick = %v, want d (rotation, sticky off)", s)
	}
}

// TestPoolColdStartRandomPick 验证冷启动（尚无成功账号）时随机起点，
// 不再是永远第一个；选定并成功后会粘住该账号。
func TestPoolColdStartRandomPick(t *testing.T) {
	oldRand := poolRandIndex
	poolRandIndex = func(n int) int { return 2 }
	t.Cleanup(func() { poolRandIndex = oldRand })
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"), testSlot(t, "c", "t"), testSlot(t, "d", "t"))
	s, ok := p.Pick(nil)
	if !ok || s.ID != "c" {
		t.Fatalf("cold start pick = %v, want c (random index 2)", s)
	}
	p.NoteSuccess("c")
	// 已有成功账号：粘性复用，不再随机
	if s2, _ := p.Pick(nil); s2.ID != "c" {
		t.Fatalf("sticky pick = %v, want c", s2)
	}
}
