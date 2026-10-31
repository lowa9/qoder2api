package bridge

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"qoder2api/account"
	"qoder2api/internal/cosy"
	"qoder2api/logger"
)

// 最小档账号池：选号 + 冷却 + 失败换号。
// 不做加权 / 熔断 / 降权 / 成本探索（这些留待后续按需增加）。

// ErrNoAccount 池中没有任何可用账号。
var ErrNoAccount = errors.New("no available account in pool")

// stickyEnabled 账号池粘性：开启时优先复用上次成功账号。
//
// 动机：上游 prompt 缓存（cached_tokens）在实测中按账号冷启动——每个账号
// 首次调用同一前缀都是全价，之后才命中（命中时代价约为未命中的 1/12~1/25）。
// 严格轮询会让每个账号各自冷启动一次；粘性选号让同一账号持续服务，
// 低频场景下可显著减少全价调用。失败/冷却时仍走正常换号逻辑。
var stickyEnabled atomic.Bool

// poolRandIndex 冷启动时的随机起点；测试可替换。
var poolRandIndex = rand.IntN

func init() { stickyEnabled.Store(true) }

// SetStickyPool 热更新粘性开关（控制台「账号池策略」）。
func SetStickyPool(enabled bool) {
	stickyEnabled.Store(enabled)
	logger.Info("pool: sticky routing set to %v", enabled)
}

const (
	// CooldownQuota 额度 / 权限类失败（上游 401/403，如 code 112 需要升级套餐）：
	// 账号在当前计费周期内基本不可用，给长冷却。
	CooldownQuota = 30 * time.Minute
	// CooldownRate 上游频控（429）。
	CooldownRate = 60 * time.Second
	// CooldownTransient 瞬时上游故障（418/5xx/传输抖动）同账号重试耗尽后：
	// 短冷却，避免立刻再撞同一个账号。
	CooldownTransient = 15 * time.Second
)

// Slot 是一个可用账号的运行时状态（session + 冷却）。
type Slot struct {
	ID     string
	Name   string
	Region account.Region

	sess   *cosy.SessionContext
	client *BearerClient

	// secret 原始凭证（PAT / OAuth JSON），用于会话过期时重新交换。
	// fp 为该账号的现代协议指纹（可为 nil），刷新时保持一致。
	secret string
	fp     *account.CheckinFingerprint

	mu            sync.Mutex
	cooldownUntil time.Time
	lastErr       string

	// 会话续期：refreshMu 保护 lastRefreshAttempt，避免并发重复交换。
	refreshMu          sync.Mutex
	lastRefreshAttempt time.Time

	// 该账号所在区域的可用模型目录缓存（用于按模型路由到正确区域）。
	catalogMu sync.Mutex
	catalog   map[string]bool
	catalogAt time.Time
}

// snapshot 返回当前会话与客户端的快照。会话可能被续期替换，读取必须走此方法。
func (s *Slot) snapshot() (*cosy.SessionContext, *BearerClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess, s.client
}

// replaceSession 原子替换会话与客户端（续期后调用）。
func (s *Slot) replaceSession(sess *cosy.SessionContext, client *BearerClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sess = sess
	s.client = client
}

// NewSlot 组装一个池内账号。
func NewSlot(id, name string, region account.Region, sess *cosy.SessionContext, client *BearerClient) *Slot {
	return &Slot{ID: id, Name: name, Region: region, sess: sess, client: client}
}

func (s *Slot) available(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !now.Before(s.cooldownUntil)
}

func (s *Slot) cool(d time.Duration, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	until := time.Now().Add(d)
	if until.After(s.cooldownUntil) {
		s.cooldownUntil = until
	}
	s.lastErr = reason
}

func (s *Slot) noteSuccess() {
	s.mu.Lock()
	s.lastErr = ""
	s.mu.Unlock()
}

// Pool 是账号池：轮询选号，跳过冷却中与已试过的账号。
type Pool struct {
	mu          sync.Mutex
	slots       []*Slot
	cursor      int
	lastSuccess string // 最近一次成功调用使用的账号 id（全局粘性，无会话标识时使用）
	// sessions 会话 -> 账号绑定。新会话随机选号，同一会话后续复用，
	// 使多个客户端会话各自落在不同账号上并保持缓存热度。
	sessions  map[string]sessionBinding
	statePath string // 非空时把冷却状态持久化到该文件（重启不丢）
}

