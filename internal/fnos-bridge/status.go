package fnosbridge

import (
	"net/http"
	"strings"
)

const bridgeDesktopPath = "/app/go-music-dl-fnos-bridge"

const bridgeStatusHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>音乐下载桥接</title>
  <style>
    :root { color-scheme: light dark; font-family: -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #f4f6f8; color: #17202a; }
    main { width: min(680px, calc(100% - 40px)); padding: 32px; border-radius: 18px; background: #fff; box-shadow: 0 12px 40px rgba(0,0,0,.08); }
    h1 { margin: 0 0 10px; font-size: 28px; }
    p { line-height: 1.7; color: #52606d; }
    .status { margin: 24px 0; padding: 16px; border-radius: 12px; background: #eef8f1; color: #176b3a; font-weight: 600; }
    dl { display: grid; grid-template-columns: 120px 1fr; gap: 10px 16px; margin: 0; }
    dt { color: #7b8794; }
    dd { margin: 0; overflow-wrap: anywhere; }
    @media (prefers-color-scheme: dark) {
      body { background: #12161b; color: #edf2f7; }
      main { background: #1c232b; }
      p, dt { color: #aab6c2; }
      .status { background: #173b29; color: #8ee6ae; }
    }
  </style>
</head>
<body>
  <main>
    <h1>音乐下载桥接</h1>
    <p>桥接服务正在运行。请在飞牛音乐中搜索外部音源、播放或下载歌曲。</p>
    <div class="status" id="status">正在检查服务状态…</div>
    <dl>
      <dt>接入方式</dt><dd>飞牛音乐 Socket 反向代理与协议适配</dd>
      <dt>下载位置</dt><dd>go-music-dl 设置中的 NAS 下载目录</dd>
      <dt>管理入口</dt><dd>飞牛应用中心</dd>
    </dl>
  </main>
  <script>
    fetch('/app/go-music-dl-fnos-bridge/healthz', { cache: 'no-store' })
      .then(r => r.ok ? r.json() : Promise.reject(new Error('HTTP ' + r.status)))
      .then(data => {
        document.getElementById('status').textContent =
          data.ok ? '服务运行正常 · ' + data.sources.join(' / ') : '服务状态异常';
      })
      .catch(() => { document.getElementById('status').textContent = '无法获取服务状态'; });
  </script>
</body>
</html>`

func (b *Bridge) handleDesktopStatus(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	if path == bridgeDesktopPath+"/healthz" {
		b.handleHealth(w, r)
		return
	}
	if path != bridgeDesktopPath {
		b.handleUpstream(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(bridgeStatusHTML))
	}
}
