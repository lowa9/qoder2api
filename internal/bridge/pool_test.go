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

// TestSlotHasQuotaOrgPackage 验证组织包只有在 available 时才参与额度判断；
// cap<0 的占位包在解析层已被标记为 unavailable。
func TestSlotHasQuotaOrgPackage(t *testing.T) {
	s := testSlot(t, "quota", "teams")
	now := time.Now()

	s.SetQuota(0, 6_000_000, false)
	if s.hasQuota(now) {
		t.Fatal("zero user quota + unavailable org package must be treated as exhausted")
	}

	s.SetQuota(0, 1649, true)
	if !s.hasQuota(now) {
		t.Fatal("available org package should count as quota")
	}

	s.SetQuota(51, 0, false)
	if !s.hasQuota(now) {
		t.Fatal("positive user quota should count as quota")
	}
}

// TestCacheAffinityModelsOnlyIsolated 只有账号隔离缓存的模型才走会话/前缀组粘性。
// TestPoolFixedRouting 固定选号：当前账号优先；只有冷却/无额度才改选。
func TestPoolFixedRouting(t *testing.T) {
	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"), testSlot(t, "c", "t"))
	p.SetQuota("a", 10, 0, false)
	p.SetQuota("b", 100, 0, false)
	p.SetQuota("c", 50, 0, false)

	// 尚未固定：选额度最高的 b，并把它记为当前账号。
	if s, ok := p.Pick(nil); !ok || s.ID != "b" {
		t.Fatalf("initial pick = %v, want b", s)
	}
	// 之后一直用 b，即使 a 额度更高也不换。
	p.SetQuota("a", 999, 0, false)
	for i := 0; i < 3; i++ {
		if s, ok := p.Pick(nil); !ok || s.ID != "b" {
			t.Fatalf("pick #%d = %v, want b", i, s)
		}
	}

	// 手动切换优先。
	p.PinAccount("c")
	if s, ok := p.Pick(nil); !ok || s.ID != "c" {
		t.Fatalf("manual pin pick = %v, want c", s)
	}

	// 当前账号冷却 → 改选额度最高的 a。
	p.Cool("c", time.Minute, "test")
	if s, ok := p.Pick(nil); !ok || s.ID != "a" {
		t.Fatalf("after cooling c, pick = %v, want a", s)
	}
	// 冷却恢复后仍保持新的当前账号 a，不自动跳回。
	p.Uncool("c")
	if s, ok := p.Pick(nil); !ok || s.ID != "a" {
		t.Fatalf("after recovery, pick = %v, want a (current)", s)
	}
}

// TestPoolFixedRoutingSkipsNoQuota 当前账号额度耗尽时改选。
func TestPoolFixedRoutingSkipsNoQuota(t *testing.T) {
	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	p.SetQuota("a", 100, 0, false)
	p.SetQuota("b", 50, 0, false)
	if s, _ := p.Pick(nil); s == nil || s.ID != "a" {
		t.Fatalf("initial pick = %v, want a", s)
	}
	p.SetQuota("a", 0, 0, false) // 额度耗尽
	if s, _ := p.Pick(nil); s == nil || s.ID != "b" {
		t.Fatalf("after quota exhausted, pick = %v, want b", s)
	}
}

// TestBestQuotaIncludesOrgPackage 真实可用的组织包参与“额度最高”排序。
func TestBestQuotaIncludesOrgPackage(t *testing.T) {
	org := testSlot(t, "org", "teams")
	user := testSlot(t, "user", "t")
	org.SetQuota(0, 1000, true)
	user.SetQuota(50, 0, false)
	p := NewPool(org, user)
	if s, _ := p.Pick(nil); s == nil || s.ID != "org" {
		t.Fatalf("best quota pick = %v, want org account", s)
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

	// 两区都有的模型 → 固定当前账号，不再轮询
	first, ok := br.pickSlotForModel("qfmodel", nil)
	if !ok || first == nil {
		t.Fatal("qfmodel pick failed")
	}
	for i := 0; i < 3; i++ {
		if s, ok := br.pickSlotForModel("qfmodel", nil); !ok || s.ID != first.ID {
			t.Fatalf("qfmodel should stay on %s, got %v", first.ID, s)
		}
	}

	// 谁都不认识的模型 → 兜底任意可用账号
	if s, ok := br.pickSlotForModel("nobody-has-this", nil); !ok || s == nil {
		t.Fatalf("unknown model should fall back to any slot, got %v", s)
	}
}

// TestPoolNoRotation 固定选号下不再轮询换号：成功账号持续服务。
func TestPoolNoRotation(t *testing.T) {
	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	for i := 0; i < 5; i++ {
		s, ok := p.Pick(nil)
		if !ok {
			t.Fatalf("pick #%d failed", i)
		}
		p.NoteSuccess(s.ID)
	}
	// 全部冷却时返回 false
	p.Cool("a", time.Minute, "test")
	p.Cool("b", time.Minute, "test")
	if _, ok := p.Pick(nil); ok {
		t.Error("expected no available account when all cooling")
	}
}

// TestPoolUncool 手动解除冷却。
func TestPoolUncool(t *testing.T) {
	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	p.Cool("a", time.Minute, "quota")
	if p.Uncool("missing") {
		t.Error("uncool missing account should return false")
	}
	if !p.Uncool("a") {
		t.Fatal("uncool existing account should return true")
	}
	for _, s := range p.snapshot() {
		if s.ID == "a" && !s.available(time.Now()) {
			t.Error("account a should be available after uncool")
		}
	}
}

// TestSetQuotaReportsIncrease 额度刷新返回前后可用额度，供自动解冷却判断。
func TestSetQuotaReportsIncrease(t *testing.T) {
	s := testSlot(t, "q", "teams")
	before, after := s.SetQuota(0, 1000, true)
	if before != 0 || after != 1000 {
		t.Fatalf("quota before/after = %v/%v, want 0/1000", before, after)
	}
	before, after = s.SetQuota(0, 500, true)
	if before != 1000 || after != 500 {
		t.Fatalf("quota before/after = %v/%v, want 1000/500", before, after)
	}
}

// TestPoolContains 判断账号是否已在池中（新增账号需要重建池）。
func TestPoolContains(t *testing.T) {
	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	if !p.Contains("a") || !p.Contains("b") {
		t.Error("existing accounts should be contained")
	}
	if p.Contains("missing") || p.Contains("") {
		t.Error("missing/empty account should not be contained")
	}
}
