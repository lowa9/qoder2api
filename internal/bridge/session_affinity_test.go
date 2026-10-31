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
