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
func (s *Slot) SetQuota(remaining, orgRemaining float64, orgAvailable bool) {
	s.quotaMu.Lock()
	s.quotaRemaining = remaining
	s.orgRemaining = orgRemaining
	s.orgAvailable = orgAvailable
	s.quotaAt = time.Now()
	s.quotaMu.Unlock()
}

// quota 返回账号自身剩余额度与采集时间。
func (s *Slot) quota() (float64, time.Time) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	return s.quotaRemaining, s.quotaAt
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

// Pool 是账号池：轮询选号，跳过冷却中与已试过的账号。
type Pool struct {
	mu          sync.Mutex
	slots       []*Slot
	cursor      int
	lastSuccess string // 最近一次成功调用使用的账号 id（全局粘性，无会话标识时使用）
	// sessions 会话 -> 账号绑定。新会话随机选号，同一会话后续复用，
	// 使多个客户端会话各自落在不同账号上并保持缓存热度。
	sessions map[string]sessionBinding
	// groups 前缀组（system prompt + 模型）-> 账号绑定。
	// 同一 system 的新会话（上下文压缩、续写）复用同一账号以命中缓存。
	groups map[string]sessionBinding
	// pinned 当前路由账号：手动「设为当前」或自动选出（额度最高）。
	// 它低于会话/前缀组绑定，是"未绑定请求的默认账号 + 绑定账号没额度时的兜底"。
	// 闲置超过 pinIdleTTL 后自动改选额度最高的账号。
	pinned       string
	pinnedManual bool
	pinnedAt     time.Time
	statePath    string // 非空时把冷却状态持久化到该文件（重启不丢）
}

// sessionBinding 一个会话当前绑定的账号。
type sessionBinding struct {
	accountID string
	seen      time.Time
}

const (
	// affinityIdleTTL 会话 / 前缀组 / 手动固定的统一闲置有效期。
	// 上游缓存 TTL 实测约 30–60 分钟（≥16 分钟命中、约 54 分钟失效），
	// 2 小时足以覆盖；再长同账号也会冷启动，固定/绑定已无缓存收益。
	affinityIdleTTL = 2 * time.Hour
	// sessionBindingTTL 会话绑定的闲置有效期；超过后视为新会话重新选号。
	sessionBindingTTL = affinityIdleTTL
	// groupBindingTTL 前缀组（model + system）绑定有效期，与会话/固定一致。
	groupBindingTTL = affinityIdleTTL
	// pinIdleTTL 手动固定账号的闲置有效期。
	pinIdleTTL = affinityIdleTTL
	// maxSessionBindings 绑定表上限，超出时淘汰最久未使用的条目。
	maxSessionBindings = 1024
	// maxGroupBindings 前缀组绑定表上限。
	maxGroupBindings = 256
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
	return p.pick("", "", exclude, filters...)
}

// PickWithSession 会话感知选号：
//   - 该会话已有绑定且账号可用 → 复用（缓存热）
//   - 新会话/绑定失效 → 随机起点（多个会话分散到不同账号）
//   - 会话为空时退化为原有全局粘性 + 冷启动随机
func (p *Pool) PickWithSession(sessionKey string, exclude map[string]struct{}, filters ...func(*Slot) bool) (*Slot, bool) {
	return p.pick(sessionKey, "", exclude, filters...)
}

// PickWithAffinity 会话 + 前缀组双键选号。
// 会话绑定优先；没有会话绑定时，相同 system 前缀（前缀组）复用同一账号。
func (p *Pool) PickWithAffinity(sessionKey, groupKey string, exclude map[string]struct{}, filters ...func(*Slot) bool) (*Slot, bool) {
	return p.pick(sessionKey, groupKey, exclude, filters...)
}

