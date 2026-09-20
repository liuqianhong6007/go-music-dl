package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/guohuiyuan/go-music-dl/core"
	"github.com/guohuiyuan/music-lib/model"
)

//go:embed templates/*
var templateFS embed.FS

// DefaultRoutePrefix is the web UI base path used when no base path is configured.
const DefaultRoutePrefix = "/music"

var RoutePrefix = DefaultRoutePrefix

type importCollectionMeta struct {
	Enabled     bool
	Name        string
	Description string
	Cover       string
	Creator     string
	TrackCount  int
	Source      string
	ExternalID  string
	Link        string
	ContentType string
	HoverText   string
}

func defaultSourcesForSearchType(searchType string) []string {
	switch searchType {
	case "playlist":
		return core.GetPlaylistSourceNames()
	case "album":
		return core.GetAlbumSourceNames()
	default:
		return core.GetDefaultSourceNames()
	}
}

func collectionLabelForSearchType(searchType string) string {
	if searchType == "album" {
		return "专辑"
	}
	return "歌单"
}

func collectionCreatorLabelForSearchType(searchType string) string {
	if searchType == "album" {
		return "歌手"
	}
	return "创建者"
}

func searchPlaceholderForType(searchType string) string {
	switch searchType {
	case "playlist":
		return "搜索歌单、创建者，或直接粘贴歌单链接"
	case "album":
		return "搜索专辑、歌手，或直接粘贴专辑链接"
	default:
		return "搜索歌曲、歌手，或直接粘贴分享链接"
	}
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		method := c.Request.Method
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE, UPDATE")
		c.Header("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept, Authorization")
		c.Header("Access-Control-Expose-Headers", "Content-Length, Access-Control-Allow-Origin, Access-Control-Allow-Headers, Cache-Control, Content-Language, Content-Type")
		c.Header("Access-Control-Allow-Credentials", "true")
		if method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
		}
		c.Next()
	}
}

func setDownloadHeader(c *gin.Context, filename string) {
	filename = strings.ReplaceAll(strings.TrimSpace(filename), "\\", "/")
	if slash := strings.LastIndex(filename, "/"); slash >= 0 {
		filename = strings.TrimSpace(filename[slash+1:])
	}
	if filename == "" {
		filename = "download"
	}
	encoded := url.PathEscape(filename)
	fallback := asciiDownloadFilenameFallback(filename)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"; filename*=UTF-8''%s", fallback, encoded))
}

func publicWebSettings() core.WebSettings {
	settings := core.GetWebSettings()
	settings.WebDAVPassword = ""
	return settings
}

func asciiDownloadFilenameFallback(filename string) string {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return "download"
	}

	base := filename
	ext := ""
	if dot := strings.LastIndex(filename, "."); dot > 0 && dot < len(filename)-1 {
		candidateExt := filename[dot:]
		if candidateExt == asciiDownloadFilenamePart(candidateExt) {
			base = filename[:dot]
			ext = candidateExt
		}
	}

	fallback := strings.Trim(asciiDownloadFilenamePart(base), " .-_")
	if fallback == "" {
		fallback = "download"
	}
	return fallback + ext
}

