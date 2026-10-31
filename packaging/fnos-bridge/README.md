# go-music-dl fnOS Music Bridge

独立的飞牛音乐增强应用包。它只运行 `fnos-bridge`，不会启动或替代
go-music-dl Web 应用，也不需要另外安装 systemd 服务。

进程边界：

```text
go-music-dl FPK
  music-dl web --unix-socket ← Web 自动包含 source API
          ↑ go-music-dl.sock + 共享令牌
fnos-bridge FPK
  fnos-bridge          ← 飞牛协议、GUID、Socket 接管、请求透传
```

`cmd/fnos-bridge` 的依赖图中不包含 `core`、`internal/sourceapi` 或
`music-lib`，因此桥接可以独立构建和发布。

桥接管理边界：

```text
internal/fnos-bridge/
├── bridge.go          飞牛协议与路由
├── bindings.go        官方曲库绑定与收藏恢复
├── tasks.go           下载任务面板适配
├── takeover.go        官方 Socket 接管
├── mapping.go         持久 GUID 映射
└── sourcecontract/    source API 契约和 Unix Socket 客户端
```

## 发布产物

```bash
./packaging/fnos-bridge/build-release.sh
```

默认生成：

```text
dist/fnos-bridge/
├── go-music-dl-fnos-bridge-<版本>-x86.fpk
├── go-music-dl-fnos-bridge-<版本>-arm.fpk
└── SHA256SUMS
```

构建脚本缺少 `fnpack` 时会从飞牛官方地址下载到用户缓存目录。也可以设置
`FNPACK` 或修改 `FNPACK_VERSION`。

## FPK 安装

FPK 声明依赖 `go-music-dl>1.1.3`，适合先安装新版 Web 应用，再从应用中心
安装本应用。source API 路由由 go-music-dl Web 进程直接挂载在原有 Socket 上。
安装向导可以设置：

- 参与飞牛音乐搜索的音源。

音源 Cookie、下载目录和音质先在 go-music-dl 应用设置中完成配置。安装后由
应用中心直接启动、停止和升级。桥接进程以 root 运行，因为飞牛音乐 Socket
位于 `/var/run`；go-music-dl Web 应用仍保持普通包用户权限。桥接通过
`go-music-dl` FPK 启动的受令牌保护 source API 获取数据，不导入 `core`。

数据位置：

```text
${TRIM_PKGVAR}/bridge.env
${TRIM_PKGVAR}/fnos-music/guid-map.json
${TRIM_PKGVAR}/bridge.log
```

停止应用时，桥接进程会先恢复官方 `/var/run/trim_music.socket`。

## 安全边界

- 不把 go-music-dl Web 服务切换为 root。
- 桥接只接管官方音乐 Socket；默认把 Socket 模式设为 `0666` 以保持飞牛客户端
  可连接，可通过 FPK profile 中的 `FNMUSIC_SOCKET_MODE` 收紧。
- 不配置无限制 sudo 权限，也不使用 setuid。
- `bridge.env` 和 GUID 映射目录仅 root 可写。
- 卸载不会主动删除已下载音乐和 GUID 映射；需要清理时由用户明确处理。
