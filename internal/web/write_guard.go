package web

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

func wantsSaveLocal(c *gin.Context) bool {
	return c != nil && strings.TrimSpace(c.Query("save_local")) == "1"
}

func allowSameOriginWrite(c *gin.Context) bool {
	if c == nil {
		return false
	}
	if c.GetHeader("X-Requested-With") != "XMLHttpRequest" {
		logWriteGuardRejection(c, "missing X-Requested-With header")
		return false
	}

	// 现代浏览器都会带上 Sec-Fetch-Site，且页面脚本无法伪造，优先采信。
	switch strings.ToLower(strings.TrimSpace(c.GetHeader("Sec-Fetch-Site"))) {
	case "same-origin", "same-site", "none":
		return true
	}

	origin := strings.TrimSpace(c.GetHeader("Origin"))
	if origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil {
			logWriteGuardRejection(c, "invalid Origin header")
			return false
		}
		for _, candidate := range requestHostCandidates(c) {
			if sameRequestHost(parsed.Host, candidate) {
				return true
			}
		}
		logWriteGuardRejection(c, "Origin does not match request host")
		return false
	}

	secFetchSite := strings.TrimSpace(strings.ToLower(c.GetHeader("Sec-Fetch-Site")))
	return secFetchSite == "" || secFetchSite == "same-origin" || secFetchSite == "same-site" || secFetchSite == "none"
}

// requestHostCandidates 列出可用于同源比较的请求主机。
// 经过反向代理（例如飞牛 fnOS 统一网关）时 Request.Host 可能已被改写，
// 因此同时参考 X-Forwarded-Host。
func requestHostCandidates(c *gin.Context) []string {
	candidates := make([]string, 0, 2)
	if forwarded := strings.TrimSpace(c.GetHeader("X-Forwarded-Host")); forwarded != "" {
		if first := strings.TrimSpace(strings.Split(forwarded, ",")[0]); first != "" {
			candidates = append(candidates, first)
		}
	}
	if host := strings.TrimSpace(c.Request.Host); host != "" {
		candidates = append(candidates, host)
	}
	return candidates
}

// sameRequestHost 比较两个主机是否等价。反向代理常把端口丢掉，
// 例如 Origin 为 http://nas:8000 而 Host 只有 nas，这里一并视为同源。
func sameRequestHost(a string, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(a, b) {
		return true
	}
	return strings.EqualFold(hostWithoutPort(a), hostWithoutPort(b))
}

func hostWithoutPort(host string) string {
	if trimmed, _, err := net.SplitHostPort(host); err == nil {
		return trimmed
	}
	return host
}

// logWriteGuardRejection 记录被拒绝的写入请求，便于在反代环境下排查来源校验问题。
func logWriteGuardRejection(c *gin.Context, reason string) {
	if c == nil || c.Request == nil {
		return
	}
	log.Printf(
		"write guard rejected %s %s: %s (origin=%q host=%q x-forwarded-host=%q sec-fetch-site=%q)",
		c.Request.Method,
		c.Request.URL.RequestURI(),
		reason,
		c.GetHeader("Origin"),
		c.Request.Host,
		c.GetHeader("X-Forwarded-Host"),
		c.GetHeader("Sec-Fetch-Site"),
	)
}

func allowSaveLocalRequest(c *gin.Context) bool {
	if !wantsSaveLocal(c) {
		return false
	}
	if c.Request.Method != http.MethodPost {
		c.AbortWithStatusJSON(http.StatusMethodNotAllowed, gin.H{"error": "save_local requires POST"})
		return false
	}
	if !allowSameOriginWrite(c) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return false
	}
	return true
}