func asciiDownloadFilenamePart(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_', r == ' ':
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func playlistExtraValue(playlist model.Playlist, key string) string {
	if playlist.Extra == nil {
		return ""
	}
	return strings.TrimSpace(playlist.Extra[key])
}

func importCollectionHoverText(contentType string) string {
	if contentType == collectionContentAlbum {
		return "导入到本地歌单列表，保存为外部导入专辑；仅保存元数据，不保存具体歌曲明细。"
	}
	return "导入到本地歌单列表，保存为外部导入歌单；仅保存元数据，不保存具体歌曲明细。"
}

func playlistDetailURL(root string, searchType string, playlist model.Playlist) string {
	if strings.TrimSpace(playlist.Source) == "local" {
		return fmt.Sprintf("%s/collection?id=%s", root, url.QueryEscape(playlist.ID))
	}
	if playlistExtraValue(playlist, "external_only") == "true" && strings.TrimSpace(playlist.Link) != "" {
		return playlist.Link
	}

	route := "playlist"
	contentType := collectionContentPlaylist
	if searchType == collectionContentAlbum {
		route = "album"
		contentType = collectionContentAlbum
	}

	values := url.Values{}
	values.Set("id", playlist.ID)
	values.Set("source", playlist.Source)
	if name := strings.TrimSpace(playlist.Name); name != "" {
		values.Set("name", name)
	}
	if description := strings.TrimSpace(playlist.Description); description != "" {
		values.Set("description", description)
	}
	if cover := strings.TrimSpace(playlist.Cover); cover != "" {
		values.Set("cover", cover)
	}
	if creator := strings.TrimSpace(playlist.Creator); creator != "" {
		values.Set("creator", creator)
	}
	if playlist.TrackCount > 0 {
		values.Set("track_count", strconv.Itoa(playlist.TrackCount))
	}
	link := strings.TrimSpace(playlist.Link)
	if link == "" {
		link = core.GetOriginalLink(playlist.Source, playlist.ID, contentType)
	}
	if link != "" {
		values.Set("link", link)
	}

	return fmt.Sprintf("%s/%s?%s", root, route, values.Encode())
}

func isDetailPagePath(requestPath string) bool {
	requestPath = strings.TrimRight(strings.TrimSpace(requestPath), "/")
	return strings.HasSuffix(requestPath, "/playlist") ||
		strings.HasSuffix(requestPath, "/album") ||
		strings.HasSuffix(requestPath, "/collection")
}

func renderIndex(c *gin.Context, songs []model.Song, playlists []model.Playlist, q string, selected []string, errMsg string, searchType string, playlistLink string, colID string, colName string, isLocalColPage bool, collectionKind string, importCollection *importCollectionMeta) {
	allSrc := core.GetAllSourceNames()
	desc := make(map[string]string)
	for _, s := range allSrc {
		desc[s] = core.GetSourceDescription(s)
	}

	playlistSupported := make(map[string]bool)
	for _, s := range core.GetPlaylistSourceNames() {
		playlistSupported[s] = true
	}
	// 本地音乐支持歌单搜索（搜本地自建/导入歌单），但不进 GetPlaylistSourceNames
	// 以免成为默认勾选项——这里仅开启复选框在歌单模式下可用。
	playlistSupported["local"] = true
	albumSupported := make(map[string]bool)
	for _, s := range core.GetAlbumSourceNames() {
		albumSupported[s] = true
	}
	playlistCategorySupported := make(map[string]bool)
	for _, s := range core.GetPlaylistCategorySourceNames() {
		playlistCategorySupported[s] = true
	}
	qrLoginSupported := make(map[string]bool)
	for _, s := range core.GetQRLoginSourceNames() {
		qrLoginSupported[s] = true
	}
	userPlaylistSupported := make(map[string]bool)
	for _, s := range core.GetUserPlaylistSourceNames() {
		userPlaylistSupported[s] = true
	}

	playlistCategorySources, _ := c.Get("PlaylistCategorySources")
	playlistCategoryCurrent, _ := c.Get("PlaylistCategoryCurrent")
	playlistSourceTabs, _ := c.Get("PlaylistSourceTabs")

	settings := core.GetWebSettings()
	defaultPageSize := settings.WebPageSize
	if defaultPageSize <= 0 {
		defaultPageSize = core.DefaultWebPageSize
	}
	pageSize := defaultPageSize
	if raw := strings.TrimSpace(c.Query("page_size")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			pageSize = n
		}
	}
	if pageSize > 500 {
		pageSize = 500
	}

	page := 1
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			page = n
		}
	}

	totalCount := 0
	if len(songs) > 0 {
		totalCount = len(songs)
	} else if len(playlists) > 0 {
		totalCount = len(playlists)
	}

	totalPages := 1
	pageStart := 0
	pageEnd := totalCount
	if totalCount > 0 {
		totalPages = (totalCount + pageSize - 1) / pageSize
		if page > totalPages {
			page = totalPages
		}
		pageStart = (page - 1) * pageSize
		if pageStart < 0 {
			pageStart = 0
		}
		pageEnd = pageStart + pageSize
		if pageEnd > totalCount {
			pageEnd = totalCount
		}

		if len(songs) > 0 {
			songs = songs[pageStart:pageEnd]
		}
		if len(playlists) > 0 {
			playlists = playlists[pageStart:pageEnd]
		}
	}

	pageStartDisplay := 0
	if totalCount > 0 {
		pageStartDisplay = pageStart + 1
	}
	c.HTML(200, "index.html", gin.H{
		"Result":                  songs,
		"Playlists":               playlists,
		"Page":                    page,
		"PageSize":                pageSize,
		"TotalCount":              totalCount,
		"TotalPages":              totalPages,
		"PageStart":               pageStartDisplay,
		"PageEnd":                 pageEnd,
		"Keyword":                 q,
		"AllSources":              allSrc,
		"DefaultSources":          defaultSourcesForSearchType(searchType),
		"SourceDescriptions":      desc,
		"Selected":                selected,
		"Error":                   errMsg,
		"SearchType":              searchType,
		"ShowDetailBack":          isDetailPagePath(c.Request.URL.Path),
		"PlaylistSupported":       playlistSupported,
		"AlbumSupported":          albumSupported,
		"CategorySupported":       playlistCategorySupported,
		"QRLoginSupported":        qrLoginSupported,
		"SearchPlaceholder":       searchPlaceholderForType(searchType),
		"CollectionLabel":         collectionLabelForSearchType(searchType),
		"CollectionCreator":       collectionCreatorLabelForSearchType(searchType),
		"Root":                    RoutePrefix,
		"PlaylistLink":            playlistLink,
		"ColID":                   colID,
		"ColName":                 colName,
		"CollectionKind":          collectionKind,
		"ImportCollection":        importCollection,
		"CanRemoveSongs":          colID != "" && collectionKind == collectionKindManual,
		"IsLocalColPage":          isLocalColPage,
		"PlaylistCategorySources": playlistCategorySources,
		"PlaylistCategoryCurrent": playlistCategoryCurrent,
		"PlaylistSourceTabs":      playlistSourceTabs,
		"UserPlaylistSupported":   userPlaylistSupported,
	})
}

func NormalizeBasePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimRight(p, "/")
	if p == "" {
		return DefaultRoutePrefix
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

type StartOptions struct {
	ShouldOpenBrowser bool
	DisableAuth       bool
	ListenHost        string
	BasePath          string
	// GatewayAuth 为 true 时，来自 ListenUnixSocket 的请求若带有飞牛 fnOS
	// 统一网关注入的用户身份头，则直接视为已登录。
	GatewayAuth bool
	// ListenUnixSocket 非空时在该 Unix Socket 上提供服务，供飞牛 fnOS 统一网关转发。
	ListenUnixSocket string
	// DisableTCP 为 true 时不监听 TCP 端口（仅使用 Unix Socket）。
	DisableTCP bool
	// RequireLogin 为 true 时，搜索、下载等业务路由同样要求登录。
	// 默认保持项目原有行为：普通功能公开，只有系统配置类操作需要登录。
	RequireLogin bool
	// AdminOnlyConfig 为 true 时，系统配置类操作还要求管理员身份。
	AdminOnlyConfig bool
	// UnixSocketMode 设置 Unix Socket 文件权限，取八进制字符串，例如 "0660"。
	// 留空时：启用 GatewayAuth 用 0666（飞牛 fnOS 网关可能以其他用户连接），
	// 否则保持系统默认权限。
	UnixSocketMode string
}

func Start(port string, shouldOpenBrowser bool, basePath string) {
	StartWithOptions(port, StartOptions{ShouldOpenBrowser: shouldOpenBrowser, BasePath: basePath})
}

func StartDesktop(port string) {
	StartWithOptions(port, StartOptions{
		DisableAuth: true,
		ListenHost:  "127.0.0.1",
	})
}

func StartWithOptions(port string, opts StartOptions) {
	RoutePrefix = NormalizeBasePath(opts.BasePath)
	core.CM.Load()
	if !opts.DisableAuth {
		settings, err := core.GetWebAuthSettings()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to read web auth settings: %v\n", err)
		} else if token, tokenErr := prepareSetupToken(settings); tokenErr == nil && token != "" {
			fmt.Printf("Web setup token: %s\nOpen %s/setup and keep this startup terminal private until setup is complete.\n", token, RoutePrefix)
		}
	}
	InitDB()
	defer CloseDB()
	syncLocalMusicIndexAsync()

	r := newEngine(opts)

	server := &http.Server{Handler: r, ConnContext: withGatewayConn}
	listeners := make([]net.Listener, 0, 2)

	socketPath := strings.TrimSpace(opts.ListenUnixSocket)
	if socketPath != "" {
		// 上次异常退出可能残留 socket 文件，先清掉避免 listen 报 address already in use。
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "Failed to clean up unix socket %s: %v\n", socketPath, err)
			return
		}
		if dir := filepath.Dir(socketPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to create unix socket directory %s: %v\n", dir, err)
				return
			}
		}
		unixListener, err := net.Listen("unix", socketPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start web server on unix socket %s: %v\n", socketPath, err)
			return
		}
		socketMode, modeErr := unixSocketFileMode(opts)
		if modeErr != nil {
			fmt.Fprintf(os.Stderr, "Invalid unix socket mode %q: %v\n", opts.UnixSocketMode, modeErr)
			return
		}
		if socketMode != 0 {
			// 该 socket 位于应用私有目录，只供本机飞牛 fnOS 网关转发使用。
			// 网关进程可能以其他用户身份连接，因此默认放开连接权限；
			// 可用 --unix-socket-mode 收紧到 0660 / 0600。
			if err := os.Chmod(socketPath, socketMode); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to adjust unix socket permission: %v\n", err)
			}
		}
		listeners = append(listeners, unixListener)
	}

	if !opts.DisableTCP {
		listenAddr := opts.ListenHost + ":" + port
		listener, err := net.Listen("tcp", listenAddr)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "address already in use") {
				fmt.Fprintf(os.Stderr, "Failed to start web server: port %s is already in use. Please use --port to specify another port, e.g. music-dl web --port 8081\n", port)
				return
			}
			fmt.Fprintf(os.Stderr, "Failed to start web server on %s: %v\n", listenAddr, err)
			return
		}
		listeners = append(listeners, listener)

		urlHost := opts.ListenHost
		if urlHost == "" || urlHost == "0.0.0.0" || urlHost == "::" {
			urlHost = "localhost"
		}
		urlStr := "http://" + urlHost + ":" + port + RoutePrefix
		fmt.Printf("Web started at %s\n", urlStr)
		if opts.ShouldOpenBrowser {
			go func() { time.Sleep(500 * time.Millisecond); core.OpenBrowser(urlStr) }()
		}
	}

	if socketPath != "" {
		fmt.Printf("fnOS gateway socket: %s (base path %s)\n", socketPath, RoutePrefix)
	}
	if len(listeners) == 0 {
		fmt.Fprintln(os.Stderr, "Failed to start web server: no listener enabled")
		return
	}

	errCh := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func(l net.Listener) { errCh <- server.Serve(l) }(listener)
	}
	for range listeners {
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "Web server stopped with error: %v\n", err)
		}
	}
}

