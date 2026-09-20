package web

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// 飞牛 fnOS 统一网关在完成 NAS 登录态校验后，会把当前用户身份写入下列请求头。
// 参考：https://developer.fnnas.com/docs/core-concepts/gateway-registration
const (
	gatewayUserIDHeader   = "X-Trim-Userid"
	gatewayUsernameHeader = "X-Trim-Username"
	gatewayIsAdminHeader  = "X-Trim-Isadmin"
)

// 请求上下文中的身份键，供路由处理函数通过 CurrentUser 读取。
const (
	ctxKeyAuthUserID   = "AuthUserID"
	ctxKeyAuthUsername = "AuthUsername"
	ctxKeyAuthIsAdmin  = "AuthIsAdmin"
	ctxKeyAuthSource   = "AuthSource"
)

// 身份来源：飞牛 fnOS 网关，或应用内置的管理员账号会话。
const (
	authSourceGateway = "fnos-gateway"
	authSourceLocal   = "local"
)

// gatewayConnKey 标记连接是否来自统一网关使用的 Unix Socket。
//
// X-Trim-* 请求头只有在请求确实由飞牛 fnOS 网关转发时才可信：如果服务同时监听
// TCP 端口，任何能访问该端口的客户端都能自行伪造这些请求头，因此这里按连接类型区分。
type gatewayConnKey struct{}

// withGatewayConn 由 http.Server.ConnContext 调用，记录当前连接是否为 Unix Socket。
func withGatewayConn(ctx context.Context, conn net.Conn) context.Context {
	_, isUnix := conn.(*net.UnixConn)
	return context.WithValue(ctx, gatewayConnKey{}, isUnix)
}

func requestFromUnixConn(r *http.Request) bool {
	if r == nil {
		return false
	}
	isUnix, _ := r.Context().Value(gatewayConnKey{}).(bool)
	return isUnix
}

// gatewayIdentity 是飞牛 fnOS 网关转发的当前 NAS 用户。
type gatewayIdentity struct {
	UID      string
	Username string
	IsAdmin  bool
}

func parseGatewayIdentity(header http.Header) (gatewayIdentity, bool) {
	if header == nil {
		return gatewayIdentity{}, false
	}
	identity := gatewayIdentity{
		UID:      strings.TrimSpace(header.Get(gatewayUserIDHeader)),
		Username: strings.TrimSpace(header.Get(gatewayUsernameHeader)),
	}
	if identity.UID == "" {
		return gatewayIdentity{}, false
	}
	if identity.Username == "" {
		identity.Username = identity.UID
	}
	identity.IsAdmin = strings.EqualFold(strings.TrimSpace(header.Get(gatewayIsAdminHeader)), "true")
	return identity, true
}

// trustedGatewayIdentity 只在启用了网关鉴权、且请求来自 Unix Socket 时返回网关身份。
func trustedGatewayIdentity(c *gin.Context, gatewayAuth bool) (gatewayIdentity, bool) {
	if !gatewayAuth || c == nil || !requestFromUnixConn(c.Request) {
		return gatewayIdentity{}, false
	}
	return parseGatewayIdentity(c.Request.Header)
}

func setGatewayAuthContext(c *gin.Context, identity gatewayIdentity) {
	c.Set(ctxKeyAuthUserID, identity.UID)
	c.Set(ctxKeyAuthUsername, identity.Username)
	c.Set(ctxKeyAuthIsAdmin, identity.IsAdmin)
	c.Set(ctxKeyAuthSource, authSourceGateway)
}

// CurrentUser 返回当前请求对应的用户身份。
// uid 与 isAdmin 在飞牛 fnOS 网关鉴权下来自 NAS 用户信息；使用内置管理员会话时为内置账号。
func CurrentUser(c *gin.Context) (uid string, username string, isAdmin bool) {
	if c == nil {
		return "", "", false
	}
	if value, ok := c.Get(ctxKeyAuthUserID); ok {
		uid, _ = value.(string)
	}
	if value, ok := c.Get(ctxKeyAuthUsername); ok {
		username, _ = value.(string)
	}
	if value, ok := c.Get(ctxKeyAuthIsAdmin); ok {
		isAdmin, _ = value.(bool)
	}
	return uid, username, isAdmin
}