// sessionBinding 一个会话当前绑定的账号。
type sessionBinding struct {
	accountID string
	seen      time.Time
}

const (
	// sessionBindingTTL 会话绑定的闲置有效期；超过后视为新会话重新随机。
	sessionBindingTTL = 2 * time.Hour
	// maxSessionBindings 绑定表上限，超出时淘汰最久未使用的条目。
	maxSessionBindings = 1024
)

func NewPool(slots ...*Slot) *Pool { return &Pool{slots: slots} }

// NewPoolWithState 创建带持久化的账号池：先从 statePath 载入冷却状态，
// 之后每次冷却 / 恢复都会写回，使重启不丢失冷却（避免重启后立刻再撞
// 已知失效的账号）。
func NewPoolWithState(statePath string, slots ...*Slot) *Pool {
	p := &Pool{slots: slots, statePath: statePath}
	p.load()
	return p
}

// poolState 是落盘的池状态（仅保存需要跨重启保留的字段）。
type poolState struct {
	Slots map[string]slotState `json:"slots"`
}

type slotState struct {
	CooldownUntil time.Time `json:"cooldown_until"`
	LastError     string    `json:"last_error,omitempty"`
}

// LoadState 从磁盘恢复冷却状态。必须在账号全部 Add 之后调用——
// 构造时空池 load 匹配不到任何 ID（这正是先前重启丢冷却的原因）。
func (p *Pool) LoadState() { p.load() }

// load 从磁盘恢复冷却状态（仅恢复尚未过期的冷却）。
func (p *Pool) load() {
	if p == nil || p.statePath == "" {
		return
	}
	data, err := os.ReadFile(p.statePath)
	if err != nil {
		return
	}
	var st poolState
	if json.Unmarshal(data, &st) != nil {
		return
	}
	now := time.Now()
	restored := 0
	for _, s := range p.snapshot() {
		e, ok := st.Slots[s.ID]
		if !ok {
			continue
		}
		s.mu.Lock()
		if e.CooldownUntil.After(now) {
			s.cooldownUntil = e.CooldownUntil
			restored++
		}
		s.lastErr = e.LastError
		s.mu.Unlock()
	}
	if restored > 0 {
		logger.Info("pool: restored %d cooling account(s) from %s", restored, p.statePath)
	}
}

// save 原子写回池状态（临时文件 + rename）。
func (p *Pool) save() {
	if p == nil || p.statePath == "" {
		return
	}
	st := poolState{Slots: map[string]slotState{}}
	for _, s := range p.snapshot() {
		s.mu.Lock()
		until := s.cooldownUntil
		lastErr := s.lastErr
		s.mu.Unlock()
		if !until.IsZero() || lastErr != "" {
			st.Slots[s.ID] = slotState{CooldownUntil: until, LastError: lastErr}
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := p.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		logger.Error("pool: save state: %v", err)
		return
	}
	if err := os.Rename(tmp, p.statePath); err != nil {
		logger.Error("pool: rename state: %v", err)
	}
}

func (p *Pool) Add(s *Slot) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.slots = append(p.slots, s)
}

func (p *Pool) Size() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.slots)
}

// snapshot 返回账号切片的副本（供 Bridge 遍历，不持有锁）。
func (p *Pool) snapshot() []*Slot {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Slot, len(p.slots))
	copy(out, p.slots)
	return out
}

// Pick 从轮询游标开始扫描，返回第一个「未冷却、不在 exclude 中、且满足
// 可选 filter」的账号。filter 在锁外调用（允许其中做缓存/网络操作）。
// Pick 无会话标识的选号（模型目录、控制台等场景沿用全局粘性）。
func (p *Pool) Pick(exclude map[string]struct{}, filters ...func(*Slot) bool) (*Slot, bool) {
	return p.pick("", exclude, filters...)
}

// PickWithSession 会话感知选号：
//   - 该会话已有绑定且账号可用 → 复用（缓存热）
//   - 新会话/绑定失效 → 随机起点（多个会话分散到不同账号）
//   - 会话为空时退化为原有全局粘性 + 冷启动随机
func (p *Pool) PickWithSession(sessionKey string, exclude map[string]struct{}, filters ...func(*Slot) bool) (*Slot, bool) {
	return p.pick(sessionKey, exclude, filters...)
}

