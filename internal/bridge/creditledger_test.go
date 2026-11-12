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

// TestCreditSummaryRangeBuckets 时间窗口只统计窗口内记录，序列按小时分桶并保留空桶。
func TestCreditSummaryRangeBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit_history.json")
	withLedger(t, path)

	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.Local)
	RecordCredit(CreditEntry{Time: base.Add(10 * time.Minute), Account: "a", AccountID: "id-a", Model: "auto", Credits: 1, PromptTokens: 100, CachedTokens: 40, OutputTokens: 10})
	RecordCredit(CreditEntry{Time: base.Add(70 * time.Minute), Account: "a", AccountID: "id-a", Model: "auto", Credits: 2, PromptTokens: 200, OutputTokens: 20})
	// 窗口外（更晚）：不得进入 totals / series
	RecordCredit(CreditEntry{Time: base.Add(3 * time.Hour), Account: "b", AccountID: "id-b", Model: "dfmodel", Credits: 4, PromptTokens: 400, OutputTokens: 40})

	s := CreditSummaryRange(CreditRange{From: base, To: base.Add(2 * time.Hour), Bucket: time.Hour})
	if s.Entries != 2 || s.AllEntries != 3 {
		t.Fatalf("entries = %d/%d, want 2/3", s.Entries, s.AllEntries)
	}
	if s.Totals.Credits != 3 || s.Totals.PromptTokens != 300 || s.Totals.OutputTokens != 30 {
		t.Errorf("totals = %+v", s.Totals)
	}
	if len(s.ByAccount) != 1 || len(s.ByModel) != 1 {
		t.Errorf("by_account/by_model should only include window: %+v %+v", s.ByAccount, s.ByModel)
	}
	// 00、01、02 三个桶；02 为空桶。
	if len(s.Series) != 3 || s.Buckets != 3 || s.BucketSeconds != 3600 {
		t.Fatalf("series = %d buckets (bucket=%d), want 3x3600", len(s.Series), s.BucketSeconds)
	}
	if s.Series[0].PromptTokens != 100 || s.Series[0].CachedTokens != 40 || s.Series[0].OutputTokens != 10 || s.Series[0].Requests != 1 {
		t.Errorf("bucket 0 = %+v", s.Series[0])
	}
	if s.Series[1].PromptTokens != 200 || s.Series[1].Credits != 2 {
		t.Errorf("bucket 1 = %+v", s.Series[1])
	}
	if s.Series[2].Requests != 0 || s.Series[2].PromptTokens != 0 {
		t.Errorf("bucket 2 should be empty: %+v", s.Series[2])
	}
	if got := s.Series[0].T; !got.Equal(base) {
		t.Errorf("bucket 0 start = %v, want %v", got, base)
	}
	if s.From == "" || s.To == "" {
		t.Errorf("window echo missing: from=%q to=%q", s.From, s.To)
	}
}

// TestCreditSummaryRangeDayBucket 天级桶按本地日历日对齐（不是 UTC 零点）。
func TestCreditSummaryRangeDayBucket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit_history.json")
	withLedger(t, path)

	day := time.Date(2026, 11, 10, 0, 0, 0, 0, time.Local)
	RecordCredit(CreditEntry{Time: day.Add(9 * time.Hour), Account: "a", AccountID: "id-a", Model: "auto", Credits: 1, PromptTokens: 100})
	RecordCredit(CreditEntry{Time: day.Add(20 * time.Hour), Account: "a", AccountID: "id-a", Model: "auto", Credits: 2, PromptTokens: 200})
	RecordCredit(CreditEntry{Time: day.AddDate(0, 0, 1).Add(2 * time.Hour), Account: "a", AccountID: "id-a", Model: "auto", Credits: 3, PromptTokens: 300})

	s := CreditSummaryRange(CreditRange{From: day, To: day.AddDate(0, 0, 2), Bucket: 24 * time.Hour})
	if len(s.Series) != 3 { // 11-10、11-11、11-12（末日空桶）
		t.Fatalf("buckets = %d, want 3", len(s.Series))
	}
	if !s.Series[0].T.Equal(day) || s.Series[1].T.Day() != day.AddDate(0, 0, 1).Day() {
		t.Errorf("day bucket starts = %v, %v", s.Series[0].T, s.Series[1].T)
	}
	if s.Series[0].PromptTokens != 300 || s.Series[1].PromptTokens != 300 || s.Series[2].PromptTokens != 0 {
		t.Errorf("day buckets = %+v", s.Series)
	}
}

