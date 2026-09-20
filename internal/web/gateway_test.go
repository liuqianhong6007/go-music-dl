package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/guohuiyuan/go-music-dl/core"
)

func gatewayHeaders(uid string, username string, isAdmin string) http.Header {
	header := http.Header{}
	if uid != "" {
		header.Set(gatewayUserIDHeader, uid)
	}
	if username != "" {
		header.Set(gatewayUsernameHeader, username)
	}
	if isAdmin != "" {
		header.Set(gatewayIsAdminHeader, isAdmin)
	}
	return header
}

// withGatewaySource 模拟由 Unix Socket 连接产生的请求上下文。
func withGatewaySource(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), gatewayConnKey{}, true))
}

func TestParseGatewayIdentity(t *testing.T) {
	tests := []struct {
		name        string
		header      http.Header
		wantOK      bool
		wantUID     string
		wantName    string
		wantIsAdmin bool
	}{
		{
			name:        "admin user",
			header:      gatewayHeaders("1000", "admin", "true"),
			wantOK:      true,
			wantUID:     "1000",
			wantName:    "admin",
			wantIsAdmin: true,
		},
		{
			name:     "regular user falls back to uid as name",
			header:   gatewayHeaders("1001", "", "false"),
			wantOK:   true,
			wantUID:  "1001",
			wantName: "1001",
		},
		{
			name:   "missing uid is not an identity",
			header: gatewayHeaders("", "admin", "true"),
		},
		{
			name:   "empty headers",
			header: http.Header{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity, ok := parseGatewayIdentity(tt.header)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if identity.UID != tt.wantUID || identity.Username != tt.wantName || identity.IsAdmin != tt.wantIsAdmin {
				t.Fatalf("identity = %+v, want uid=%q name=%q isAdmin=%v", identity, tt.wantUID, tt.wantName, tt.wantIsAdmin)
			}
		})
	}
}

func TestAuthRequiredTrustsGatewayIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// 未配置内置账号：正常访问应跳转 /setup，但网关身份应当直接放行。
	router.Use(authRequired(func() (core.WebAuthSettings, error) {
		return core.WebAuthSettings{}, nil
	}, routeAuthOptions{gatewayAuth: true}))
	router.GET(RoutePrefix, func(c *gin.Context) {
		uid, username, isAdmin := CurrentUser(c)
		if isAdmin {
			c.String(http.StatusOK, "admin:"+uid+":"+username)
			return
		}
		c.String(http.StatusOK, "user:"+uid+":"+username)
	})

	req := withGatewaySource(httptest.NewRequest(http.MethodGet, RoutePrefix, nil))
	req.Header = gatewayHeaders("1002", "alice", "false")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "user:1002:alice" {
		t.Fatalf("body = %q, want gateway user identity", rec.Body.String())
	}
}

func TestAuthRequiredReadsAdminFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(authRequired(func() (core.WebAuthSettings, error) {
		return core.WebAuthSettings{}, nil
	}, routeAuthOptions{gatewayAuth: true}))
	router.GET(RoutePrefix, func(c *gin.Context) {
		uid, username, isAdmin := CurrentUser(c)
		if isAdmin {
			c.String(http.StatusOK, "admin:"+uid+":"+username)
			return
		}
		c.String(http.StatusOK, "user")
	})

	req := withGatewaySource(httptest.NewRequest(http.MethodGet, RoutePrefix, nil))
	req.Header = gatewayHeaders("1000", "admin", "true")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "admin:1000:admin" {
		t.Fatalf("body = %q, want gateway admin identity", rec.Body.String())
	}
}

func TestAuthRequiredIgnoresGatewayHeadersFromTCP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(authRequired(func() (core.WebAuthSettings, error) {
		return core.WebAuthSettings{}, nil
	}, routeAuthOptions{gatewayAuth: true}))
	router.GET(RoutePrefix, func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	// 没有 Unix Socket 连接标记（等同于 TCP 直连），身份头不可信。
	req := httptest.NewRequest(http.MethodGet, RoutePrefix, nil)
	req.Header = gatewayHeaders("1000", "admin", "true")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuthRequiredIgnoresGatewayHeadersWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(authRequired(func() (core.WebAuthSettings, error) {
		return core.WebAuthSettings{}, nil
	}, routeAuthOptions{}))
	router.GET(RoutePrefix, func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := withGatewaySource(httptest.NewRequest(http.MethodGet, RoutePrefix, nil))
	req.Header = gatewayHeaders("1000", "admin", "true")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestLoginRedirectsWhenGatewayAuthed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group(RoutePrefix)
	bindAuthRoutes(api, true)

	req := withGatewaySource(httptest.NewRequest(http.MethodGet, RoutePrefix+"/login", nil))
	req.Header = gatewayHeaders("1000", "admin", "true")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != RoutePrefix {
		t.Fatalf("Location = %q, want %q", got, RoutePrefix)
	}
}

func TestLoginStillRendersWithoutGatewayIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group(RoutePrefix)
	bindAuthRoutes(api, true)

	req := withGatewaySource(httptest.NewRequest(http.MethodGet, RoutePrefix+"/login", nil))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// 没有身份头时应继续跳转到 /setup（未初始化账号），而不是死循环重定向到自身。
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != RoutePrefix+"/setup" {
		t.Fatalf("Location = %q, want %q", got, RoutePrefix+"/setup")
	}
}

