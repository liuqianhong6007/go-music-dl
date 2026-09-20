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