// TestCreditSummaryRangeAllStartsAtFirstEntry From 为空（全部历史）时序列从最早记录
// 所在桶开始，而不是从零值时间开始生成海量空桶。
func TestCreditSummaryRangeAllStartsAtFirstEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit_history.json")
	withLedger(t, path)

	old := time.Date(2026, 9, 1, 12, 0, 0, 0, time.Local)
	RecordCredit(CreditEntry{Time: old, Account: "a", AccountID: "id-a", Model: "auto", Credits: 1, PromptTokens: 10})
	RecordCredit(CreditEntry{Time: old.AddDate(0, 0, 2), Account: "a", AccountID: "id-a", Model: "auto", Credits: 1, PromptTokens: 20})

	s := CreditSummaryRange(CreditRange{To: old.AddDate(0, 0, 5), Bucket: 24 * time.Hour})
	if len(s.Series) != 6 { // 从最早记录日 9-1 到 9-6（含首尾桶）
		t.Fatalf("buckets = %d, want 6: %+v", len(s.Series), s.Series)
	}
	if got := s.Series[0].T; got.Day() != 1 || got.Hour() != 0 {
		t.Errorf("first bucket = %v, want local midnight of first entry day", got)
	}
	if s.Series[0].PromptTokens != 10 || s.Series[2].PromptTokens != 20 {
		t.Errorf("first/third bucket should hold entries: %+v", s.Series[:3])
	}
}

// TestCreditSummaryRangeNonWholeDayBucket 25h 这类非整天宽度应规整为 1 天，
// 且序列长度与索引使用同一口径——否则末端记录会被静默丢弃。
func TestCreditSummaryRangeNonWholeDayBucket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit_history.json")
	withLedger(t, path)

	start := time.Date(2026, 11, 10, 0, 0, 0, 0, time.Local)
	RecordCredit(CreditEntry{Time: start.Add(1 * time.Hour), Account: "a", AccountID: "id-a", Model: "auto", Credits: 1})
	RecordCredit(CreditEntry{Time: start.Add(25 * time.Hour), Account: "a", AccountID: "id-a", Model: "auto", Credits: 1})
	RecordCredit(CreditEntry{Time: start.Add(49 * time.Hour), Account: "a", AccountID: "id-a", Model: "auto", Credits: 1})

	s := CreditSummaryRange(CreditRange{From: start, To: start.Add(72 * time.Hour), Bucket: 25 * time.Hour})
	if s.BucketSeconds != 86400 {
		t.Errorf("bucket = %d, want 86400 (normalized to whole day)", s.BucketSeconds)
	}
	if len(s.Series) != 4 { // 11-10 至 11-13
		t.Fatalf("buckets = %d, want 4: %+v", len(s.Series), s.Series)
	}
	var got int
	for _, p := range s.Series {
		got += p.Requests
	}
	if got != 3 {
		t.Errorf("series requests = %d, want 3 (no entry dropped)", got)
	}
	for i, want := range []int{1, 1, 1, 0} {
		if s.Series[i].Requests != want {
			t.Errorf("bucket %d requests = %d, want %d", i, s.Series[i].Requests, want)
		}
	}
}

// TestCreditSummaryRangeDayBucketDST 天级桶按日历日归并，不能被 DST 的 23/25h
// 日长带偏（America/New_York 2026-03-08 春季调时）。
func TestCreditSummaryRangeDayBucketDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	oldLoc := time.Local
	time.Local = loc
	defer func() { time.Local = oldLoc }()

	path := filepath.Join(t.TempDir(), "credit_history.json")
	withLedger(t, path)

	start := time.Date(2026, 3, 7, 0, 0, 0, 0, loc)
	RecordCredit(CreditEntry{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, loc), Account: "a", AccountID: "id-a", Model: "auto", Credits: 1})
	RecordCredit(CreditEntry{Time: time.Date(2026, 3, 9, 0, 30, 0, 0, loc), Account: "a", AccountID: "id-a", Model: "auto", Credits: 1})

	s := CreditSummaryRange(CreditRange{From: start, To: time.Date(2026, 3, 11, 0, 0, 0, 0, loc), Bucket: 24 * time.Hour})
	if len(s.Series) != 5 { // 3-7、3-8、3-9、3-10、3-11
		t.Fatalf("buckets = %d, want 5: %+v", len(s.Series), s.Series)
	}
	if s.Series[1].Requests != 1 {
		t.Errorf("3-08 entry landed in wrong bucket: %+v", s.Series)
	}
	if s.Series[2].Requests != 1 {
		t.Errorf("3-09 entry landed in wrong bucket: %+v", s.Series)
	}
}

