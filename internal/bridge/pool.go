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
	"time"

	"qoder2api/account"
	"qoder2api/internal/cosy"
	"qoder2api/logger"
)

// 最小档账号池：选号 + 冷却 + 失败换号。
// 不做加权 / 熔断 / 降权 / 成本探索（这些留待后续按需增加）。

// ErrNoAccount 池中没有任何可用账号。
var ErrNoAccount = errors.New("no available account in pool")

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

	// 额度缓存：用于自动选号与"额度耗尽"兜底。
	// quotaRemaining 仅含账号自身额度（user+addon+专属包）；
	// orgRemaining/orgAvailable 是组织共享包（所有账号共用，仅用于判断"还有没有额度"）。
	quotaMu        sync.Mutex
	quotaRemaining float64
	orgRemaining   float64
	orgAvailable   bool
	quotaAt        time.Time
}

// SetQuota 更新账号剩余额度缓存（由 Service 定期刷新）。
// 返回更新前后的可用额度（含真实组织包），供服务在额度上升时解除陈旧冷却。
func (s *Slot) SetQuota(remaining, orgRemaining float64, orgAvailable bool) (before, after float64) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	before = s.rankQuotaLocked()
	s.quotaRemaining = remaining
	s.orgRemaining = orgRemaining
	s.orgAvailable = orgAvailable
	s.quotaAt = time.Now()
	after = s.rankQuotaLocked()
	return before, after
}

func (s *Slot) rankQuotaLocked() float64 {
	q := s.quotaRemaining
	if s.orgAvailable && s.orgRemaining > 0 {
		q += s.orgRemaining
	}
	return q
}

// quota 返回账号自身剩余额度与采集时间。
func (s *Slot) quota() (float64, time.Time) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	return s.quotaRemaining, s.quotaAt
}

// rankQuota 用于「额度最高」排序：真实可用的组织包也计入。
// cap<0 的占位包在解析层已标记为 unavailable，不会参与排序。
func (s *Slot) rankQuota() (float64, time.Time) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	return s.rankQuotaLocked(), s.quotaAt
}