func (p *Pool) pick(sessionKey string, exclude map[string]struct{}, filters ...func(*Slot) bool) (*Slot, bool) {
	if p == nil {
		return nil, false
	}
	var filter func(*Slot) bool
	if len(filters) > 0 {
		filter = filters[0]
	}
	slots := p.snapshot()
	n := len(slots)
	if n == 0 {
		return nil, false
	}
	now := time.Now()

	p.mu.Lock()
	start := p.cursor
	stickyID := p.lastSuccess
	sessionAcc := ""
	if sessionKey != "" && stickyEnabled.Load() {
		if b, ok := p.sessions[sessionKey]; ok && now.Sub(b.seen) < sessionBindingTTL {
			sessionAcc = b.accountID
		}
	}
	p.mu.Unlock()
	// 关闭粘性时退化为纯轮询：忽略会话绑定与随机起点
	if !stickyEnabled.Load() {
		sessionKey = ""
		sessionAcc = ""
	}

	usable := func(id string) *Slot {
		if id == "" {
			return nil
		}
		if _, skip := exclude[id]; skip {
			return nil
		}
		for _, s := range slots {
			if s.ID != id || !s.available(now) {
				continue
			}
			if filter != nil && !filter(s) {
				return nil
			}
			return s
		}
		return nil
	}

	// 1) 会话绑定优先
	if s := usable(sessionAcc); s != nil {
		return s, true
	}
	// 2) 无会话标识时才使用全局粘性（新会话应随机分散）
	if sessionKey == "" && stickyEnabled.Load() {
		if s := usable(stickyID); s != nil {
			return s, true
		}
	}
	// 3) 新会话 / 粘性冷启动：随机起点（关闭粘性时保持纯轮询）
	if poolRandIndex != nil {
		if sessionKey != "" && sessionAcc == "" {
			start = poolRandIndex(n)
		} else if sessionKey == "" && stickyID == "" && stickyEnabled.Load() {
			start = poolRandIndex(n)
		}
	}

	for i := 0; i < n; i++ {
		idx := (start + i) % n
		s := slots[idx]
		if _, skip := exclude[s.ID]; skip {
			continue
		}
		if !s.available(now) {
			continue
		}
		if filter != nil && !filter(s) {
			continue
		}
		p.mu.Lock()
		p.cursor = (idx + 1) % n
		p.mu.Unlock()
		return s, true
	}
	return nil, false
}

// Cool 给指定账号设置冷却（取较晚的到期时间）。
func (p *Pool) Cool(id string, d time.Duration, reason string) {
	for _, s := range p.snapshot() {
		if s.ID == id {
			s.cool(d, reason)
			logger.Info("pool: account %s cooled for %s (%s)", id, d, reason)
			p.save()
			return
		}
	}
}

// NoteSessionSuccess 记录会话与账号的绑定，后续同会话优先复用该账号。
func (p *Pool) NoteSessionSuccess(sessionKey, id string) {
	if p == nil || sessionKey == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions == nil {
		p.sessions = map[string]sessionBinding{}
	}
	now := time.Now()
	p.sessions[sessionKey] = sessionBinding{accountID: id, seen: now}
	if len(p.sessions) > maxSessionBindings {
		p.pruneSessionsLocked(now)
	}
}

// pruneSessionsLocked 清理过期绑定；仍超限时反复淘汰最久未使用项。
// 调用方必须持有 p.mu。
func (p *Pool) pruneSessionsLocked(now time.Time) {
	for k, b := range p.sessions {
		if now.Sub(b.seen) >= sessionBindingTTL {
			delete(p.sessions, k)
		}
	}
	for len(p.sessions) > maxSessionBindings {
		oldestKey := ""
		var oldest time.Time
		for k, b := range p.sessions {
			if oldestKey == "" || b.seen.Before(oldest) {
				oldestKey, oldest = k, b.seen
			}
		}
		delete(p.sessions, oldestKey)
	}
}

// NoteSuccess 清除账号的失败标记。
func (p *Pool) NoteSuccess(id string) {
	for _, s := range p.snapshot() {
		if s.ID == id {
			s.noteSuccess()
			p.mu.Lock()
			p.lastSuccess = id
			p.mu.Unlock()
			p.save()
			return
		}
	}
}