// TestCreditSummaryRangeWidenedBucketWholeDays 极端跨度下自动放大桶宽后，
// 天级桶必须仍是整天，且最新记录不能因索引越界被丢弃。
func TestCreditSummaryRangeWidenedBucketWholeDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit_history.json")
	withLedger(t, path)

	now := time.Now()
	start := now.AddDate(-6, 0, 0)
	RecordCredit(CreditEntry{Time: start, Account: "a", AccountID: "id-a", Model: "auto", Credits: 1})
	RecordCredit(CreditEntry{Time: start.AddDate(3, 0, 0), Account: "a", AccountID: "id-a", Model: "auto", Credits: 1})
	RecordCredit(CreditEntry{Time: now, Account: "a", AccountID: "id-a", Model: "auto", Credits: 1})

	s := CreditSummaryRange(CreditRange{From: start, To: now, Bucket: time.Hour})
	if len(s.Series) > maxCreditSeriesBuckets {
		t.Fatalf("buckets = %d, want <= %d", len(s.Series), maxCreditSeriesBuckets)
	}
	if s.BucketSeconds%86400 != 0 {
		t.Errorf("widened bucket = %ds, want whole days", s.BucketSeconds)
	}
	var got int
	for _, p := range s.Series {
		got += p.Requests
	}
	if got != 3 {
		t.Errorf("series requests = %d, want 3 (latest entries must not be dropped)", got)
	}
}

// TestCreditSummaryRangeBucketCap 极端跨度 + 细桶时自动放大桶宽，桶数有上限。
func TestCreditSummaryRangeBucketCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit_history.json")
	withLedger(t, path)

	now := time.Now()
	RecordCredit(CreditEntry{Time: now.AddDate(-3, 0, 0), Account: "a", AccountID: "id-a", Model: "auto", Credits: 1, PromptTokens: 10})
	RecordCredit(CreditEntry{Time: now, Account: "a", AccountID: "id-a", Model: "auto", Credits: 1, PromptTokens: 20})

	s := CreditSummaryRange(CreditRange{Bucket: time.Second})
	if len(s.Series) > maxCreditSeriesBuckets {
		t.Fatalf("buckets = %d, want <= %d", len(s.Series), maxCreditSeriesBuckets)
	}
	if s.BucketSeconds <= 1 {
		t.Errorf("bucket should widen above 1s, got %d", s.BucketSeconds)
	}
}

// TestCreditSummaryRangeEmpty 没有记录时返回空序列与零总量（不 panic）。
func TestCreditSummaryRangeEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit_history.json")
	withLedger(t, path)

	s := CreditSummaryRange(CreditRange{Bucket: time.Hour})
	if s.Entries != 0 || len(s.Series) != 0 || s.Buckets != 0 {
		t.Errorf("empty summary = %+v", s)
	}
	if s.Since != nil {
		t.Errorf("empty window since = %v, want nil (avoid 0001-01-01 in JSON)", *s.Since)
	}
}

// TestCreditSummaryRangeSince 窗口内最早一条作为 since，窗口无记录时省略。
func TestCreditSummaryRangeSince(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credit_history.json")
	withLedger(t, path)

	old := time.Date(2026, 11, 10, 8, 0, 0, 0, time.Local)
	RecordCredit(CreditEntry{Time: old, Account: "a", AccountID: "id-a", Model: "auto", Credits: 1})
	RecordCredit(CreditEntry{Time: old.Add(2 * time.Hour), Account: "a", AccountID: "id-a", Model: "auto", Credits: 1})

	s := CreditSummaryRange(CreditRange{From: old, To: old.Add(3 * time.Hour)})
	if s.Since == nil || !s.Since.Equal(old) {
		t.Errorf("since = %v, want %v", s.Since, old)
	}
	empty := CreditSummaryRange(CreditRange{From: old.AddDate(0, 0, 2), To: old.AddDate(0, 0, 3)})
	if empty.Since != nil {
		t.Errorf("empty window since = %v, want nil", *empty.Since)
	}
}