func (p *Pool) pick(sessionKey, groupKey string, exclude map[string]struct{}, filters ...func(*Slot) bool) (*Slot, bool) {
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
	currentID := ""
	sessionAcc := ""
	groupAcc := ""
	if stickyEnabled.Load() {
		currentID = p.pinned
		// 当前路由账号闲置过期：清除，稍后立即改选额度最高
		if currentID != "" && !p.pinnedAt.IsZero() && now.Sub(p.pinnedAt) > pinIdleTTL {
			logger.Info("pool: current routing account %s expired after %s idle", currentID, pinIdleTTL)
			currentID = ""
			p.pinned = ""
			p.pinnedManual = false
			p.pinnedAt = time.Time{}
		}
		if sessionKey != "" {
			if b, ok := p.sessions[sessionKey]; ok && now.Sub(b.seen) < sessionBindingTTL {
				sessionAcc = b.accountID
			}
		}
		if groupKey != "" {
			if b, ok := p.groups[groupKey]; ok && now.Sub(b.seen) < groupBindingTTL {
				groupAcc = b.accountID
			}
		}
	}
	p.mu.Unlock()
	// 关闭粘性时退化为纯轮询
	if !stickyEnabled.Load() {
		sessionKey, groupKey = "", ""
		sessionAcc, groupAcc, currentID = "", "", ""
	}

	usable := func(id string, requireQuota bool) *Slot {
		if id == "" {
			return nil
		}
		if _, skip := exclude[id]; skip {
			return nil
		}
		for _, sl := range slots {
			if sl.ID != id || !sl.available(now) {
				continue
			}
			if filter != nil && !filter(sl) {
				return nil
			}
			if requireQuota && !sl.hasQuota(now) {
				return nil
			}
			return sl
		}
		return nil
	}

	// 1) 会话绑定（额度耗尽时跳过，走兜底）
	if s := usable(sessionAcc, true); s != nil {
		return s, true
	}
	// 2) 前缀组绑定（同上）
	if s := usable(groupAcc, true); s != nil {
		return s, true
	}
	// 3) 当前路由账号（手动选择或上次自动选择）
	if s := usable(currentID, true); s != nil {
		return s, true
	}

	// 4) 兜底：选择剩余额度最高的账号。
	// 当前账号为空/额度耗尽/被删除时，立即把它改写为这个自动选出的账号；
	// 仅冷却时保留原当前账号，本次请求借用其他账号。
	autoIdx := p.bestQuotaIndex(slots, exclude, now, filter)
	currentSlot := (*Slot)(nil)
	for _, sl := range slots {
		if sl.ID == currentID {
			currentSlot = sl
			break
		}
	}
	needNewCurrent := currentID == "" || currentSlot == nil ||
		!currentSlot.available(now) || !currentSlot.hasQuota(now)
	if stickyEnabled.Load() && autoIdx >= 0 {
		if needNewCurrent {
			p.setCurrentAuto(slots[autoIdx].ID, now)
		}
		start = autoIdx
	} else if stickyEnabled.Load() && poolRandIndex != nil {
		if (sessionKey != "" || groupKey != "") && sessionAcc == "" && groupAcc == "" {
			start = poolRandIndex(n)
		} else if sessionKey == "" && groupKey == "" && stickyID == "" {
			start = poolRandIndex(n)
		}
	}

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

// SetQuota 更新某个账号的剩余额度缓存（供自动选号排序与额度兜底）。
func (p *Pool) SetQuota(id string, remaining, orgRemaining float64, orgAvailable bool) {
	for _, s := range p.snapshot() {
		if s.ID == id {
			s.SetQuota(remaining, orgRemaining, orgAvailable)
			return
		}
	}
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
		rem, at := s.quota()
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
	if len(ties) > 1 && poolRandIndex != nil {
		return ties[poolRandIndex(len(ties))]
	}
	return best
}

// setCurrentAuto 把当前路由账号设为 id（自动来源）。
func (p *Pool) setCurrentAuto(id string, now time.Time) {
	p.mu.Lock()
	changed := p.pinned != id || p.pinnedManual
	p.pinned = id
	p.pinnedManual = false
	p.pinnedAt = now
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
	p.pinnedAt = time.Now()
	p.mu.Unlock()
	logger.Info("pool: current routing account -> %s (manual, idle TTL %s)", id, pinIdleTTL)
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
	p.pinnedAt = time.Time{}
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
	p.setCurrentAuto(slots[idx].ID, now)
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
	at := p.pinnedAt
	p.mu.Unlock()

	slots := p.snapshot()
	var curSlot *Slot
	for _, sl := range slots {
		if sl.ID == cur {
			curSlot = sl
			break
		}
	}
	expired := cur != "" && (at.IsZero() || now.Sub(at) > pinIdleTTL)
	noQuota := curSlot != nil && !curSlot.hasQuota(now)
	if cur != "" && curSlot != nil && !expired && !noQuota {
		return // 当前账号仍然健康
	}
	idx := p.bestQuotaIndex(slots, nil, now, nil)
	if idx < 0 {
		if expired || noQuota || cur == "" || curSlot == nil {
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
	p.setCurrentAuto(slots[idx].ID, now)
}

// NoteGroupSuccess 记录前缀组与账号的绑定。
func (p *Pool) NoteGroupSuccess(groupKey, id string) {
	if p == nil || groupKey == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.groups == nil {
		p.groups = map[string]sessionBinding{}
	}
	now := time.Now()
	p.groups[groupKey] = sessionBinding{accountID: id, seen: now}
	if len(p.groups) > maxGroupBindings {
		for k, b := range p.groups {
			if now.Sub(b.seen) >= groupBindingTTL {
				delete(p.groups, k)
			}
		}
		for len(p.groups) > maxGroupBindings {
			oldestKey := ""
			var oldest time.Time
			for k, b := range p.groups {
				if oldestKey == "" || b.seen.Before(oldest) {
					oldestKey, oldest = k, b.seen
				}
			}
			delete(p.groups, oldestKey)
		}
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
			now := time.Now()
			switch {
			case p.pinned == id:
				p.pinnedAt = now // 有流量即续期
			case p.pinned == "":
				p.pinned = id
				p.pinnedManual = false
				p.pinnedAt = now
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