// hasQuota 判断账号是否还有可用额度。额度未知或过旧时视为可用，
// 避免因刷新失败把正常账号误判为耗尽。
func (s *Slot) hasQuota(now time.Time) bool {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	if s.quotaAt.IsZero() || now.Sub(s.quotaAt) > quotaFreshFor {
		return true
	}
	return s.quotaRemaining > 0 || (s.orgAvailable && s.orgRemaining > 0)
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

// uncool 立即解除冷却（额度重置/充值后由刷新器或手动操作调用）。
func (s *Slot) uncool() {
	s.mu.Lock()
	s.cooldownUntil = time.Time{}
	s.mu.Unlock()
}

// Pool 是账号池：轮询选号，跳过冷却中与已试过的账号。
type Pool struct {
	mu          sync.Mutex
	slots       []*Slot
	cursor      int
	lastSuccess string // 最近一次成功调用使用的账号 id（仅用于状态展示）
	// pinned 当前路由账号（固定选号）。
	// 只有三种情况会改选：账号冷却、额度耗尽/不可用，或用户手动切换。
	// 不再有会话/前缀组绑定：Qwen/Kimi 的上游缓存跨账号共享，换号不丢缓存；
	// DeepSeek 同号缓存由「固定当前账号」本身保证。
	pinned       string
	pinnedManual bool
	statePath    string // 非空时把冷却状态持久化到该文件（重启不丢）
}

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

// Pick 选号（固定当前账号模式）。
//
// 规则只有三条：当前账号可用且还有额度 → 直接用；否则（冷却/额度耗尽/
// 不存在）立即改选可用额度最高的账号；再不行才轮询扫描。
// 不再有会话/前缀组绑定：Qwen/Kimi 的上游缓存跨账号共享（实测换号后
// 仍 99.9–100% 命中），DeepSeek 靠「固定当前账号」本身保住同号缓存。
func (p *Pool) Pick(exclude map[string]struct{}, filters ...func(*Slot) bool) (*Slot, bool) {
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
	currentID := p.pinned
	p.mu.Unlock()

	// 1) 当前账号：可用 + 有额度
	for _, sl := range slots {
		if sl.ID != currentID {
			continue
		}
		if _, skip := exclude[sl.ID]; skip {
			break
		}
		if sl.available(now) && (filter == nil || filter(sl)) && sl.hasQuota(now) {
			return sl, true
		}
		break
	}

	// 2) 改选可用额度最高的账号（并记为当前账号）
	if idx := p.bestQuotaIndex(slots, exclude, now, filter); idx >= 0 {
		p.setCurrentAuto(slots[idx].ID)
		return slots[idx], true
	}

	// 3) 兜底：轮询扫描（额度未知/全部无额度数据时仍可发请求）。
	// 选中的账号同样记为当前账号，保证后续请求固定用它。
	p.mu.Lock()
	start := p.cursor
	p.mu.Unlock()
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		sl := slots[idx]
		if _, skip := exclude[sl.ID]; skip {
			continue
		}
		if !sl.available(now) {
			continue
		}
		if filter != nil && !filter(sl) {
			continue
		}
		p.mu.Lock()
		p.cursor = (idx + 1) % n
		p.mu.Unlock()
		p.setCurrentAuto(sl.ID)
		return sl, true
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

// Uncool 立即解除指定账号的冷却；返回是否找到该账号。
func (p *Pool) Uncool(id string) bool {
	for _, s := range p.snapshot() {
		if s.ID == id {
			s.uncool()
			logger.Info("pool: account %s cooldown cleared", id)
			p.save()
			return true
		}
	}
	return false
}

// Contains 判断账号是否已经在当前池里（新增账号后池需要重建）。
func (p *Pool) Contains(id string) bool {
	if p == nil || id == "" {
		return false
	}
	for _, s := range p.snapshot() {
		if s.ID == id {
			return true
		}
	}
	return false
}

// UncoolAll 解除池内所有账号的冷却（额度重置/充值后的手动兜底）。
func (p *Pool) UncoolAll() {
	for _, s := range p.snapshot() {
		s.uncool()
	}
	logger.Info("pool: all account cooldowns cleared")
	p.save()
}

// SetQuota 更新某个账号的剩余额度缓存（供自动选号排序与额度兜底）。
func (p *Pool) SetQuota(id string, remaining, orgRemaining float64, orgAvailable bool) (before, after float64) {
	for _, s := range p.snapshot() {
		if s.ID == id {
			return s.SetQuota(remaining, orgRemaining, orgAvailable)
		}
	}
	return 0, 0
}

// quotaFreshFor 额度缓存在该时长内才参与排序。
const quotaFreshFor = 30 * time.Minute

// bestQuotaIndex 返回剩余额度最高且可用的 slot 下标；无可用额度数据返回 -1。
func (p *Pool) bestQuotaIndex(slots []*Slot, exclude map[string]struct{}, now time.Time, filter func(*Slot) bool) int {
	best := -1
	bestRem := 0.0
	var ties []int
	for i, s := range slots {
		if _, skip := exclude[s.ID]; skip || !s.available(now) {
			continue
		}
		if filter != nil && !filter(s) {
			continue
		}
		rem, at := s.rankQuota()
		if at.IsZero() || now.Sub(at) > quotaFreshFor {
			continue
		}
		switch {
		case best < 0 || rem > bestRem:
			best, bestRem, ties = i, rem, []int{i}
		case rem == bestRem:
			ties = append(ties, i)
		}
	}
	if best < 0 {
		return -1
	}
	if len(ties) > 1 {
		return ties[rand.IntN(len(ties))]
	}
	return best
}

// setCurrentAuto 把当前路由账号设为 id（自动来源）。
func (p *Pool) setCurrentAuto(id string) {
	p.mu.Lock()
	changed := p.pinned != id || p.pinnedManual
	p.pinned = id
	p.pinnedManual = false
	p.mu.Unlock()
	if changed {
		logger.Info("pool: current routing account -> %s (auto)", id)
	}
}

// PinAccount 手动选择当前路由账号（控制台「设为当前」）。
func (p *Pool) PinAccount(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	p.pinned = id
	p.pinnedManual = true
	p.mu.Unlock()
	logger.Info("pool: current routing account -> %s (manual)", id)
}

// UnpinAccount 清除当前路由账号（不自动选号）。
func (p *Pool) UnpinAccount() {
	if p == nil {
		return
	}
	p.mu.Lock()
	was := p.pinned
	p.pinned = ""
	p.pinnedManual = false
	p.mu.Unlock()
	if was != "" {
		logger.Info("pool: cleared current routing account %s", was)
	}
}

// AutoSelectCurrent 立即把当前路由账号改为剩余额度最高的账号。
func (p *Pool) AutoSelectCurrent() string {
	if p == nil {
		return ""
	}
	slots := p.snapshot()
	now := time.Now()
	idx := p.bestQuotaIndex(slots, nil, now, nil)
	if idx < 0 {
		p.UnpinAccount()
		return ""
	}
	p.setCurrentAuto(slots[idx].ID)
	return slots[idx].ID
}

// RefreshCurrent 在额度刷新后校正当前路由账号：
// 闲置过期、额度耗尽或账号不存在时，立即改选额度最高的账号。
func (p *Pool) RefreshCurrent(now time.Time) {
	if p == nil {
		return
	}
	p.mu.Lock()
	cur := p.pinned
	p.mu.Unlock()

	slots := p.snapshot()
	var curSlot *Slot
	for _, sl := range slots {
		if sl.ID == cur {
			curSlot = sl
			break
		}
	}
	noQuota := curSlot != nil && !curSlot.hasQuota(now)
	if cur != "" && curSlot != nil && !noQuota {
		return // 当前账号仍然健康；额度刷新不会因闲置换号
	}
	idx := p.bestQuotaIndex(slots, nil, now, nil)
	if idx < 0 {
		if noQuota || cur == "" || curSlot == nil {
			p.UnpinAccount()
		}
		return
	}
	p.mu.Lock()
	sameCurrent := p.pinned == cur
	p.mu.Unlock()
	if !sameCurrent {
		return // 期间被手动改选，尊重手动选择
	}
	p.setCurrentAuto(slots[idx].ID)
}

// NoteSuccess 清除账号的失败标记。
func (p *Pool) NoteSuccess(id string) {
	for _, s := range p.snapshot() {
		if s.ID == id {
			s.noteSuccess()
			p.mu.Lock()
			p.lastSuccess = id
			if p.pinned == "" {
				p.pinned = id
				p.pinnedManual = false
			}
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
	Pinned             bool   `json:"pinned,omitempty"`
	PinnedManual       bool   `json:"pinned_manual,omitempty"`
	LastUsed           bool   `json:"last_used,omitempty"`
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
	p.mu.Lock()
	pinned := p.pinned
	manual := p.pinnedManual
	lastUsed := p.lastSuccess
	p.mu.Unlock()
	out := []SlotStatus{}
	for _, s := range p.snapshot() {
		st := s.Status()
		st.Pinned = st.ID == pinned
		st.PinnedManual = st.Pinned && manual
		st.LastUsed = !st.Pinned && st.ID == lastUsed
		out = append(out, st)
	}
	return out
}
