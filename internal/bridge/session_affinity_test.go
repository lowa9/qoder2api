package bridge

import (
	"testing"
	"time"
)

// TestDeriveSessionKey 验证会话标识稳定且区分不同会话。
func TestDeriveSessionKey(t *testing.T) {
	base := []interface{}{
		map[string]interface{}{"role": "system", "content": "你是助手"},
		map[string]interface{}{"role": "user", "content": "帮我修 bug"},
		map[string]interface{}{"role": "assistant", "content": "好的"},
	}
	k1 := deriveSessionKey(base)
	k2 := deriveSessionKey(base)
	if k1 == "" || k1 != k2 {
		t.Fatalf("session key not stable: %q vs %q", k1, k2)
	}
	other := []interface{}{
		map[string]interface{}{"role": "system", "content": "你是助手"},
		map[string]interface{}{"role": "user", "content": "另一个任务"},
	}
	if k3 := deriveSessionKey(other); k3 == k1 || k3 == "" {
		t.Fatalf("different conversations should differ: %q vs %q", k1, k3)
	}
	// 后续轮次追加消息不应改变键（首条 user 未变）
	grown := append([]interface{}{}, base...)
	grown = append(grown, map[string]interface{}{"role": "user", "content": "继续"})
	if k4 := deriveSessionKey(grown); k4 != k1 {
		t.Fatalf("grown conversation changed key: %q vs %q", k4, k1)
	}
	if deriveSessionKey(nil) != "" {
		t.Error("empty messages should yield empty key")
	}
}

