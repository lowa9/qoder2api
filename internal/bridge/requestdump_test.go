package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qoder2api/account"
)

func TestFingerprintMessages(t *testing.T) {
	msgsA := []interface{}{
		map[string]interface{}{"role": "system", "content": "<system-reminder>git clean</system-reminder>你是助手"},
		map[string]interface{}{"role": "user", "content": "任务"},
	}
	msgsB := []interface{}{
		map[string]interface{}{"role": "system", "content": "<system-reminder>git dirty</system-reminder>你是助手"},
		map[string]interface{}{"role": "user", "content": "任务"},
	}
	a := fingerprintMessages(msgsA)
	b := fingerprintMessages(msgsB)
	if a.systemSHA == "" || a.systemNormSHA == "" || a.firstUserSHA == "" {
		t.Fatalf("fingerprint incomplete: %+v", a)
	}
	if a.systemSHA == b.systemSHA {
		t.Error("different raw system prompts should hash differently")
	}
	if a.systemNormSHA != b.systemNormSHA {
		t.Error("reminder-only changes should normalize to the same system hash")
	}
	if a.firstUserSHA != shortSHA("任务") || a.firstUserNormSHA != shortSHA("任务") {
		t.Errorf("first user hash = %q / %q", a.firstUserSHA, a.firstUserNormSHA)
	}
}

func TestDumpRequestBodyOnSystemChange(t *testing.T) {
	oldRoot := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	t.Cleanup(func() { account.SetDataRoot(oldRoot) })

	ip := "dump-test-" + t.Name()
	mk := func(sys string) []interface{} {
		return []interface{}{
			map[string]interface{}{"role": "system", "content": sys},
			map[string]interface{}{"role": "user", "content": "任务"},
		}
	}
	body := func(s string) []byte { return []byte(`{"marker":"` + s + `"}`) }

	DumpRequestBody("test", "r1", "model-a", ip, body("old"), mk("系统 A"))
	DumpRequestBody("test", "r2", "model-a", ip, body("same"), mk("系统 A"))
	files, _ := filepath.Glob(filepath.Join(account.DataRoot(), "request_dumps", "*.json"))
	if len(files) != 1 {
		t.Fatalf("same system should dump once, got %d files", len(files))
	}

	DumpRequestBody("test", "r3", "model-a", ip, body("new"), mk("系统 B"))
	files, _ = filepath.Glob(filepath.Join(account.DataRoot(), "request_dumps", "*.json"))
	if len(files) != 2 {
		t.Fatalf("system change should add one dump, got %d files", len(files))
	}
	changes, _ := filepath.Glob(filepath.Join(account.DataRoot(), "request_dumps", "changes", "*.json"))
	if len(changes) != 2 {
		t.Fatalf("system change should preserve prev+new, got %d files", len(changes))
	}
	changeDir := filepath.Join(account.DataRoot(), "request_dumps", "changes")
	entries, _ := os.ReadDir(changeDir)
	contents := map[string]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(changeDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(e.Name(), "-prev.json") {
			contents["prev"] = string(data)
		} else {
			contents["new"] = string(data)
		}
	}
	if contents["prev"] != `{"marker":"old"}` {
		t.Errorf("preserved prev body = %q", contents["prev"])
	}
	if contents["new"] != `{"marker":"new"}` {
		t.Errorf("preserved new body = %q", contents["new"])
	}

	DumpRequestBody("test", "r4", "model-a", ip, body("same2"), mk("系统 B"))
	files, _ = filepath.Glob(filepath.Join(account.DataRoot(), "request_dumps", "*.json"))
	if len(files) != 2 {
		t.Fatalf("known prefix should not dump again, got %d files", len(files))
	}

	// system 不变、首条用户消息变化（新会话 / 重写前缀）同样应落盘。
	newSession := []interface{}{
		map[string]interface{}{"role": "system", "content": "系统 B"},
		map[string]interface{}{"role": "user", "content": "另一个任务"},
	}
	DumpRequestBody("test", "r5", "model-a", ip, body("new-session"), newSession)
	files, _ = filepath.Glob(filepath.Join(account.DataRoot(), "request_dumps", "*.json"))
	if len(files) != 3 {
		t.Fatalf("new first user should dump, got %d files", len(files))
	}
}