// newEngine 构建完整的 Web 路由。抽成独立函数便于在测试里直接校验路由鉴权，
// 无需真正监听端口。调用方需自行完成数据库初始化等准备工作。
func newEngine(opts StartOptions) *gin.Engine {
	return newEngineWithProvider(opts, core.GetWebAuthSettings)
}

func newEngineWithProvider(opts StartOptions, provider authSettingsProvider) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	r.Use(corsMiddleware())

	tmpl := template.Must(template.New("").Funcs(template.FuncMap{
		"artistTokens":       splitArtistTokens,
		"albumID":            songAlbumID,
		"playlistDetailURL":  playlistDetailURL,
		"playlistExtraValue": playlistExtraValue,
		"tojson": func(v interface{}) string {
			if v == nil {
				return ""
			}
			b, err := json.Marshal(v)
			if err != nil {
				return ""
			}
			return string(b)
		},
	}).ParseFS(templateFS,
		"templates/pages/*.html",
		"templates/partials/*.html",
	))
	r.SetHTMLTemplate(tmpl)

	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, RoutePrefix)
	})

	videoDir := "data/video_output"
	os.MkdirAll(videoDir, 0755)

	api := r.Group(RoutePrefix)

	api.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"app":    "go-music-dl",
			"status": "ok",
		})
	})

	// Static assets embedded at build time.
	api.GET("/icon.png", func(c *gin.Context) { c.FileFromFS("templates/static/images/icon.png", http.FS(templateFS)) })
	api.GET("/style.css", func(c *gin.Context) { c.FileFromFS("templates/static/css/style.css", http.FS(templateFS)) })
	api.GET("/videogen.css", func(c *gin.Context) { c.FileFromFS("templates/static/css/videogen.css", http.FS(templateFS)) })
	api.GET("/videogen.js", func(c *gin.Context) { c.FileFromFS("templates/static/js/videogen.js", http.FS(templateFS)) })
	api.GET("/app.js", func(c *gin.Context) { c.FileFromFS("templates/static/js/app.js", http.FS(templateFS)) })
	// 路由分为三层：公开资源、需要登录的业务路由、系统配置路由。
	// 默认只有系统配置需要登录（与 README 描述一致）；--require-login 会把业务路由
	// 也纳入登录校验，飞牛 fnOS 网关身份可以直接满足该要求。
	routes := bindAuthMiddlewareWithProvider(api, opts, provider)
	routes.protected.Static("/videos", videoDir)

	routes.protected.GET("/render", func(c *gin.Context) {
		c.HTML(200, "render.html", gin.H{
			"Root": RoutePrefix,
		})
	})

	routes.config.HEAD("/cookies", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	routes.config.GET("/cookies", func(c *gin.Context) { c.JSON(200, core.CM.GetAll()) })
	routes.config.POST("/cookies", func(c *gin.Context) {
		var req map[string]string
		if err := c.ShouldBindJSON(&req); err == nil {
			core.CM.SetAll(req)
			core.CM.Save()
			c.JSON(200, gin.H{"status": "ok"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cookies payload"})
	})

	api.GET("/settings", func(c *gin.Context) {
		c.JSON(200, publicWebSettings())
	})
	routes.config.POST("/settings", func(c *gin.Context) {
		var req core.WebSettings
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid settings payload"})
			return
		}
		if err := core.SaveWebSettings(req); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, publicWebSettings())
	})

	RegisterMusicRoutes(routes.protected, routes.config)
	// 扫码登录会写入平台 Cookie，属于系统配置操作，按 README 的说明需要管理员登录。
	RegisterQRLoginRoutes(routes.config)
	RegisterCollectionRoutes(routes.protected)
	RegisterLocalMusicRoutes(routes.protected)
	RegisterVideogenRoutes(routes.protected, videoDir)
	RegisterUpdateRoutes(routes.protected)

	return r
}

// appRouteGroups 把应用路由分成三层：
//   - public：无需登录，例如静态资源、健康检查和登录页。
//   - protected：搜索、下载等业务路由。默认公开（与项目原有行为一致），
//     开启 StartOptions.RequireLogin 后要求登录。
//   - config：系统配置类操作（Cookie、系统设置、扫码登录写入 Cookie）。
//     始终要求登录，开启 StartOptions.AdminOnlyConfig 后额外要求管理员身份。
type appRouteGroups struct {
	public    *gin.RouterGroup
	protected *gin.RouterGroup
	config    *gin.RouterGroup
}

func bindAuthMiddleware(api *gin.RouterGroup, opts StartOptions) appRouteGroups {
	return bindAuthMiddlewareWithProvider(api, opts, core.GetWebAuthSettings)
}

func bindAuthMiddlewareWithProvider(api *gin.RouterGroup, opts StartOptions, provider authSettingsProvider) appRouteGroups {
	bindAuthRoutes(api, opts.GatewayAuth)
	if opts.DisableAuth {
		return appRouteGroups{public: api, protected: api, config: api}
	}

	protected := api
	if opts.RequireLogin {
		protected = api.Group("")
		protected.Use(authRequired(provider, routeAuthOptions{gatewayAuth: opts.GatewayAuth}))
	}

	// 系统配置类操作始终需要登录，这一点不受 RequireLogin 影响。
	config := api.Group("")
	config.Use(authRequired(provider, routeAuthOptions{
		gatewayAuth: opts.GatewayAuth,
		adminOnly:   opts.AdminOnlyConfig,
	}))

	return appRouteGroups{public: api, protected: protected, config: config}
}

// unixSocketFileMode 解析 Unix Socket 权限。返回 0 表示保持系统默认权限。
func unixSocketFileMode(opts StartOptions) (os.FileMode, error) {
	mode := strings.TrimSpace(opts.UnixSocketMode)
	if mode == "" {
		if opts.GatewayAuth {
			// 飞牛 fnOS 网关可能以其他用户身份连接 socket，默认放开连接权限。
			return 0o666, nil
		}
		return 0, nil
	}
	parsed, err := strconv.ParseUint(strings.TrimPrefix(mode, "0o"), 8, 32)
	if err != nil {
		return 0, err
	}
	if parsed > 0o777 {
		return 0, fmt.Errorf("mode %s is out of range", mode)
	}
	return os.FileMode(parsed), nil
}
