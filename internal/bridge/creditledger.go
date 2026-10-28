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
	Totals    CreditTotals  `json:"totals"`
	ByAccount []CreditAgg   `json:"by_account"`
	ByModel   []CreditAgg   `json:"by_model"`
	Entries   int           `json:"entries"`
	Since     time.Time     `json:"since,omitempty"`
	FileBytes int64         `json:"file_bytes,omitempty"`
	Recent    []CreditEntry `json:"recent,omitempty"`
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

// CreditSummary 返回聚合视图（新的在前，最多附带 50 条最近记录）。
func CreditSummary() CreditSummaryView {
	creditBook.mu.Lock()
	defer creditBook.mu.Unlock()
	creditBook.ensureLoaded()

	byAcct := map[string]*CreditAgg{}
	byModel := map[string]*CreditAgg{}
	view := CreditSummaryView{Entries: len(creditBook.entries)}
	for _, e := range creditBook.entries {
		accumulate(byAcct, e.AccountID+"|"+e.Account, e)
		accumulate(byModel, e.Model, e)
		view.Totals.Credits += e.Credits
		view.Totals.Requests++
		view.Totals.PromptTokens += e.PromptTokens
		view.Totals.CachedTokens += e.CachedTokens
		view.Totals.OutputTokens += e.OutputTokens
		if view.Since.IsZero() || e.Time.Before(view.Since) {
			view.Since = e.Time
		}
	}
	// 账号维度补充展示字段
	for _, e := range creditBook.entries {
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

	// 最近 50 条（新的在前）
	n := len(creditBook.entries)
	for i := n - 1; i >= 0 && len(view.Recent) < 50; i-- {
		view.Recent = append(view.Recent, creditBook.entries[i])
	}
	if st, err := os.Stat(creditBook.filePath()); err == nil {
		view.FileBytes = st.Size()
	}
	return view
}
