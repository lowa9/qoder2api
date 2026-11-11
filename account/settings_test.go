package account

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEffectiveQueueWaitFirstToken(t *testing.T) {
	// 未设置（nil）→ 默认开
	s := &Settings{}
	if !s.EffectiveQueueWaitFirstToken() {
		t.Error("nil should default to true (ON)")
	}
	// 显式 true
	tr := true
	s.QueueWaitFirstToken = &tr
	if !s.EffectiveQueueWaitFirstToken() {
		t.Error("explicit true should be true")
	}
	// 显式 false
	fa := false
	s.QueueWaitFirstToken = &fa
	if s.EffectiveQueueWaitFirstToken() {
		t.Error("explicit false should be false")
	}
}

func TestEffectiveStickyPool(t *testing.T) {
	if !(&Settings{}).EffectiveStickyPool() {
		t.Error("nil (unset) should default to true")
	}
	tr := true
	if !(&Settings{StickyPool: &tr}).EffectiveStickyPool() {
		t.Error("explicit true should be true")
	}
	fa := false
	if (&Settings{StickyPool: &fa}).EffectiveStickyPool() {
		t.Error("explicit false should be false")
	}
}

// TestSaveSettingsAtomic settings.json 使用临时文件 + rename，读者不会看到半截 JSON。
func TestSaveSettingsAtomic(t *testing.T) {
	SetDataRoot(t.TempDir())
	want := &Settings{Port: 4321, LogLevel: "debug"}
	if err := SaveSettings(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadSettings()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Port != 4321 || got.LogLevel != "debug" {
		t.Fatalf("roundtrip = port %d level %q", got.Port, got.LogLevel)
	}
	info, err := os.Stat(settingsPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("settings mode = %o, want 600", perm)
	}
	entries, err := os.ReadDir(filepath.Dir(settingsPath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".settings-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}
