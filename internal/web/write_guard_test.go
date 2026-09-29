package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAllowSameOriginWriteBehindReverseProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name          string
		requestHost   string
		origin        string
		forwardedHost string
		secFetchSite  string
		want          bool
	}{
		{
			name:        "same host and port",
			requestHost: "example.test:8000",
			origin:      "http://example.test:8000",
			want:        true,
		},
		{
			// 飞牛 fnOS 统一网关转发时会改写 Host，丢掉端口。
			name:        "proxy drops port from host header",
			requestHost: "example.test",
			origin:      "http://example.test:8000",
			want:        true,
		},
		{
			name:          "proxy rewrites host but keeps X-Forwarded-Host",
			requestHost:   "127.0.0.1",
			forwardedHost: "nas.example.test:8000",
			origin:        "http://nas.example.test:8000",
			want:          true,
		},
		{
			name:         "browser reports same-origin fetch metadata",
			requestHost:  "localhost",
			origin:       "https://nas.example.test",
			secFetchSite: "same-origin",
			want:         true,
		},
		{
			// 回归：same-site 只代表注册域相同。攻击者页面位于同一注册域下的另一个
			// 子域（DDNS 场景很常见），不能因此跳过 Origin 校验。
			name:         "same-site with foreign origin stays rejected",
			requestHost:  "nas.ddns.net:8000",
			origin:       "https://evil.ddns.net",
			secFetchSite: "same-site",
			want:         false,
		},
		{
			// 合法情况：same-site 且 Origin 确实与请求主机一致，走 Origin 校验放行。
			name:         "same-site with matching origin passes origin check",
			requestHost:  "nas.ddns.net:8000",
			origin:       "https://nas.ddns.net:8000",
			secFetchSite: "same-site",
			want:         true,
		},
		{
			// 没有 Origin 时也不再因 same-site 而隐式放行。
			name:         "same-site without origin stays rejected",
			requestHost:  "nas.ddns.net:8000",
			secFetchSite: "same-site",
			want:         false,
		},
		{
			// 用户直接在地址栏发起（无 Origin），不是脚本可构造的场景。
			name:         "none fetch metadata from user navigation is allowed",
			requestHost:  "nas.example.test:8000",
			secFetchSite: "none",
			want:         true,
		},
		{
			name:         "cross-site fetch metadata stays rejected",
			requestHost:  "nas.example.test:8000",
			origin:       "https://evil.example",
			secFetchSite: "cross-site",
			want:         false,
		},
		{
			name:        "different host stays rejected",
			requestHost: "nas.example.test",
			origin:      "https://evil.example",
			want:        false,
		},
		{
			name:        "no origin header falls back to allowing xhr",
			requestHost: "nas.example.test",
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			req := httptest.NewRequest(http.MethodPost, "http://"+tt.requestHost+RoutePrefix+"/download?save_local=1", nil)
			req.Host = tt.requestHost
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.forwardedHost != "" {
				req.Header.Set("X-Forwarded-Host", tt.forwardedHost)
			}
			if tt.secFetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tt.secFetchSite)
			}
			c.Request = req

			if got := allowSameOriginWrite(c); got != tt.want {
				t.Fatalf("allowSameOriginWrite = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllowSameOriginWriteRejectsSameSiteCSRF(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// same-site 曾被当作可信直接放行，于是：目标 nas.ddns.net，攻击者页面
	// evil.ddns.net（同一 DDNS 注册域，浏览器会给出 Sec-Fetch-Site: same-site），
	// 配合 CORS 中间件放行 X-Requested-With，预检可过 —— 攻击者就能 CSRF 触发
	// save_local，把文件写进 NAS 磁盘。这里把该场景钉死。
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(
		http.MethodPost,
		"http://nas.ddns.net:8000"+RoutePrefix+"/download?save_local=1",
		nil,
	)
	req.Host = "nas.ddns.net:8000"
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", "https://evil.ddns.net")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	c.Request = req

	if allowSameOriginWrite(c) {
		t.Fatal("same-site + 不同 Origin 必须被拒绝，否则同注册域即可 CSRF 写入 NAS")
	}
}

func TestHostWithoutPort(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "nas.example.test:8000", want: "nas.example.test"},
		{in: "nas.example.test", want: "nas.example.test"},
		{in: "192.168.1.10:5666", want: "192.168.1.10"},
		{in: "[fd00::1]:8000", want: "fd00::1"},
	}
	for _, tt := range tests {
		if got := hostWithoutPort(tt.in); got != tt.want {
			t.Fatalf("hostWithoutPort(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