func TestWithGatewayConnMarksUnixConnections(t *testing.T) {
	// Unix Socket 路径长度受限（macOS 约 104 字节），这里用较短的临时目录。
	dir, err := os.MkdirTemp("/tmp", "gw")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socketPath := filepath.Join(dir, "gw.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			// 某些沙箱/CI 环境禁止创建 Unix Socket，跳过而不是误报失败。
			t.Skipf("当前环境不允许创建 Unix Socket：%v", err)
		}
		t.Fatalf("listen unix: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()

	client, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	defer client.Close()

	server, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	defer server.Close()

	if requestFromUnixConn(&http.Request{}) {
		t.Fatal("request without context should not be treated as unix connection")
	}

	ctx := withGatewayConn(context.Background(), server)
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	if !requestFromUnixConn(req) {
		t.Fatal("unix connection should be marked as trusted transport")
	}

	tcpCtx := withGatewayConn(context.Background(), &stubConn{})
	tcpReq := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(tcpCtx)
	if requestFromUnixConn(tcpReq) {
		t.Fatal("non-unix connection must not be marked as trusted transport")
	}
}

type stubConn struct{ net.Conn }

// newRouteGroupsRouter 用真实的路由分组逻辑组装探针路由，验证各分组的鉴权强度。
func newRouteGroupsRouter(t *testing.T, opts StartOptions, settings core.WebAuthSettings) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group(RoutePrefix)
	groups := bindAuthMiddlewareWithProvider(api, opts, func() (core.WebAuthSettings, error) {
		return settings, nil
	})
	probe := func(name string, group *gin.RouterGroup) {
		group.GET("/"+name, func(c *gin.Context) {
			c.String(http.StatusOK, name)
		})
	}
	probe("public", groups.public)
	probe("business", groups.protected)
	probe("config", groups.config)
	return router
}

func TestBusinessRoutesStayPublicByDefault(t *testing.T) {
	router := newRouteGroupsRouter(t, StartOptions{}, core.WebAuthSettings{})

	// 默认行为与 README 一致：普通业务功能无需登录。
	req := httptest.NewRequest(http.MethodGet, RoutePrefix+"/business", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("business status = %d, want %d", rec.Code, http.StatusOK)
	}

	// 系统配置类操作仍然需要登录。
	configReq := httptest.NewRequest(http.MethodGet, RoutePrefix+"/config", nil)
	configReq.Header.Set("Accept", "application/json")
	configRec := httptest.NewRecorder()
	router.ServeHTTP(configRec, configReq)
	if configRec.Code != http.StatusUnauthorized {
		t.Fatalf("config status = %d, want %d", configRec.Code, http.StatusUnauthorized)
	}
}

func TestRequireLoginProtectsBusinessRoutes(t *testing.T) {
	router := newRouteGroupsRouter(t, StartOptions{RequireLogin: true, GatewayAuth: true}, core.WebAuthSettings{})

	req := httptest.NewRequest(http.MethodGet, RoutePrefix+"/business", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("business status without identity = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// 飞牛网关身份可以直接满足登录要求。
	gatewayReq := withGatewaySource(httptest.NewRequest(http.MethodGet, RoutePrefix+"/business", nil))
	gatewayReq.Header = gatewayHeaders("1002", "alice", "false")
	gatewayRec := httptest.NewRecorder()
	router.ServeHTTP(gatewayRec, gatewayReq)
	if gatewayRec.Code != http.StatusOK {
		t.Fatalf("business status with gateway identity = %d, want %d", gatewayRec.Code, http.StatusOK)
	}
}

func TestAdminOnlyConfigRequiresAdminIdentity(t *testing.T) {
	router := newRouteGroupsRouter(t, StartOptions{AdminOnlyConfig: true, GatewayAuth: true}, core.WebAuthSettings{})

	userReq := withGatewaySource(httptest.NewRequest(http.MethodGet, RoutePrefix+"/config", nil))
	userReq.Header = gatewayHeaders("1002", "alice", "false")
	userRec := httptest.NewRecorder()
	router.ServeHTTP(userRec, userReq)
	if userRec.Code != http.StatusForbidden {
		t.Fatalf("config status as non-admin = %d, want %d", userRec.Code, http.StatusForbidden)
	}

	adminReq := withGatewaySource(httptest.NewRequest(http.MethodGet, RoutePrefix+"/config", nil))
	adminReq.Header = gatewayHeaders("1000", "admin", "true")
	adminRec := httptest.NewRecorder()
	router.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusOK {
		t.Fatalf("config status as admin = %d, want %d", adminRec.Code, http.StatusOK)
	}
}

func TestAdminOnlyConfigAcceptsLocalSession(t *testing.T) {
	settings := core.WebAuthSettings{
		Username:      "owner",
		PasswordHash:  "hash",
		SessionSecret: "secret",
	}
	value, err := createSessionValue(settings, time.Now())
	if err != nil {
		t.Fatalf("create session value: %v", err)
	}

	// 内置账号即管理员，开启 AdminOnlyConfig 后仍应可以访问系统配置。
	router := newRouteGroupsRouter(t, StartOptions{AdminOnlyConfig: true}, settings)
	req := httptest.NewRequest(http.MethodGet, RoutePrefix+"/config", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: value})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("config status with local session = %d, want %d", rec.Code, http.StatusOK)
	}
}

