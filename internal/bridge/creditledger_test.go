package bridge

import (
	"path/filepath"
	"testing"
	"time"
)

// withLedger 把全局账本替换为指定路径的临时账本，测试结束恢复。
func withLedger(t *testing.T, path string) *creditLedger {
	t.Helper()
	old := creditBook
	l := &creditLedger{path: path}
	creditBook = l
	t.Cleanup(func() { creditBook = old })
	return l
}

func TestCreditLedgerRecordAndSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit_history.json")
	withLedger(t, path)

	now := time.Now()
	RecordCredit(CreditEntry{Time: now, Account: "acct-a", AccountID: "id-a", Region: "cn", Model: "auto", Credits: 0.5, PromptTokens: 10000, CachedTokens: 9000, OutputTokens: 100})
	RecordCredit(CreditEntry{Time: now, Account: "acct-a", AccountID: "id-a", Region: "cn", Model: "auto", Credits: 0.25, PromptTokens: 5000, CachedTokens: 0, OutputTokens: 50})
	RecordCredit(CreditEntry{Time: now, Account: "acct-b", AccountID: "id-b", Region: "cn", Model: "dfmodel", Credits: 0.1, PromptTokens: 2000, CachedTokens: 1900, OutputTokens: 20})
	// credits=0（免费模型）不入账
	RecordCredit(CreditEntry{Time: now, Account: "acct-b", AccountID: "id-b", Model: "qfmodel", Credits: 0})

	s := CreditSummary()
	if s.Entries != 3 {
		t.Fatalf("entries = %d, want 3", s.Entries)
	}
	if s.Totals.Requests != 3 || s.Totals.Credits < 0.849 || s.Totals.Credits > 0.851 {
		t.Errorf("totals = %+v", s.Totals)
	}
	if s.Totals.PromptTokens != 17000 || s.Totals.CachedTokens != 10900 {
		t.Errorf("tokens = %d/%d, want 17000/10900", s.Totals.PromptTokens, s.Totals.CachedTokens)
	}
	if s.Totals.CacheMissTokens != 6100 {
		t.Errorf("cache miss = %d, want 6100", s.Totals.CacheMissTokens)
	}
	wantPer1M := 0.85 / 17000 * 1e6
	if diff := s.Totals.CreditsPer1MTok - wantPer1M; diff > 0.01 || diff < -0.01 {
		t.Errorf("per 1M = %f, want %f", s.Totals.CreditsPer1MTok, wantPer1M)
	}

	if len(s.ByAccount) != 2 {
		t.Fatalf("by_account = %d, want 2", len(s.ByAccount))
	}
	// 按积分降序：acct-a 0.75 在前
	if s.ByAccount[0].Nickname != "acct-a" || s.ByAccount[0].Requests != 2 {
		t.Errorf("first account = %+v", s.ByAccount[0])
	}
	if len(s.ByModel) != 2 || s.ByModel[0].Key != "auto" {
		t.Errorf("by_model = %+v", s.ByModel)
	}
}

func TestCreditLedgerPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credit_history.json")
	withLedger(t, path)
	RecordCredit(CreditEntry{Time: time.Now(), Account: "acct-a", AccountID: "id-a", Model: "auto", Credits: 1.25, PromptTokens: 100, CachedTokens: 50})

	// 模拟重启：同一路径的新账本应恢复记录
	l2 := withLedger(t, path)
	l2.loaded = false
	s := CreditSummary()
	if s.Entries != 1 || s.Totals.Credits != 1.25 {
		t.Fatalf("reloaded summary = %+v", s)
	}
	if len(s.Recent) != 1 || s.Recent[0].Account != "acct-a" {
		t.Errorf("recent = %+v", s.Recent)
	}
}

func TestCreditLedgerCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit_history.json")
	l := &creditLedger{path: path}
	for i := 0; i < creditLedgerCapacity+10; i++ {
		l.record(CreditEntry{Time: time.Now(), Account: "a", AccountID: "id", Model: "auto", Credits: 0.001})
	}
	if len(l.entries) != creditLedgerCapacity {
		t.Fatalf("entries = %d, want %d", len(l.entries), creditLedgerCapacity)
	}
}
