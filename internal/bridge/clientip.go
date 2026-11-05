package bridge

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ── 调用来源 IP ─────────────────────────────────────────────────────────
//
// 规则：
//  1. 直连请求以 TCP 对端（RemoteAddr）为准；
//  2. 只有对端是本机地址（反向代理运行在 loopback）时才信任
//     X-Forwarded-For / X-Real-IP，避免远端客户端伪造/代理误写 127.0.0.1；
//  3. 本机接口地址（含 Tailscale 100.64/10）统一归一化为 127.0.0.1；
//  4. XFF 链从前往后跳过本机/代理地址，取第一个真实客户端地址。

var localIPCache struct {
	sync.Mutex
	at  time.Time
	set map[string]bool
}

// localIPSet 返回本机接口地址集合（1 分钟缓存）。
func localIPSet() map[string]bool {
	localIPCache.Lock()
	defer localIPCache.Unlock()
	if localIPCache.set != nil && time.Since(localIPCache.at) < time.Minute {
		return localIPCache.set
	}
	set := map[string]bool{"127.0.0.1": true, "::1": true}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil {
				set[ip.String()] = true
			}
		}
	}
	localIPCache.set = set
	localIPCache.at = time.Now()
	return set
}

// hostOnly 去掉端口、IPv6 方括号与 zone 后缀。
func hostOnly(addr string) string {
	addr = strings.TrimSpace(addr)
	if h, _, err := net.SplitHostPort(addr); err == nil {
		addr = h
	}
	addr = strings.Trim(addr, "[]")
	if i := strings.Index(addr, "%"); i >= 0 {
		addr = addr[:i]
	}
	return addr
}

func isLocalIP(ip string) bool {
	return localIPSet()[hostOnly(ip)]
}

// requestClientIP 解析调用来源 IP。
func requestClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	peer := hostOnly(r.RemoteAddr)
	if peer == "" {
		return ""
	}
	// 远端直连：以 TCP 对端为准，不信任可能被伪造的代理头
	if !isLocalIP(peer) {
		return peer
	}
	// 本机/反代：优先取代理头里的真实客户端
	if ip := forwardedClientIP(r.Header); ip != "" {
		return ip
	}
	return "127.0.0.1"
}

// forwardedClientIP 解析 X-Forwarded-For 链，跳过本机/代理地址；
// 全部都是本机地址时退回首段。最后尝试 X-Real-IP。
func forwardedClientIP(h http.Header) string {
	if xff := strings.TrimSpace(h.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		for _, p := range parts {
			ip := hostOnly(p)
			if ip == "" || isLocalIP(ip) {
				continue
			}
			return ip
		}
		if ip := hostOnly(parts[0]); ip != "" {
			return ip
		}
	}
	if real := hostOnly(h.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}