// newAppEngine 构建完整的生产路由表（不监听端口），用于校验真实路由的鉴权行为。
// 账号配置用桩函数提供，避免测试触碰真实配置库。
func newAppEngine(t *testing.T, opts StartOptions) *gin.Engine {
	t.Helper()
	previous := RoutePrefix
	RoutePrefix = DefaultRoutePrefix
	t.Cleanup(func() { RoutePrefix = previous })
	return newEngineWithProvider(opts, func() (core.WebAuthSettings, error) {
		return core.WebAuthSettings{}, nil
	})
}

func doRequest(engine *gin.Engine, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestQrLoginRoutesRequireConfigAuth(t *testing.T) {
	engine := newAppEngine(t, StartOptions{})

	// 扫码登录会写入平台 Cookie，按 README 说明属于系统配置操作，必须登录。
	requests := []*http.Request{
		httptest.NewRequest(http.MethodPost, RoutePrefix+"/qr_login/netease", nil),
		httptest.NewRequest(http.MethodGet, RoutePrefix+"/qr_login/netease?key=abc", nil),
	}
	for _, req := range requests {
		req.Header.Set("Accept", "application/json")
		rec := doRequest(engine, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want %d", req.Method, req.URL.Path, rec.Code, http.StatusUnauthorized)
		}
	}
}

func TestRealBusinessRoutesRespectRequireLogin(t *testing.T) {
	// 默认：业务路由公开（与 README 描述一致），直接落到处理器（静态文件不存在返回 404）。
	publicEngine := newAppEngine(t, StartOptions{})
	publicRec := doRequest(publicEngine, httptest.NewRequest(http.MethodGet, RoutePrefix+"/videos/missing.mp4", nil))
	if publicRec.Code != http.StatusNotFound {
		t.Fatalf("public business route status = %d, want %d", publicRec.Code, http.StatusNotFound)
	}

	// 开启 --require-login 后：无身份被拒。
	engine := newAppEngine(t, StartOptions{RequireLogin: true, GatewayAuth: true})
	anonReq := httptest.NewRequest(http.MethodGet, RoutePrefix+"/videos/missing.mp4", nil)
	anonReq.Header.Set("Accept", "application/json")
	anonRec := doRequest(engine, anonReq)
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("business route without identity = %d, want %d", anonRec.Code, http.StatusUnauthorized)
	}

	// 带飞牛网关身份：放行到处理器。
	gatewayReq := withGatewaySource(httptest.NewRequest(http.MethodGet, RoutePrefix+"/videos/missing.mp4", nil))
	gatewayReq.Header = gatewayHeaders("1002", "alice", "false")
	gatewayRec := doRequest(engine, gatewayReq)
	if gatewayRec.Code != http.StatusNotFound {
		t.Fatalf("business route with gateway identity = %d, want %d", gatewayRec.Code, http.StatusNotFound)
	}
}

func TestRealConfigRoutesRequireLoginByDefault(t *testing.T) {
	engine := newAppEngine(t, StartOptions{GatewayAuth: true})

	anonReq := httptest.NewRequest(http.MethodGet, RoutePrefix+"/cookies", nil)
	anonReq.Header.Set("Accept", "application/json")
	if rec := doRequest(engine, anonReq); rec.Code != http.StatusUnauthorized {
		t.Fatalf("cookies without identity = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// 系统配置路由不受 --require-login 影响，网关身份依旧可以直接通过。
	gatewayReq := withGatewaySource(httptest.NewRequest(http.MethodGet, RoutePrefix+"/cookies", nil))
	gatewayReq.Header = gatewayHeaders("1000", "admin", "true")
	if rec := doRequest(engine, gatewayReq); rec.Code != http.StatusOK {
		t.Fatalf("cookies with gateway identity = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestUnixSocketFileMode(t *testing.T) {
	tests := []struct {
		name    string
		opts    StartOptions
		want    os.FileMode
		wantErr bool
	}{
		{name: "gateway auth defaults to 0666", opts: StartOptions{GatewayAuth: true}, want: 0o666},
		{name: "no gateway auth keeps system default", opts: StartOptions{}, want: 0},
		{name: "explicit mode wins", opts: StartOptions{UnixSocketMode: "0660", GatewayAuth: true}, want: 0o660},
		{name: "explicit mode without gateway auth", opts: StartOptions{UnixSocketMode: "0600"}, want: 0o600},
		{name: "invalid mode", opts: StartOptions{UnixSocketMode: "8x8"}, wantErr: true},
		{name: "out of range mode", opts: StartOptions{UnixSocketMode: "1000"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := unixSocketFileMode(tt.opts)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("mode = %#o, want %#o", got, tt.want)
			}
		})
	}
}
