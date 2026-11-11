package bridge

import (
	"strings"
	"testing"
)

func TestRelocateTaskNotifications(t *testing.T) {
	notify := "<system-reminder>\n[SYSTEM NOTIFICATION - NOT USER INPUT]\n<task-notification>done</task-notification>\n</system-reminder>"
	stable := "You are Claude Code.\n<system-reminder>git status: clean</system-reminder>"
	msgs := []interface{}{
		map[string]interface{}{"role": "system", "content": stable + "\n" + notify},
		map[string]interface{}{"role": "user", "content": "继续"},
		map[string]interface{}{"role": "assistant", "content": "好"},
	}
	out := relocateTaskNotifications(msgs)
	if len(out) != 4 {
		t.Fatalf("relocated len = %d, want 4", len(out))
	}
	first, _ := out[0].(map[string]interface{})
	firstText, _ := first["content"].(string)
	if strings.Contains(firstText, "task-notification") {
		t.Error("task notification still in the leading system message")
	}
	if !strings.Contains(firstText, "git status: clean") {
		t.Error("non-notification system reminder should stay in place")
	}
	last, _ := out[len(out)-1].(map[string]interface{})
	lastText, _ := last["content"].(string)
	if last["role"] != "system" || !strings.Contains(lastText, "task-notification") {
		t.Errorf("trailing message = %+v, want system task notification", last)
	}

	clean := []interface{}{
		map[string]interface{}{"role": "system", "content": stable},
		map[string]interface{}{"role": "user", "content": "继续"},
		map[string]interface{}{"role": "assistant", "content": "好"},
	}

	// 稳定 system + 历史消息的前缀哈希应与“从未出现通知”时一致。
	if a, b := deriveSessionKey(msgs), deriveSessionKey(clean); a != b {
		t.Error("session key should already ignore reminder-only changes")
	}
	if a, b := deriveSessionKey(out), deriveSessionKey(clean); a != b {
		t.Errorf("session key changed after relocation: %q vs %q", a, b)
	}

	// 多个通知合并成一条末尾 system 消息。
	two := []interface{}{
		map[string]interface{}{"role": "system", "content": "SYS\n" + notify + "\n" + notify},
		map[string]interface{}{"role": "user", "content": "hi"},
	}
	out2 := relocateTaskNotifications(two)
	if len(out2) != 3 {
		t.Fatalf("two notifications should merge into one trailing message, len=%d", len(out2))
	}
	if c, _ := out2[2].(map[string]interface{})["content"].(string); strings.Count(c, "<task-notification>") != 2 {
		t.Errorf("trailing message should contain both notifications: %q", c)
	}

	// 关闭开关时保持原样。
	t.Setenv("QODER2API_RELOCATE_NOTIFICATIONS", "off")
	if out3 := relocateTaskNotifications(msgs); len(out3) != len(msgs) {
		t.Errorf("disabled relocation changed messages: %d vs %d", len(out3), len(msgs))
	}
}

func TestRelocateTaskNotificationsWithoutSystem(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
	}
	out := relocateTaskNotifications(msgs)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
}

// TestStripBillingHeaderFromMessages 客户端计费头不应进入上游 system，
// 否则 Qwen/Kimi 的前缀缓存每次都会失效。
func TestStripBillingHeaderFromMessages(t *testing.T) {
	hdr := "x-anthropic-billing-header: cc_version=2.1.280.096; cc_entrypoint=cli; cch=64dfe;"
	msgs := []interface{}{
		map[string]interface{}{"role": "system", "content": hdr + "\nYou are Claude Code."},
		map[string]interface{}{"role": "user", "content": "hi"},
	}
	out := stripBillingHeaderFromMessages(msgs)
	sys, _ := out[0].(map[string]interface{})["content"].(string)
	if strings.Contains(sys, "cch=") || strings.Contains(sys, "billing-header") {
		t.Errorf("billing header still present: %q", sys)
	}
	if sys != "You are Claude Code." {
		t.Errorf("content = %q", sys)
	}
	// 原始切片不被修改（上游 body 之外仍用于日志/指纹）
	orig, _ := msgs[0].(map[string]interface{})["content"].(string)
	if !strings.Contains(orig, "cch=") {
		t.Error("input messages must not be mutated")
	}

	// 只有计费头的 system 消息应整体丢弃。
	only := []interface{}{
		map[string]interface{}{"role": "system", "content": hdr},
		map[string]interface{}{"role": "user", "content": "hi"},
	}
	if out := stripBillingHeaderFromMessages(only); len(out) != 1 {
		t.Errorf("header-only system message should be dropped, got %d messages", len(out))
	}

	// 中间出现的 cch token 也要去掉。
	inline := []interface{}{
		map[string]interface{}{"role": "system", "content": "You are Claude Code. cch=abc12; stable"},
	}
	if s, _ := stripBillingHeaderFromMessages(inline)[0].(map[string]interface{})["content"].(string); strings.Contains(s, "cch=") {
		t.Errorf("inline cch not stripped: %q", s)
	}

	// 无计费头时返回原切片，不做无谓复制。
	plain := []interface{}{map[string]interface{}{"role": "system", "content": "plain system"}}
	if got := stripBillingHeaderFromMessages(plain); &got[0] != &plain[0] {
		t.Error("plain messages should be returned as-is")
	}
}
