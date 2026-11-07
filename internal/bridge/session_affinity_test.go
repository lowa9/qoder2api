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

// TestDeriveSessionKeyIgnoresSystemReminders 验证动态提醒块不会拆分会话键。
func TestDeriveSessionKeyIgnoresSystemReminders(t *testing.T) {
	mk := func(reminder string) []interface{} {
		return []interface{}{
			map[string]interface{}{"role": "system", "content": "你是助手"},
			map[string]interface{}{
				"role":    "user",
				"content": "<system-reminder>" + reminder + "</system-reminder>\n帮我修 bug",
			},
		}
	}
	k1 := deriveSessionKey(mk("git status: clean"))
	k2 := deriveSessionKey(mk("git status: 2 files changed"))
	if k1 == "" || k1 != k2 {
		t.Fatalf("volatile reminder changed session key: %q vs %q", k1, k2)
	}
	// 真正的首条用户内容变化仍必须产生新键。
	other := []interface{}{
		map[string]interface{}{"role": "system", "content": "你是助手"},
		map[string]interface{}{"role": "user", "content": "<system-reminder>x</system-reminder>\n另一个任务"},
	}
	if k3 := deriveSessionKey(other); k3 == k1 {
		t.Fatal("different user prompt must not share session key")
	}
	// 首条消息只有提醒时，继续使用后面真正的用户消息。
	onlyReminder := []interface{}{
		map[string]interface{}{"role": "system", "content": "你是助手"},
		map[string]interface{}{"role": "user", "content": "<system-reminder>临时上下文</system-reminder>"},
		map[string]interface{}{"role": "user", "content": "真正的任务"},
	}
	rawPrompt := []interface{}{
		map[string]interface{}{"role": "system", "content": "你是助手"},
		map[string]interface{}{"role": "user", "content": "真正的任务"},
	}
	if k4, k5 := deriveSessionKey(onlyReminder), deriveSessionKey(rawPrompt); k4 != k5 {
		t.Fatalf("reminder-only prefix should fall through: %q vs %q", k4, k5)
	}
	// system 里的动态提醒同样不应改变会话键。
	sysA := []interface{}{
		map[string]interface{}{"role": "system", "content": "<system-reminder>git clean</system-reminder>你是助手"},
		map[string]interface{}{"role": "user", "content": "帮我修 bug"},
	}
	sysB := []interface{}{
		map[string]interface{}{"role": "system", "content": "<system-reminder>git dirty</system-reminder>你是助手"},
		map[string]interface{}{"role": "user", "content": "帮我修 bug"},
	}
	if a, b := deriveSessionKey(sysA), deriveSessionKey(sysB); a != b {
		t.Fatalf("system reminder changed session key: %q vs %q", a, b)
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

// TestPoolCurrentIsDefaultNotOverride 验证当前路由账号是默认/兜底，
// 已有会话/前缀组绑定仍然优先。
func TestPoolCurrentIsDefaultNotOverride(t *testing.T) {
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	p.SetQuota("a", 500, 0, false)
	p.SetQuota("b", 500, 0, false)
	p.NoteSessionSuccess("sess", "a")
	p.NoteGroupSuccess("G", "a")
	p.PinAccount("b")

	if s, _ := p.PickWithAffinity("sess", "G", nil); s.ID != "a" {
		t.Fatalf("bound session = %v, want a (binding wins)", s)
	}
	// 没有绑定（新会话/新前缀组）才用当前路由账号
	if s, _ := p.PickWithAffinity("new", "G2", nil); s.ID != "b" {
		t.Fatalf("unbound session = %v, want current b", s)
	}
}

// TestPoolCurrentFallbackWhenBindingNoQuota 验证绑定账号没额度时，
// 先用当前路由账号，再退到额度最高的账号。
func TestPoolCurrentFallbackWhenBindingNoQuota(t *testing.T) {
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"), testSlot(t, "c", "t"))
	p.NoteSessionSuccess("sess", "a")
	p.NoteGroupSuccess("G", "a")
	p.PinAccount("b")
	p.SetQuota("a", 0, 0, false)
	p.SetQuota("b", 500, 0, false)
	p.SetQuota("c", 900, 0, false)

	if s, _ := p.PickWithAffinity("sess", "G", nil); s.ID != "b" {
		t.Fatalf("fallback = %v, want current b (has quota)", s)
	}

	// 当前账号也没额度 → 自动选额度最高的 c，并把当前账号改为 c（自动）
	p.SetQuota("b", 0, 0, false)
	if s, _ := p.PickWithAffinity("sess", "G", nil); s.ID != "c" {
		t.Fatalf("auto fallback = %v, want c (highest quota)", s)
	}
	found := false
	for _, st := range p.Status() {
		if st.ID == "c" {
			found = st.Pinned && !st.PinnedManual
		}
	}
	if !found {
		t.Error("current routing account should switch to c (auto)")
	}
}

// TestPoolCurrentIdleExpiryAutoSelectsHighest 验证当前账号闲置过期后
// 立即改选额度最高的账号（自动），而不是清空等待。
func TestPoolCurrentIdleExpiryAutoSelectsHighest(t *testing.T) {
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	p.SetQuota("a", 500, 0, false)
	p.SetQuota("b", 100, 0, false)
	p.PinAccount("b")
	p.mu.Lock()
	p.pinnedAt = time.Now().Add(-pinIdleTTL - time.Minute)
	p.mu.Unlock()

	if s, _ := p.PickWithAffinity("new", "G", nil); s.ID != "a" {
		t.Fatalf("expired current = %v, want a (highest quota)", s)
	}
	found := false
	for _, st := range p.Status() {
		if st.ID == "a" {
			found = st.Pinned && !st.PinnedManual
		}
	}
	if !found {
		t.Error("current should auto-switch to a")
	}
}

// TestPoolAutoSelectCurrent 验证「自动选号」立即切到额度最高的账号。
func TestPoolAutoSelectCurrent(t *testing.T) {
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"), testSlot(t, "c", "t"))
	p.SetQuota("a", 100, 0, false)
	p.SetQuota("b", 500, 0, false)
	p.SetQuota("c", 300, 0, false)

	if id := p.AutoSelectCurrent(); id != "b" {
		t.Fatalf("auto current = %q, want b", id)
	}
	if s, _ := p.PickWithAffinity("new", "G", nil); s.ID != "b" {
		t.Fatalf("pick = %v, want b", s)
	}
}

// TestPoolBindingsWrittenWithCurrent 验证当前账号只是兜底，绑定仍会正常写入。
func TestPoolBindingsWrittenWithCurrent(t *testing.T) {
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	p.PinAccount("b")
	s, _ := p.PickWithAffinity("sess", "G", nil)
	if s.ID != "b" {
		t.Fatalf("pick = %v, want b", s)
	}
	p.NoteSessionSuccess("sess", s.ID)
	p.NoteGroupSuccess("G", s.ID)
	p.mu.Lock()
	nSessions, nGroups := len(p.sessions), len(p.groups)
	p.mu.Unlock()
	if nSessions == 0 || nGroups == 0 {
		t.Fatalf("bindings should be written: sessions=%d groups=%d", nSessions, nGroups)
	}
}

// TestPoolSelectsHighestQuota 验证没有当前账号时按剩余额度最高选号。
func TestPoolSelectsHighestQuota(t *testing.T) {
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"), testSlot(t, "c", "t"))
	p.SetQuota("a", 100, 0, false)
	p.SetQuota("b", 500, 0, false)
	p.SetQuota("c", 300, 0, false)

	s, ok := p.PickWithAffinity("new-1", "G1", nil)
	if !ok || s.ID != "b" {
		t.Fatalf("highest quota pick = %v, want b", s)
	}
	p.Cool("b", time.Minute, "test")
	s2, ok := p.PickWithAffinity("new-2", "G2", nil)
	if !ok || s2.ID != "c" {
		t.Fatalf("after cooling b = %v, want c", s2)
	}
}

// TestPoolCurrentRefreshOnSuccess 验证当前账号有成功请求时续期，不会闲置过期。
func TestPoolCurrentRefreshOnSuccess(t *testing.T) {
	SetStickyPool(true)
	t.Cleanup(func() { SetStickyPool(true) })

	p := NewPool(testSlot(t, "a", "t"), testSlot(t, "b", "t"))
	p.SetQuota("a", 500, 0, false)
	p.SetQuota("b", 100, 0, false)
	p.PinAccount("b")
	p.mu.Lock()
	p.pinnedAt = time.Now().Add(-pinIdleTTL - time.Minute)
	p.mu.Unlock()
	p.NoteSuccess("b") // 成功请求刷新闲置时间

	if s, ok := p.PickWithAffinity("new", "G", nil); !ok || s.ID != "b" {
		t.Fatalf("refreshed current = %v, want b", s)
	}
}

// TestDerivePrefixGroupIgnoresReminders 动态提醒不应把同一 system 拆成多个前缀组。
func TestDerivePrefixGroupIgnoresReminders(t *testing.T) {
	mk := func(rem string) []interface{} {
		return []interface{}{
			map[string]interface{}{"role": "system", "content": "You are Claude Code.\n<system-reminder>" + rem + "</system-reminder>"},
			map[string]interface{}{"role": "user", "content": "任务"},
		}
	}
	g1 := derivePrefixGroup(mk("git status: clean"), "dfmodel")
	g2 := derivePrefixGroup(mk("git status: 3 files changed"), "dfmodel")
	if g1 == "" || g1 != g2 {
		t.Fatalf("reminder-only change split the group: %q vs %q", g1, g2)
	}
	other := []interface{}{
		map[string]interface{}{"role": "system", "content": "You are Claude Code.\nExtra stable instruction."},
		map[string]interface{}{"role": "user", "content": "任务"},
	}
	if g3 := derivePrefixGroup(other, "dfmodel"); g3 == g1 {
		t.Fatal("real system change must produce a new group")
	}
}
