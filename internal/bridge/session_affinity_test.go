package bridge

import (
	"testing"
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
