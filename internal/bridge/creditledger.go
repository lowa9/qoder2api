package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"qoder2api/account"
	"qoder2api/logger"
)

// 积分扣除账本：把每次成功调用的上游 usage.credits 持久化，
// 按账号 / 按模型聚合，供控制台「用量 → 积分扣除历史」展示。
//
// 与 workbuddy2api-panel 的「积分扣除历史」对齐：两个维度、同样的
// 请求数 / 扣除积分 / 样本 Token / 积分每 1M Token / 缓存命中率。
// 数据来源不同——那边通过余额快照差分推断，这里直接用上游逐次返回的
// usage.credits（更精确，且能拆分到模型）。

// CreditEntry 一条积分扣除记录（一次成功调用）。
type CreditEntry struct {
	Time         time.Time `json:"time"`
	Account      string    `json:"account"`
	AccountID    string    `json:"account_id,omitempty"`
	Region       string    `json:"region,omitempty"`
	Model        string    `json:"model"` // 上游 model.key
	Protocol     string    `json:"protocol,omitempty"`
	Credits      float64   `json:"credits"`
	PriceFactor  float64   `json:"price_factor,omitempty"`
	PromptTokens int       `json:"prompt_tokens"`
	CachedTokens int       `json:"cached_tokens"`
	OutputTokens int       `json:"output_tokens"`
}

const creditLedgerCapacity = 5000

type creditLedger struct {
	mu      sync.Mutex
	path    string // 空则按 account.DataRoot() 推导
	entries []CreditEntry
	loaded  bool
}

var creditBook = &creditLedger{}

func (l *creditLedger) filePath() string {
	if l.path != "" {
		return l.path
	}
	return filepath.Join(account.DataRoot(), "credit_history.json")
}

type creditDiskFile struct {
	Version int           `json:"version"`
	Saved   string        `json:"saved,omitempty"`
	Entries []CreditEntry `json:"entries"`
}

// ensureLoaded 惰性加载（数据根目录可能在启动后才设置）。
// 调用方必须持有 l.mu。
func (l *creditLedger) ensureLoaded() {
	path := l.filePath()
	if l.loaded {
		return
	}
	l.loaded = true
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Error("credit ledger: read %s: %v", path, err)
		}
		return
	}
	var f creditDiskFile
	if err := json.Unmarshal(raw, &f); err != nil {
		logger.Error("credit ledger: parse %s: %v", path, err)
		return
	}
	l.entries = f.Entries
	if len(l.entries) > creditLedgerCapacity {
		l.entries = append([]CreditEntry(nil), l.entries[len(l.entries)-creditLedgerCapacity:]...)
	}
	logger.Info("credit ledger: restored %d entries (%s)", len(l.entries), path)
}

// saveLocked 原子写盘。调用方必须持有 l.mu。
func (l *creditLedger) saveLocked() {
	path := l.filePath()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			logger.Error("credit ledger: mkdir: %v", err)
			return
		}
	}
	raw, err := json.Marshal(creditDiskFile{
		Version: 1,
		Saved:   time.Now().Format(time.RFC3339),
		Entries: l.entries,
	})
	if err != nil {
		logger.Error("credit ledger: marshal: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		logger.Error("credit ledger: write tmp: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logger.Error("credit ledger: rename: %v", err)
	}
}

