package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"qoder2api/account"
	"qoder2api/logger"
)

// CallRecord 是一次「模型调用」的结构化记录。
//
// 与 logger 的原始行日志不同，这里按字段拆分，供控制台渲染成可读表格：
// 每次客户端请求（chat / claude / codex）最终落到一次或多次上游调用，
// 记录实际使用的账号、映射后的模型、token 用量、耗时与结果。
//
// 保留最近 N 条并持久化到 credit 数据目录的 call_history.json；
// 控制台可清空，清空后从该时刻重新累积。
type CallRecord struct {
	Seq           int       `json:"seq"`
	Time          time.Time `json:"time"`                  // 请求开始时间
	RequestID     string    `json:"request_id"`            // 与原始日志 [Chat][xxxx] 对应的短 id
	SessionKey    string    `json:"session_key,omitempty"` // 会话标识（用于账号粘性与排查）
	ClientIP      string    `json:"client_ip,omitempty"`   // 调用来源 IP（本地/反代已归一）
	Protocol      string    `json:"protocol"`              // chat | claude | codex
	Agent         string    `json:"agent"`                 // claude | codex | gemini
	Model         string    `json:"model"`                 // 客户端请求的模型名
	UpstreamModel string    `json:"upstream_model"`        // 映射后的上游 model.key
	// RemoteModel 上游帧回报的实际模型名（若上游提供）。auto 等路由模型
	// 若更换底层模型，这里会变化，且前缀缓存随之失效。
	RemoteModel     string  `json:"remote_model,omitempty"`
	Account         string  `json:"account"`        // 最终使用的账号名
	AccountID       string  `json:"account_id"`     // 账号 id（排查用）
	Region          string  `json:"region"`         // cn | global
	Stream          bool    `json:"stream"`         // 客户端是否要求流式
	Messages        int     `json:"messages"`       // 消息条数（上游请求体）
	Tools           bool    `json:"tools"`          // 是否携带工具定义
	PromptPreview   string  `json:"prompt_preview"` // 可读的请求预览（已剥离提醒/transcript 包装）
	Attempts        int     `json:"attempts"`       // 实际尝试过的账号数（>1 表示发生换号）
	Status          string  `json:"status"`         // ok | error | cancelled
	ErrorType       string  `json:"error_type,omitempty"`
	ErrorMessage    string  `json:"error_message,omitempty"`
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	CachedTokens    int     `json:"cached_tokens"`    // 上游 prompt 缓存命中 token 数
	ReasoningTokens int     `json:"reasoning_tokens"` // 其中推理 token（completion 明细）
	Credits         float64 `json:"credits"`          // 本次计费 credits
	OriginalCredits float64 `json:"original_credits"` // 优惠前 credits（不同即命中促销）
	DurationMS      int64   `json:"duration_ms"`
	// TTFTMS 首字延迟：从客户端请求开始到第一个内容/推理/工具 delta。
	TTFTMS int64 `json:"ttft_ms"`
	// GenerationMS 生成窗口：首字到最后一个内容 delta。
	GenerationMS int64 `json:"generation_ms,omitempty"`
	// TokensPerSec 吐字速率 = completion_tokens / 生成窗口（窗口过短时为 0）。
	TokensPerSec float64 `json:"tokens_per_sec,omitempty"`
	HadToolCalls bool    `json:"had_tool_calls"` // 上游是否返回了工具调用
	Emitted      bool    `json:"emitted"`        // 是否向客户端发过内容
}

// 调用结果枚举
const (
	CallStatusOK        = "ok"
	CallStatusError     = "error"
	CallStatusCancelled = "cancelled" // 客户端提前断开
)

// CallMeta 由各协议 handler 在入口处注入，CallQoder 记录下来源信息。
type CallMeta struct {
	RequestID string
	Protocol  string // chat | claude | codex
	Stream    bool
	// StartedAt 客户端请求进入 handler 的时间，用于计算首字延迟（TTFT）。
	// 零值时回退到 CallQoder 入口时间。
	StartedAt time.Time
	// SessionKey 客户端会话标识（metadata/conversation id 或消息前缀哈希）。
	// 用于会话级账号粘性：新会话随机选号，同会话后续复用同一账号。
	SessionKey string
	// ClientIP 调用来源 IP（由 handler 从请求中解析）。
	ClientIP string
}

type callMetaKey struct{}

// WithCallMeta 把请求来源信息挂到 context。bridge 包内各 handler 入口调用。
func WithCallMeta(ctx context.Context, m CallMeta) context.Context {
	return context.WithValue(ctx, callMetaKey{}, m)
}

func callMetaFrom(ctx context.Context) CallMeta {
	if ctx == nil {
		return CallMeta{}
	}
	if m, ok := ctx.Value(callMetaKey{}).(CallMeta); ok {
		return m
	}
	return CallMeta{}
}

// callLog 固定容量环形缓冲（最新在前返回），并持久化到磁盘。
type callLog struct {
	mu      sync.Mutex
	cap     int
	nextSeq int
	entries []CallRecord
	path    string
	loaded  bool
}

const callLogCapacity = 2000

var recentCallLog = &callLog{cap: callLogCapacity, nextSeq: 1}

// callDiskFile call_history.json 的落盘结构。
type callDiskFile struct {
	Version int          `json:"version"`
	Saved   string       `json:"saved,omitempty"`
	Entries []CallRecord `json:"entries"`
}

func (c *callLog) filePath() string {
	return filepath.Join(account.DataRoot(), "call_history.json")
}