// ShouldFailover 判断某次上游失败是否值得换号。
// 只有「换号可能成功」的错误才换：额度/权限(401/403)、频控(429)、
// 瞬时上游故障(418/5xx/传输层)。客户端参数错、内容审核换号无意义，直接上抛。
func ShouldFailover(err error) bool {
	if err == nil {
		return false
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		if ue.ErrType == ErrTypeContentPolicy {
			return false
		}
		// 队列拥塞：换号无益（同模型同队列），且已在 streamWithSlot 内同账号
		// 等待重试；直接上抛友好错误，不冷却、不换号。
		if ue.ErrType == ErrTypeUpstreamQueue {
			return false
		}
		switch ue.Status {
		case http.StatusUnauthorized, http.StatusForbidden: // 额度 / 凭证
			return true
		case http.StatusTooManyRequests: // 频控
			return true
		}
		if ue.Status == 418 || ue.Status >= 500 {
			return true
		}
		if ue.Status >= 400 && ue.Status < 500 {
			return false // 其余 4xx：客户端参数错
		}
		// 流内业务错误（Status=0）：额度/频控类才换号
		return isAccountExhaustedDetail(ue.Detail)
	}
	// 裸传输错误（TLS EOF / 超时等）
	return IsTransientTransport(err)
}

// isLoginExpired 判断上游错误是否为「会话过期」（code 105）。
// 这类错误可通过重新交换凭证自愈，不应直接冷却 30 分钟。
func isLoginExpired(err error) bool {
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	if ue.Status != http.StatusUnauthorized && ue.Status != http.StatusForbidden {
		return false
	}
	d := strings.ToLower(ue.Detail)
	return strings.Contains(d, `"code":"105"`) || strings.Contains(d, "login expired")
}

// isAccountExhaustedDetail 识别流内业务错误里「该账号已耗尽」的语义。
func isAccountExhaustedDetail(detail string) bool {
	d := strings.ToLower(detail)
	for _, k := range []string{"quota", "credit", "exceed", "10605", "rate limit", "too many request"} {
		if strings.Contains(d, k) {
			return true
		}
	}
	return false
}

// CooldownFor 给出错误对应的冷却时长。
func CooldownFor(err error) time.Duration {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		// 上游给了明确重试时间（如 10605 排队 retryAfterSeconds）时优先用它，
		// 上限 5 分钟，避免异常大值把账号长期雪藏。
		if d, ok := retryAfterFromDetail(ue.Detail); ok {
			return d
		}
		switch {
		case ue.Status == http.StatusUnauthorized || ue.Status == http.StatusForbidden:
			return CooldownQuota
		case ue.Status == http.StatusTooManyRequests:
			return CooldownRate
		default:
			return CooldownTransient
		}
	}
	return CooldownTransient
}

// retryAfterFromDetail 从上游详情解析 retryAfterSeconds（10605 排队等场景）。
func retryAfterFromDetail(detail string) (time.Duration, bool) {
	idx := strings.Index(detail, "retryAfterSeconds")
	if idx < 0 {
		return 0, false
	}
	rest := detail[idx+len("retryAfterSeconds"):]
	start := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] >= '0' && rest[i] <= '9' {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, false
	}
	end := start
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(rest[start:end])
	if err != nil || n <= 0 {
		return 0, false
	}
	d := time.Duration(n) * time.Second
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d, true
}

// SlotStatus 是给控制台/运维看的账号池运行时状态。
type SlotStatus struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Region             string `json:"region"`
	CoolingRemainingMS int64  `json:"cooling_remaining_ms"`
	LastError          string `json:"last_error,omitempty"`
}

// Status 返回该账号的运行时状态。
func (s *Slot) Status() SlotStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rem int64
	if now := time.Now(); now.Before(s.cooldownUntil) {
		rem = s.cooldownUntil.Sub(now).Milliseconds()
	}
	return SlotStatus{ID: s.ID, Name: s.Name, Region: string(s.Region), CoolingRemainingMS: rem, LastError: s.lastErr}
}

// Status 返回池内所有账号的运行时状态。
func (p *Pool) Status() []SlotStatus {
	if p == nil {
		return nil
	}
	out := []SlotStatus{}
	for _, s := range p.snapshot() {
		out = append(out, s.Status())
	}
	return out
}
