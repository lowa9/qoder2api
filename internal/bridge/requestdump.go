package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"qoder2api/account"
	"qoder2api/logger"
)

const (
	// dumpRateLimit 同一 stream 两次落盘的最小间隔，避免客户端每轮都
	// 改变动态 system 内容时把磁盘写满。指纹日志不受此限制。
	dumpRateLimit = 30 * time.Second
	dumpKeepFiles = 200
	dumpKeepBytes = 1 << 30
	// 系统前缀发生变化时额外保留前后两份请求体，便于事后 diff。
	changeKeepFiles = 100
	changeKeepBytes = 512 << 20
)

type requestFingerprint struct {
	messages         int
	systemSHA        string
	systemNormSHA    string
	systemLen        int
	firstUserSHA     string
	firstUserNormSHA string
	firstUserLen     int
}

func (f requestFingerprint) prefixKey() string {
	if f.systemNormSHA == "" && f.firstUserNormSHA == "" {
		return ""
	}
	// 用归一化后的键判断是否变化：cch/纯 reminder 变化不落盘。
	return f.systemNormSHA + "|" + f.firstUserNormSHA
}

func (f requestFingerprint) normKey() string {
	return f.systemNormSHA + "|" + f.firstUserNormSHA
}

var (
	dumpMu       sync.Mutex
	dumpSeen     = map[string]map[string]string{} // stream -> prefixKey -> dump path
	dumpLastPath = map[string]string{}            // stream -> 最近一次落盘路径
	dumpLastNorm = map[string]string{}            // stream -> 最近一次落盘的标准化前缀键
	dumpLastAt   = map[string]time.Time{}         // stream -> 最近一次落盘时间
)

// DumpRequestBody 在会话前缀（system + 首条 user）发生变化时把原始请求体
// 落盘，供事后排查客户端到底发了什么。相同前缀只落盘一次；请求体目录有
// 容量上限。设置 QODER2API_DUMP_BODIES=off 可关闭。
func DumpRequestBody(protocol, reqID, model, clientIP string, raw []byte, messages []interface{}) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("QODER2API_DUMP_BODIES")), "off") {
		return
	}
	fp := fingerprintMessages(messages)
	key := fp.prefixKey()
	if key == "" {
		return
	}
	normKey := fp.normKey()
	stream := protocol + "|" + model + "|" + clientIP

	dumpMu.Lock()
	_, known := dumpSeen[stream][key]
	lastAt := dumpLastAt[stream]
	lastNorm := dumpLastNorm[stream]
	dumpMu.Unlock()
	if known {
		return
	}
	// 只有「动态提醒变化、真正的前缀内容没变」时才限流；真正的前缀
	// 变化要立刻落盘，否则可能错过唯一的现场。
	if !lastAt.IsZero() && time.Since(lastAt) < dumpRateLimit && normKey == lastNorm {
		dumpMu.Lock()
		if dumpSeen[stream] == nil {
			dumpSeen[stream] = map[string]string{}
		}
		dumpSeen[stream][key] = ""
		dumpMu.Unlock()
		logger.Info("[reqdump][%s][%s] reminder churn (sys_sha=%s first_sha=%s, dump rate-limited)", protocol, reqID, fp.systemSHA, fp.firstUserSHA)
		return
	}

	path, err := writeRequestDump(protocol, reqID, model, raw)
	if err != nil {
		logger.Error("[reqdump][%s][%s] 请求体落盘失败: %v", protocol, reqID, err)
		return
	}

	dumpMu.Lock()
	seen := dumpSeen[stream]
	if seen == nil {
		seen = map[string]string{}
		dumpSeen[stream] = seen
	}
	prevPath, hadPrev := dumpLastPath[stream]
	seen[key] = path
	dumpLastPath[stream] = path
	dumpLastNorm[stream] = normKey
	dumpLastAt[stream] = time.Now()
	dumpMu.Unlock()

	logger.Info("[reqdump][%s][%s] saved %s size=%d msgs=%d sys_len=%d sys_sha=%s sys_norm_sha=%s first_len=%d first_sha=%s first_norm_sha=%s",
		protocol, reqID, path, len(raw), fp.messages, fp.systemLen, fp.systemSHA,
		fp.systemNormSHA, fp.firstUserLen, fp.firstUserSHA, fp.firstUserNormSHA)

	if hadPrev && prevPath != "" {
		preservePrefixChange(protocol, reqID, prevPath, path)
	}
}

func fingerprintMessages(messages []interface{}) requestFingerprint {
	sys, firstUser := messagePrefix(messages)
	return requestFingerprint{
		messages:         len(messages),
		systemSHA:        shortSHA(sys),
		systemNormSHA:    shortSHA(stripClientMarkers(stripSystemReminders(sys))),
		systemLen:        len(sys),
		firstUserSHA:     shortSHA(firstUser),
		firstUserNormSHA: shortSHA(stripClientMarkers(stripSystemReminders(firstUser))),
		firstUserLen:     len(firstUser),
	}
}

func shortSHA(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func writeRequestDump(protocol, reqID, model string, raw []byte) (string, error) {
	dir := filepath.Join(account.DataRoot(), "request_dumps")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s-%s-%s.json", time.Now().Format("20060102-150405.000"), protocol, reqID, safeFilePart(model))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	pruneDir(dir, dumpKeepFiles, dumpKeepBytes)
	return path, nil
}

func preservePrefixChange(protocol, reqID, prevPath, newPath string) {
	dir := filepath.Join(account.DataRoot(), "request_dumps", "changes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.Error("[reqdump][%s] 创建 changes 目录失败: %v", reqID, err)
		return
	}
	base := fmt.Sprintf("%s-%s-%s", time.Now().Format("20060102-150405"), protocol, reqID)
	prevDst := filepath.Join(dir, base+"-prev.json")
	newDst := filepath.Join(dir, base+"-new.json")
	if err := copyFile(prevPath, prevDst); err != nil {
		logger.Error("[reqdump][%s] 保留旧请求体失败: %v", reqID, err)
		prevDst = ""
	}
	if err := copyFile(newPath, newDst); err != nil {
		logger.Error("[reqdump][%s] 保留新请求体失败: %v", reqID, err)
		newDst = ""
	}
	logger.Info("[reqdump][%s] session prefix changed; preserved prev=%s new=%s", reqID, prevDst, newDst)
	pruneDir(dir, changeKeepFiles, changeKeepBytes)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func pruneDir(dir string, maxFiles int, maxBytes int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		path string
		size int64
		mod  time.Time
	}
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{filepath.Join(dir, e.Name()), info.Size(), info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	var kept int
	var keptBytes int64
	for _, it := range items {
		if kept >= maxFiles || (kept > 0 && keptBytes+it.size > maxBytes) {
			_ = os.Remove(it.path)
			continue
		}
		kept++
		keptBytes += it.size
	}
}

func safeFilePart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