// ensureLoaded 惰性加载（数据根目录可能在启动后才设置）。调用方必须持有 c.mu。
func (c *callLog) ensureLoaded() {
	path := c.filePath()
	if c.loaded && c.path == path {
		return
	}
	c.loaded = true
	c.path = path
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Error("call log: read %s: %v", path, err)
		}
		return
	}
	var f callDiskFile
	if err := json.Unmarshal(raw, &f); err != nil {
		logger.Error("call log: parse %s: %v", path, err)
		return
	}
	c.entries = f.Entries
	if len(c.entries) > c.cap {
		c.entries = append([]CallRecord(nil), c.entries[len(c.entries)-c.cap:]...)
	}
	c.nextSeq = 1
	for _, e := range c.entries {
		if e.Seq >= c.nextSeq {
			c.nextSeq = e.Seq + 1
		}
	}
	logger.Info("call log: restored %d entries (%s)", len(c.entries), path)
}

// saveLocked 原子写盘。调用方必须持有 c.mu。
func (c *callLog) saveLocked() {
	raw, err := json.Marshal(callDiskFile{
		Version: 1,
		Saved:   time.Now().Format(time.RFC3339),
		Entries: c.entries,
	})
	if err != nil {
		logger.Error("call log: marshal: %v", err)
		return
	}
	if dir := filepath.Dir(c.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			logger.Error("call log: mkdir: %v", err)
			return
		}
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		logger.Error("call log: write tmp: %v", err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		logger.Error("call log: rename: %v", err)
	}
}

func (c *callLog) record(rec CallRecord) {
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLoaded()
	rec.Seq = c.nextSeq
	c.nextSeq++
	if len(c.entries) >= c.cap {
		// 淘汰最旧一条（整体前移，保持容量）
		copy(c.entries, c.entries[1:])
		c.entries[len(c.entries)-1] = rec
	} else {
		c.entries = append(c.entries, rec)
	}
	c.saveLocked()
}

// clear 清空内存与磁盘记录；之后的新记录从 seq=1 重新累积。
func (c *callLog) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLoaded()
	c.entries = []CallRecord{}
	c.nextSeq = 1
	c.saveLocked()
	logger.Info("call log: cleared by user (%s)", c.path)
}

// snapshot 返回最近 limit 条（最新在前）；limit<=0 返回全部。
func (c *callLog) snapshot(limit int) []CallRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLoaded()
	n := len(c.entries)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]CallRecord, limit)
	for i := 0; i < limit; i++ {
		out[i] = c.entries[n-1-i] // 反转：最新在前
	}
	return out
}

// RecentCalls 返回最近模型调用记录（最新在前），供控制台 API 使用。
func RecentCalls(limit int) []CallRecord {
	return recentCallLog.snapshot(limit)
}

// ClearCalls 清空调用记录（内存与磁盘），供控制台「清空」按钮与测试使用。
func ClearCalls() {
	recentCallLog.clear()
}

// truncateRunesForLog 按 rune 截断，避免切断 UTF-8；空白折叠成单空格，
// 让多行 prompt 在表格中保持一行可读。
func truncateRunesForLog(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}

var (
	transcriptUserFieldRE = regexp.MustCompile(`(?s)"user"\s*:\s*"((?:\\.|[^"\\])*)"`)
	transcriptTagRE       = regexp.MustCompile(`(?s)</?transcript>`)
	commandTagRE          = regexp.MustCompile(`(?s)<command-(?:name|message|args)>.*?</command-(?:name|message|args)>`)
	localCommandRE        = regexp.MustCompile(`(?s)<local-command-[a-z]+>.*?</local-command-[a-z]+>`)
)

// cleanPreviewText 把客户端包装成 <transcript> / <system-reminder> / 斜杠命令的
// 文本还原成人类可读的内容。空字符串表示这条消息没有可展示的真实文本。
func cleanPreviewText(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	if m := transcriptUserFieldRE.FindStringSubmatch(s); len(m) > 1 {
		if u, err := strconv.Unquote(`"` + m[1] + `"`); err == nil {
			s = u
		}
	}
	s = systemReminderPattern.ReplaceAllString(s, "")
	s = transcriptTagRE.ReplaceAllString(s, "")
	s = commandTagRE.ReplaceAllString(s, "")
	s = localCommandRE.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

// messageText 取消息的可读文本：user 消息在 Qoder body 里通常是
// content="" + contents=[{type:text,...}]，不能只读 content。
func messageText(m map[string]interface{}) string {
	if s := NormalizeMessageContent(m); s != "" {
		return s
	}
	contents, ok := m["contents"].([]interface{})
	if !ok {
		return ""
	}
	var parts []string
	for _, b := range contents {
		bm, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		if t, ok := bm["text"].(string); ok && strings.TrimSpace(t) != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// callPreview 生成调用日志里的「请求预览」：优先最近的真实用户输入，
// 其次助手文本，最后工具结果首行。避免整列都是 <transcript>/<system-reminder>。
func callPreview(messages []interface{}, max int) string {
	var firstUser, lastUser, lastAssistant, lastTool string
	for _, it := range messages {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		raw := messageText(m)
		switch role {
		case "user":
			if t := cleanPreviewText(raw); t != "" {
				if firstUser == "" {
					firstUser = t
				}
				lastUser = t
			}
		case "assistant":
			if t := cleanPreviewText(raw); t != "" {
				lastAssistant = t
			}
		case "tool":
			// 先取工具结果首行再做清理，避免 cleanPreviewText 把换行折叠掉。
			raw = strings.TrimLeft(raw, "\r\n \t")
			if i := strings.IndexByte(raw, '\n'); i >= 0 {
				raw = raw[:i]
			}
			if t := cleanPreviewText(raw); t != "" {
				lastTool = t
			}
		}
	}
	pick := lastUser
	if pick == "" {
		pick = lastAssistant
	}
	if pick == "" {
		pick = lastTool
	}
	if pick == "" {
		pick = firstUser
	}
	return truncateRunesForLog(pick, max)
}
