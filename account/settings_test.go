package account

import "testing"

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
