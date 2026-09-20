# 飞牛 fnOS 应用包（fpk）

把 go-music-dl 打包成飞牛 fnOS 应用，通过 **统一网关** 复用 NAS 登录态：
用户从桌面点开应用时，fnOS 先校验 NAS 会话，再把当前用户身份
（`X-Trim-Userid` / `X-Trim-Username` / `X-Trim-Isadmin`）转发给 go-music-dl，
因此不需要在应用内再登录一次。

## 相关代码

- `internal/web/gateway.go`：解析并信任网关注入的用户身份。
- `internal/web/auth.go`：网关身份优先于内置管理员会话；网关已登录时 `/login`、`/setup` 直接跳回应用。
- `internal/web/server.go`：`--unix-socket` 监听与连接来源标记（只信任 Unix Socket 上的身份头）。
- `cmd/music-dl/web.go`：`--unix-socket`、`--gateway-auth` 参数。

## 构建

1. 下载 `fnpack` 并加入 `PATH`：<https://developer.fnnas.com/docs/cli/fnpack>
2. 执行构建脚本：

   ```bash
   packaging/fnos/build.sh
   ```

   产物位于 `dist/fnos/`，例如 `go-music-dl-1.1.1-x86.fpk`、`go-music-dl-1.1.1-arm.fpk`。

## 安装

1. 应用中心 → 手动安装 → 选择对应的 `.fpk`；或在 NAS 上执行
   `appcenter-cli install-fpk go-music-dl-1.1.1-x86.fpk`。
2. 安装完成后从桌面打开「音乐下载」。入口走 `/app/go-music-dl`，由 fnOS 校验登录态。

## 运行参数

`cmd/main` 启动应用时使用：

```bash
music-dl web --no-browser \
  --unix-socket "${TRIM_APPDEST}/go-music-dl.sock" \
  --gateway-auth \
  --require-login \
  --base-path /app/go-music-dl
```

只指定 `--unix-socket` 时不再监听 TCP 端口；如需局域网直接访问，显式加上 `--port 8080`，
此时 TCP 请求不会信任任何身份头，并且由于启用了 `--require-login`，所有业务功能都要求
先登录内置管理员账号。

## 鉴权参数

| 参数 | 作用 |
| --- | --- |
| `--gateway-auth` | 信任统一网关注入的身份头，且只信任 Unix Socket 上的请求 |
| `--require-login` | 搜索、下载等业务路由也要求登录（不开启时按项目原有行为公开） |
| `--admin-only-config` | 系统配置类操作要求管理员身份，飞牛网关下按 `X-Trim-Isadmin` 判断 |
| `--unix-socket-mode` | Unix Socket 文件权限，默认在 `--gateway-auth` 下为 `0666`，可收紧为 `0660`/`0600` |

## 数据与下载目录

- 配置库：`${TRIM_PKGVAR}/settings.db`（应用数据目录，随存储空间持久化）。
- 下载目录：默认 `${TRIM_PKGVAR}/data/downloads`，可在应用「设置 → 下载目录」改成
  飞牛共享目录（如 `/vol1/.../go-music-dl/downloads`）。
- 应用包声明了共享目录 `go-music-dl/downloads`，可以在文件管理器中直接查看。
  若要让应用写入其它共享目录，可在应用中心的「授权目录」中把目录授权给本应用。

## 安全说明

`X-Trim-*` 请求头只有在请求确实来自统一网关时才可信。应用按连接类型判断：
仅 Unix Socket 连接上的身份头会被接受，TCP 端口上的同名头一律忽略，
所以不能在局域网里伪造 NAS 用户身份。