// record 追加一条扣除记录（credits<=0 的免费调用不入账）。
func (l *creditLedger) record(e CreditEntry) {
	if e.Credits <= 0 {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureLoaded()
	l.entries = append(l.entries, e)
	if len(l.entries) > creditLedgerCapacity {
		// 丢最旧，复制到新数组避免底层数组长期占用
		l.entries = append([]CreditEntry(nil), l.entries[len(l.entries)-creditLedgerCapacity:]...)
	}
	l.saveLocked()
}

// RecordCredit 记录一次积分扣除（供 CallQoderWithOpts 调用）。
func RecordCredit(e CreditEntry) {
	creditBook.record(e)
}

// ClearCreditHistory 清空积分扣除历史（内存与磁盘），之后从零重新累积。
func ClearCreditHistory() {
	l := creditBook
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureLoaded()
	l.entries = []CreditEntry{}
	l.saveLocked()
	logger.Info("credit ledger: cleared by user (%s)", l.filePath())
}

// CreditAgg 账号或模型维度的聚合。
type CreditAgg struct {
	Key             string  `json:"key"`
	Nickname        string  `json:"nickname,omitempty"`
	Realm           string  `json:"realm,omitempty"`
	Region          string  `json:"region,omitempty"`
	PriceFactor     float64 `json:"price_factor,omitempty"`
	Requests        int     `json:"requests"`
	Credits         float64 `json:"credits"`
	PromptTokens    int     `json:"prompt_tokens"`
	CachedTokens    int     `json:"cached_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	CreditsPer1MTok float64 `json:"credits_per_1m_tokens"`
	Samples         int     `json:"credit_samples"`
	CacheHitTokens  int     `json:"cache_hit_tokens"`
	CacheMissTokens int     `json:"cache_miss_tokens"`
}

// CreditTotals 汇总指标。
type CreditTotals struct {
	Credits         float64 `json:"credits"`
	Requests        int     `json:"requests"`
	PromptTokens    int     `json:"prompt_tokens"`
	CachedTokens    int     `json:"cached_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	Samples         int     `json:"credit_samples"`
	CreditsPer1MTok float64 `json:"credits_per_1m_tokens"`
	CacheHitTokens  int     `json:"cache_hit_tokens"`
	CacheMissTokens int     `json:"cache_miss_tokens"`
}

// CreditSummaryView 返回给控制台的结构。
type CreditSummaryView struct {
	Totals     CreditTotals  `json:"totals"`
	ByAccount  []CreditAgg   `json:"by_account"`
	ByModel    []CreditAgg   `json:"by_model"`
	Entries    int           `json:"entries"`         // 时间窗口内的入账条数
	AllEntries int           `json:"all_entries"`     // 账本总条数（清空后从 0 重新累积）
	Since      *time.Time    `json:"since,omitempty"` // 窗口内最早一条；空窗口时为 nil
	FileBytes  int64         `json:"file_bytes,omitempty"`
	Recent     []CreditEntry `json:"recent,omitempty"`
	// 时间序列（仅当 CreditRange.Bucket>0 且窗口内有记录时返回）。
	From          string              `json:"from,omitempty"` // 序列首桶起点（RFC3339）
	To            string              `json:"to,omitempty"`   // 窗口终点（RFC3339）
	BucketSeconds int                 `json:"bucket_seconds,omitempty"`
	Buckets       int                 `json:"buckets,omitempty"`
	Series        []CreditSeriesPoint `json:"series,omitempty"`
}

// CreditSeriesPoint 一个时间桶的汇总，供控制台柱状图渲染。
type CreditSeriesPoint struct {
	T            time.Time `json:"t"` // 桶起点（按本地时间对齐）
	Requests     int       `json:"requests"`
	Credits      float64   `json:"credits"`
	PromptTokens int       `json:"prompt_tokens"`
	CachedTokens int       `json:"cached_tokens"` // prompt 中的缓存命中部分
	OutputTokens int       `json:"output_tokens"`
}

// CreditRange 描述积分汇总的时间窗口与分桶。
// From 零值 = 从最早记录开始；To 零值 = 现在；Bucket 0 = 不生成时间序列。
type CreditRange struct {
	From   time.Time
	To     time.Time
	Bucket time.Duration
}

// maxCreditSeriesBuckets 限制单次返回的桶数（极端跨度 + 细桶时自动放大桶宽）。
const maxCreditSeriesBuckets = 2000

func minCreditBucket() time.Duration { return time.Second }
func maxCreditBucket() time.Duration { return 31 * 24 * time.Hour }

// normalizeCreditBucket 把天级桶规整为整天。天级桶的长度、序列长度与索引
// 必须使用同一日历口径，否则非整天宽度（如 25h）会让序列末端丢记录。
func normalizeCreditBucket(bucket time.Duration) time.Duration {
	if bucket < 24*time.Hour {
		return bucket
	}
	days := bucket / (24 * time.Hour)
	if days < 1 {
		days = 1
	}
	return days * 24 * time.Hour
}

// widenCreditBucket 按整数倍放大桶宽；跨过天级后向上取整到整天，
// 保证放大后的桶仍是合法日历日宽。
func widenCreditBucket(bucket time.Duration, mult int) time.Duration {
	widened := bucket * time.Duration(mult)
	if widened < 24*time.Hour {
		return widened
	}
	days := widened / (24 * time.Hour)
	if widened%(24*time.Hour) != 0 {
		days++
	}
	if days < 1 {
		days = 1
	}
	return days * 24 * time.Hour
}

// civilDayNumber 返回本地日历日的序数（按年月日计算，不受 DST 影响）。
func civilDayNumber(t time.Time) int {
	lt := t.Local()
	y, m, d := lt.Date()
	return int(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

// floorCreditBucket 把时间向下取整到桶起点。
// 小时级桶锚定本地零点（而非 UTC），天级桶直接取本地日历日零点。
func floorCreditBucket(t time.Time, bucket time.Duration) time.Time {
	lt := t.Local()
	midnight := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, lt.Location())
	if bucket >= 24*time.Hour {
		return midnight
	}
	return midnight.Add(lt.Sub(midnight).Truncate(bucket))
}

// creditBucketIndex 计算 t 落在第几个桶。
// 天级桶按本地日历日差归并，避免 DST/时区偏移导致桶错位。
func creditBucketIndex(start time.Time, bucket time.Duration, t time.Time) int {
	if bucket < 24*time.Hour {
		return int(t.Sub(start) / bucket)
	}
	days := int(bucket / (24 * time.Hour))
	if days < 1 {
		days = 1
	}
	d := civilDayNumber(t) - civilDayNumber(start)
	if d < 0 {
		return -1
	}
	return d / days
}

// creditBucketStart 返回第 i 个桶的起点。
func creditBucketStart(start time.Time, bucket time.Duration, i int) time.Time {
	if bucket >= 24*time.Hour {
		days := int(bucket / (24 * time.Hour))
		if days < 1 {
			days = 1
		}
		return start.AddDate(0, 0, i*days)
	}
	return start.Add(time.Duration(i) * bucket)
}

// creditBucketCount 返回序列桶数，与 creditBucketIndex 使用同一口径。
func creditBucketCount(start, to time.Time, bucket time.Duration) int {
	if bucket < 24*time.Hour {
		return int(to.Sub(start)/bucket) + 1
	}
	days := int(bucket / (24 * time.Hour))
	if days < 1 {
		days = 1
	}
	n := (civilDayNumber(to)-civilDayNumber(start))/days + 1
	if n < 1 {
		n = 1
	}
	return n
}

// buildCreditSeries 把窗口内记录装进等宽时间桶（保留空桶），返回序列与实际桶宽
// （极端跨度下会按整数倍放大桶宽以限制桶数）。
func buildCreditSeries(entries []CreditEntry, start time.Time, to time.Time, bucket time.Duration) ([]CreditSeriesPoint, time.Duration) {
	if to.Before(start) {
		to = start
	}
	n := creditBucketCount(start, to, bucket)
	if n > maxCreditSeriesBuckets {
		mult := (n + maxCreditSeriesBuckets - 1) / maxCreditSeriesBuckets
		bucket = widenCreditBucket(bucket, mult)
		n = creditBucketCount(start, to, bucket)
		for n > maxCreditSeriesBuckets && bucket < maxCreditBucket() {
			bucket = widenCreditBucket(bucket, 2)
			n = creditBucketCount(start, to, bucket)
		}
	}
	if n <= 0 {
		return nil, bucket
	}
	series := make([]CreditSeriesPoint, n)
	for i := range series {
		series[i].T = creditBucketStart(start, bucket, i)
	}
	for _, e := range entries {
		i := creditBucketIndex(start, bucket, e.Time)
		if i < 0 || i >= n {
			continue
		}
		p := &series[i]
		p.Requests++
		p.Credits += e.Credits
		p.PromptTokens += e.PromptTokens
		p.CachedTokens += e.CachedTokens
		p.OutputTokens += e.OutputTokens
	}
	return series, bucket
}

// summarize 把条目累积进聚合器（调用方负责加锁与快照）。
func accumulate(m map[string]*CreditAgg, key string, e CreditEntry) {
	a := m[key]
	if a == nil {
		a = &CreditAgg{Key: key}
		m[key] = a
	}
	a.Requests++
	a.Credits += e.Credits
	a.PromptTokens += e.PromptTokens
	a.CachedTokens += e.CachedTokens
	a.OutputTokens += e.OutputTokens
	if e.PriceFactor > 0 {
		a.PriceFactor = e.PriceFactor
	}
}

func finalize(a *CreditAgg) {
	a.Samples = a.Requests
	a.CacheHitTokens = a.CachedTokens
	a.CacheMissTokens = a.PromptTokens - a.CachedTokens
	if a.CacheMissTokens < 0 {
		a.CacheMissTokens = 0
	}
	if a.PromptTokens > 0 {
		a.CreditsPer1MTok = a.Credits / float64(a.PromptTokens) * 1_000_000
	}
}

// CreditSummary 返回全量聚合视图（兼容旧调用）。
func CreditSummary() CreditSummaryView {
	return CreditSummaryRange(CreditRange{})
}

// CreditSummaryRange 返回指定时间窗口的聚合视图与可选的时间序列。
//
// 窗口外的记录不参与 totals / by_account / by_model；Bucket>0 时，
// 序列从窗口起点（From 零值则从最早记录所在桶）开始，保留无请求的空桶。
func CreditSummaryRange(r CreditRange) CreditSummaryView {
	creditBook.mu.Lock()
	defer creditBook.mu.Unlock()
	creditBook.ensureLoaded()

	to := r.To
	if to.IsZero() {
		to = time.Now()
	}
	from := r.From
	if !from.IsZero() && to.Before(from) {
		from, to = to, from
	}

	// 账本按写入顺序追加，可能包含零散时间戳，不能依赖排序做二分。
	entries := make([]CreditEntry, 0, len(creditBook.entries))
	for _, e := range creditBook.entries {
		if !from.IsZero() && e.Time.Before(from) {
			continue
		}
		if e.Time.After(to) {
			continue
		}
		entries = append(entries, e)
	}

	byAcct := map[string]*CreditAgg{}
	byModel := map[string]*CreditAgg{}
	view := CreditSummaryView{
		Entries:    len(entries),
		AllEntries: len(creditBook.entries),
	}
	var since time.Time
	for _, e := range entries {
		accumulate(byAcct, e.AccountID+"|"+e.Account, e)
		accumulate(byModel, e.Model, e)
		view.Totals.Credits += e.Credits
		view.Totals.Requests++
		view.Totals.PromptTokens += e.PromptTokens
		view.Totals.CachedTokens += e.CachedTokens
		view.Totals.OutputTokens += e.OutputTokens
		if since.IsZero() || e.Time.Before(since) {
			since = e.Time
		}
	}
	if !since.IsZero() {
		view.Since = &since
	}
	// 账号维度补充展示字段
	for _, e := range entries {
		if a := byAcct[e.AccountID+"|"+e.Account]; a != nil {
			a.Nickname = e.Account
			a.Region = string(e.Region)
		}
	}
	for _, a := range byAcct {
		finalize(a)
		view.ByAccount = append(view.ByAccount, *a)
	}
	for _, a := range byModel {
		finalize(a)
		view.ByModel = append(view.ByModel, *a)
	}
	sort.Slice(view.ByAccount, func(i, j int) bool { return view.ByAccount[i].Credits > view.ByAccount[j].Credits })
	sort.Slice(view.ByModel, func(i, j int) bool { return view.ByModel[i].Credits > view.ByModel[j].Credits })

	view.Totals.Samples = view.Totals.Requests
	view.Totals.CacheHitTokens = view.Totals.CachedTokens
	view.Totals.CacheMissTokens = view.Totals.PromptTokens - view.Totals.CachedTokens
	if view.Totals.CacheMissTokens < 0 {
		view.Totals.CacheMissTokens = 0
	}
	if view.Totals.PromptTokens > 0 {
		view.Totals.CreditsPer1MTok = view.Totals.Credits / float64(view.Totals.PromptTokens) * 1_000_000
	}

	// 最近 50 条（新的在前，限定在窗口内）
	for i := len(entries) - 1; i >= 0 && len(view.Recent) < 50; i-- {
		view.Recent = append(view.Recent, entries[i])
	}
	if st, err := os.Stat(creditBook.filePath()); err == nil {
		view.FileBytes = st.Size()
	}

	// 时间序列：从窗口起点（或最早记录）开始，保留空桶。
	if r.Bucket > 0 && len(entries) > 0 {
		bucket := r.Bucket
		if bucket < minCreditBucket() {
			bucket = minCreditBucket()
		}
		if bucket > maxCreditBucket() {
			bucket = maxCreditBucket()
		}
		bucket = normalizeCreditBucket(bucket)
		start := from
		if start.IsZero() {
			start = entries[0].Time
			for _, e := range entries[1:] {
				if e.Time.Before(start) {
					start = e.Time
				}
			}
		}
		start = floorCreditBucket(start, bucket)
		series, effective := buildCreditSeries(entries, start, to, bucket)
		if len(series) > 0 {
			view.Series = series
			view.Buckets = len(series)
			view.BucketSeconds = int(effective / time.Second)
			view.From = series[0].T.Format(time.RFC3339)
			view.To = to.Format(time.RFC3339)
		}
	}
	return view
}