func TestSessionKeyFromBody(t *testing.T) {
	cases := []struct {
		name string
		req  map[string]interface{}
		want string
	}{
		{"conversation_id", map[string]interface{}{"conversation_id": "c1"}, "conv:c1"},
		{"conversationId", map[string]interface{}{"conversationId": "c2"}, "conv:c2"},
		{"metadata user_id", map[string]interface{}{"metadata": map[string]interface{}{"user_id": "u1"}}, "meta:u1"},
		{"empty", map[string]interface{}{}, ""},
	}
	for _, c := range cases {
		if got := sessionKeyFromBody(c.req); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestPoolSessionAffinity 验证不同会话首次随机分散、后续各自粘住；
// 账号冷却后该会话重新选号并更新绑定。
func TestPoolSessionAffinity(t *testing.T) {
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	oldRand := poolRandIndex
	seq := []int{0, 1}
	i := 0
	poolRandIndex = func(n int) int {
		v := seq[i%len(seq)]
		i++
		return v % n
	}
	t.Cleanup(func() { poolRandIndex = oldRand })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"), testSlot(t, "c", "t"))

	sa, ok := p.PickWithSession("sessA", nil)
	if !ok || sa.ID != "a" {
		t.Fatalf("session A first pick = %v, want a", sa)
	}
	p.NoteSessionSuccess("sessA", sa.ID)

	sb, ok := p.PickWithSession("sessB", nil)
	if !ok || sb.ID != "b" {
		t.Fatalf("session B first pick = %v, want b", sb)
	}
	p.NoteSessionSuccess("sessB", sb.ID)

	// 两个会话各自粘住
	if s, _ := p.PickWithSession("sessA", nil); s.ID != "a" {
		t.Fatalf("session A sticky = %v, want a", s)
	}
	if s, _ := p.PickWithSession("sessB", nil); s.ID != "b" {
		t.Fatalf("session B sticky = %v, want b", s)
	}

	// a 冷却 → 会话 A 重新选号并更新绑定
	p.Cool("a", time.Minute, "test")
	s2, ok := p.PickWithSession("sessA", nil)
	if !ok || s2.ID == "a" {
		t.Fatalf("session A after cooling = %v, want another account", s2)
	}
	p.NoteSessionSuccess("sessA", s2.ID)
	if s3, _ := p.PickWithSession("sessA", nil); s3.ID != s2.ID {
		t.Fatalf("session A new binding = %v, want %s", s3, s2.ID)
	}
}

// TestDerivePrefixGroup 验证前缀组只由 system + 模型决定：
// 同一 system 下不同 user 属于同一组，不同 system/model 不同组。
func TestDerivePrefixGroup(t *testing.T) {
	a := []interface{}{
		map[string]interface{}{"role": "system", "content": "同一个系统提示"},
		map[string]interface{}{"role": "user", "content": "任务一"},
	}
	b := []interface{}{
		map[string]interface{}{"role": "system", "content": "同一个系统提示"},
		map[string]interface{}{"role": "user", "content": "任务二"},
	}
	c := []interface{}{
		map[string]interface{}{"role": "system", "content": "另一个系统提示"},
		map[string]interface{}{"role": "user", "content": "任务一"},
	}
	g1 := derivePrefixGroup(a, "dfmodel")
	if g1 == "" {
		t.Fatal("group should not be empty with system prompt")
	}
	if g2 := derivePrefixGroup(b, "dfmodel"); g2 != g1 {
		t.Errorf("same system should share group: %q vs %q", g1, g2)
	}
	if g3 := derivePrefixGroup(c, "dfmodel"); g3 == g1 {
		t.Errorf("different system should differ: %q", g3)
	}
	if g4 := derivePrefixGroup(a, "auto"); g4 == g1 {
		t.Errorf("different model should differ: %q", g4)
	}
	noSystem := []interface{}{map[string]interface{}{"role": "user", "content": "hi"}}
	if g := derivePrefixGroup(noSystem, "dfmodel"); g != "" {
		t.Errorf("no system should yield empty group, got %q", g)
	}
}

// TestPoolPrefixGroupAffinity 验证同一 system 的新会话复用账号，
// 不同 system 走新的随机起点。
func TestPoolPrefixGroupAffinity(t *testing.T) {
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	oldRand := poolRandIndex
	seq := []int{0, 1}
	i := 0
	poolRandIndex = func(n int) int {
		v := seq[i%len(seq)]
		i++
		return v % n
	}
	t.Cleanup(func() { poolRandIndex = oldRand })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"), testSlot(t, "c", "t"))

	s1, ok := p.PickWithAffinity("sess1", "G1", nil)
	if !ok || s1.ID != "a" {
		t.Fatalf("first pick = %v, want a", s1)
	}
	p.NoteSessionSuccess("sess1", s1.ID)
	p.NoteGroupSuccess("G1", s1.ID)

	// 新会话、同前缀组 → 复用 a（不再随机）
	if s2, _ := p.PickWithAffinity("sess2", "G1", nil); s2.ID != "a" {
		t.Fatalf("same group pick = %v, want a", s2)
	}

	// 新前缀组 → 下一个随机起点 b
	if s3, _ := p.PickWithAffinity("sess3", "G2", nil); s3.ID != "b" {
		t.Fatalf("different group pick = %v, want b", s3)
	}
}

// TestPoolPinAccount 验证手动固定优先级最高，取消后恢复自动选号，
// 固定账号冷却时回退。
func TestPoolPinAccount(t *testing.T) {
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	oldRand := poolRandIndex
	poolRandIndex = func(n int) int { return 0 }
	t.Cleanup(func() { poolRandIndex = oldRand })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	p.NoteSessionSuccess("sess", "a")
	p.NoteGroupSuccess("G", "a")

	p.PinAccount("b")
	if s, _ := p.PickWithAffinity("sess", "G", nil); s.ID != "b" {
		t.Fatalf("pinned pick = %v, want b", s)
	}
	// 状态里能看出 pinned
	for _, st := range p.Status() {
		if st.ID == "b" && !st.Pinned {
			t.Error("status should mark b as pinned")
		}
	}

	p.UnpinAccount()
	if s, _ := p.PickWithAffinity("sess", "G", nil); s.ID != "a" {
		t.Fatalf("after unpin = %v, want session binding a", s)
	}

	p.PinAccount("b")
	p.Cool("b", time.Minute, "test")
	if s, ok := p.PickWithAffinity("sess", "G", nil); !ok || s.ID != "a" {
		t.Fatalf("cooling pinned should fall back = %v, want a", s)
	}
}

// TestPoolPinnedSkipsAffinity 验证固定期间不写入会话/前缀组绑定，
// 取消固定后同一会话/前缀组会重新随机，而不是继续粘在原账号。
func TestPoolPinnedSkipsAffinity(t *testing.T) {
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	oldRand := poolRandIndex
	poolRandIndex = func(n int) int { return 0 }
	t.Cleanup(func() { poolRandIndex = oldRand })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	p.PinAccount("b")
	s, _ := p.PickWithAffinity("sess", "G", nil)
	if s.ID != "b" {
		t.Fatalf("pinned pick = %v, want b", s)
	}
	p.NoteSessionSuccess("sess", s.ID)
	p.NoteGroupSuccess("G", s.ID)

	p.mu.Lock()
	nSessions, nGroups := len(p.sessions), len(p.groups)
	p.mu.Unlock()
	if nSessions != 0 || nGroups != 0 {
		t.Fatalf("bindings should be skipped while pinned: sessions=%d groups=%d", nSessions, nGroups)
	}

	p.UnpinAccount()
	// 随机源返回 0 → 取消固定后重新随机到 a
	if s2, _ := p.PickWithAffinity("sess", "G", nil); s2.ID != "a" {
		t.Fatalf("after unpin = %v, want a (random again)", s2)
	}
}
