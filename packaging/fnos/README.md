# 飞牛 fnOS 应用包（fpk）

把 go-music-dl 打包成飞牛 fnOS 应用，桌面入口通过 **统一网关** 复用 NAS 登录态：
用户从桌面点开应用时，fnOS 先校验 NAS 会话，再把当前用户身份
（`X-Trim-Userid` / `X-Trim-Username` / `X-Trim-Isadmin`）转发给 go-music-dl，
因此不需要在应用内再登录一次。

同时应用会监听一个 **直连 TCP 端口**，供安卓客户端等无法携带 NAS 会话的场景使用。

## 相关代码

- `internal/web/gateway.go`：解析并信任网关注入的用户身份。
- `internal/web/auth.go`：网关身份优先于内置管理员会话；网关已登录时 `/login`、`/setup` 直接跳回应用。
- `internal/web/server.go`：`--unix-socket` / `--port` 监听与连接来源标记（只信任 Unix Socket 上的身份头）。
- `cmd/music-dl/web.go`：`--unix-socket`、`--gateway-auth`、`--require-login` 等参数。
- `packaging/fnos/go-music-dl/cmd/main`：启动脚本，负责解析直连端口。

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
  --base-path /app/go-music-dl \
  --port "$(cat ${TRIM_PKGVAR}/web-port.conf)"
```

`--port` 只在直连端口启用时才附加（见下节）。`--base-path` 对两条访问路径都生效。

## 直连端口（安卓客户端等）

**为什么需要它**：统一网关要求 NAS 登录会话，而安卓客户端（例如飞牛音乐第三方
客户端）手里只有飞牛音乐的 `music-token`，拿不到 NAS 会话，因此无法走
`/app/go-music-dl`。给应用加一个直连 TCP 端口后，客户端可以直接登录内置管理员账号。

**端口配置**：`${TRIM_PKGVAR}/web-port.conf`

| 文件内容 | 行为 |
| --- | --- |
| 端口号（如 `18080`） | 同时监听 Unix Socket 与 `0.0.0.0:<端口>` |
| 缺失 | 首次启动自动写入 `18080` |
| `0` / `off` / `none` / 空 | 只监听 Unix Socket，仅统一网关可达 |

首次启动会自动创建该文件，因此不需要事先手工准备。改完在应用中心「重启」应用，
或执行 `cmd/main stop && cmd/main start` 生效。

客户端填写的地址是：

```text
http://<NAS_IP>:18080/app/go-music-dl
```

注意必须带上 `/app/go-music-dl`，因为 `--base-path` 对直连端口同样生效。

**登录**：直连端口上的请求不会信任任何身份头（`X-Trim-*` 一律忽略），并且由于
启用了 `--require-login`，所有业务接口都要求内置管理员登录。客户端应先调用
`POST /app/go-music-dl/login`（表单字段 `username` / `password`，默认用户名
`admin`）拿到 `music_dl_session` Cookie，后续请求带上它。

**外网访问**：不要把该端口直接裸露到公网。需要远程使用时，请通过 VPN
（WireGuard / Tailscale）或带 HTTPS 与访问控制的反向代理接入。

## 鉴权参数

| 参数 | 作用 |
| --- | --- |
| `--gateway-auth` | 信任统一网关注入的身份头，且只信任 Unix Socket 上的请求 |
| `--require-login` | 搜索、下载等业务路由也要求登录（不开启时按项目原有行为公开） |
| `--admin-only-config` | 系统配置类操作要求管理员身份，飞牛网关下按 `X-Trim-Isadmin` 判断 |
| `--unix-socket-mode` | Unix Socket 文件权限，默认在 `--gateway-auth` 下为 `0666`，可收紧为 `0660`/`0600` |

> 直连端口启用后，系统配置类接口（平台 Cookie、系统设置）也会暴露在该端口上。
> 建议加上 `--admin-only-config`，让这些操作额外要求管理员身份。

## 数据与下载目录

- 配置库：`${TRIM_PKGVAR}/settings.db`（应用数据目录，随存储空间持久化）。
- 下载目录：默认 `${TRIM_PKGVAR}/data/downloads`，可在应用「设置 → 下载目录」改成
  飞牛共享目录（如 `/vol1/.../go-music-dl/downloads`）。
- 应用包声明了共享目录 `go-music-dl/downloads`，可以在文件管理器中直接查看。
  若要让应用写入其它共享目录，可在应用中心的「授权目录」中把目录授权给本应用。

### 要让下载的歌出现在「飞牛音乐」里

1. 把下载目录改成**飞牛音乐的曲库目录**（或其子目录），否则新文件不会被收录。
   曲库根目录可在 `GET /music/api/v1/shared-library/list` 的 `data.list[].path` 查看。
2. 在应用「设置」里打开「**下载时内嵌元数据（封面/歌词）**」。飞牛音乐的扫描器
   只识别音频文件**内嵌**的标签，外挂 `cover.jpg` 不生效；不开这个开关，扫进去的
   歌曲会没有封面和歌手。

## 安全说明

`X-Trim-*` 请求头只有在请求确实来自统一网关时才可信。应用按连接类型判断：
仅 Unix Socket 连接上的身份头会被接受，TCP 端口上的同名头一律忽略，
所以不能在局域网里伪造 NAS 用户身份。

直连端口上所有业务接口都要求登录（`--require-login`），因此不再存在
「无鉴权即可向 NAS 磁盘写入文件」的问题；但仍请使用强密码，并优先通过 VPN
或带访问控制的 HTTPS 反向代理接入。
