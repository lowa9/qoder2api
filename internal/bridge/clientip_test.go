package bridge

import (
	"net"
	"net/http/httptest"
	"testing"
)

func TestRequestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		xff    string
		real   string
		want   string
	}{
		{"remote direct ignores XFF", "203.0.113.7:5555", "127.0.0.1", "", "203.0.113.7"},
		{"remote direct no header", "100.72.245.69:5555", "", "", "100.72.245.69"},
		{"local proxy XFF chain", "127.0.0.1:5555", "203.0.113.7, 127.0.0.1", "", "203.0.113.7"},
		{"local proxy all local", "127.0.0.1:5555", "127.0.0.1", "", "127.0.0.1"},
		{"local proxy X-Real-IP", "127.0.0.1:5555", "", "203.0.113.9", "203.0.113.9"},
		{"ipv6 loopback", "[::1]:5555", "", "", "127.0.0.1"},
		{"ipv6 remote", "[2001:db8::1]:5555", "", "", "2001:db8::1"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "http://example/", nil)
		req.RemoteAddr = c.remote
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.real != "" {
			req.Header.Set("X-Real-IP", c.real)
		}
		if got := requestClientIP(req); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestRequestClientIPLocalInterface 验证本机接口地址（含 Tailscale）归一化为 127.0.0.1。
func TestRequestClientIPLocalInterface(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip("InterfaceAddrs unavailable")
	}
	var local string
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			local = ipnet.IP.String()
			break
		}
	}
	if local == "" {
		t.Skip("no non-loopback interface")
	}
	req := httptest.NewRequest("GET", "http://example/", nil)
	req.RemoteAddr = net.JoinHostPort(local, "5555")
	if got := requestClientIP(req); got != "127.0.0.1" {
		t.Errorf("local interface %s: got %q want 127.0.0.1", local, got)
	}
}
