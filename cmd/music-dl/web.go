package main

import (
	"strings"

	"github.com/guohuiyuan/go-music-dl/internal/web"
	"github.com/spf13/cobra"
)

var port string
var noBrowser bool
var desktopMode bool
var basePath string
var unixSocket string
var gatewayAuth bool
var unixSocketMode string
var requireLogin bool
var adminOnlyConfig bool

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "启动 Web 服务模式",
	Run: func(cmd *cobra.Command, args []string) {
		if desktopMode {
			web.StartDesktop(port)
			return
		}
		// 使用 Unix Socket 时默认只监听 Socket（由飞牛 fnOS 统一网关暴露给用户），
		// 只有在显式指定 --port 时才同时保留 TCP 端口。
		socketOnly := strings.TrimSpace(unixSocket) != "" && !cmd.Flags().Changed("port")
		web.StartWithOptions(port, web.StartOptions{
			ShouldOpenBrowser: !noBrowser && !socketOnly,
			BasePath:          basePath,
			GatewayAuth:       gatewayAuth,
			ListenUnixSocket:  unixSocket,
			UnixSocketMode:    unixSocketMode,
			DisableTCP:        socketOnly,
			RequireLogin:      requireLogin,
			AdminOnlyConfig:   adminOnlyConfig,
		})
	},
}

func init() {
	webCmd.Flags().StringVarP(&port, "port", "p", "8080", "服务端口")
	webCmd.Flags().StringVar(&basePath, "base-path", web.DefaultRoutePrefix, "Web 端基础路径")
	webCmd.Flags().BoolVar(&noBrowser, "no-browser", false, "不自动打开浏览器")
	webCmd.Flags().StringVar(&unixSocket, "unix-socket", "", "改为在指定 Unix Socket 上提供服务（飞牛 fnOS 统一网关）")
	webCmd.Flags().BoolVar(&gatewayAuth, "gateway-auth", false, "信任飞牛 fnOS 统一网关注入的用户身份头（X-Trim-Userid 等）")
	webCmd.Flags().StringVar(&unixSocketMode, "unix-socket-mode", "", "Unix Socket 文件权限，八进制，例如 0660；默认在 --gateway-auth 下为 0666")
	webCmd.Flags().BoolVar(&requireLogin, "require-login", false, "搜索、下载等业务路由也要求登录（默认只有系统配置需要登录）")
	webCmd.Flags().BoolVar(&adminOnlyConfig, "admin-only-config", false, "系统配置类操作要求管理员身份（飞牛网关下按 X-Trim-Isadmin 判断）")
	webCmd.Flags().BoolVar(&desktopMode, "desktop", false, "桌面内嵌模式")
	_ = webCmd.Flags().MarkHidden("desktop")
	rootCmd.AddCommand(webCmd)
}
