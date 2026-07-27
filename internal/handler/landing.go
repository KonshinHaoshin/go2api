package handler

import (
	"fmt"
	"net/http"
	"strings"
)

// Landing handles GET / and returns a small HTML page that explains what the
// server is and points users to the web UI. Without this Gin returns the bare
// "404 page not found" text, which is confusing when someone hits the API
// port directly.
func Landing(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		// Anything else is a real 404.
		NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, landingHTML)
}

// NotFound renders a friendly 404 page that lists the routes the server
// actually exposes. It returns JSON to API-style Accept headers, HTML otherwise.
func NotFound(w http.ResponseWriter, r *http.Request) {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"message":"接口不存在","type":"not_found","hint":"这是 go2api API,管理后台请访问 Web UI 端口"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, notFoundHTML)
}

const landingHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<title>go2api</title>
<style>
  body { font: 14px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
         background: #0b0d10; color: #e6ebf0; margin: 0; padding: 60px 20px; }
  .wrap { max-width: 720px; margin: 0 auto; }
  h1 { font-size: 22px; margin: 0 0 6px; }
  .sub { color: #97a1ac; margin-bottom: 28px; }
  .card { background: #14181d; border: 1px solid #2a323c; border-radius: 10px;
          padding: 18px 22px; margin-bottom: 14px; }
  code { font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 13px;
         background: #1c2229; padding: 2px 6px; border-radius: 4px; color: #f97316; }
  .pill { display: inline-block; padding: 2px 8px; border-radius: 999px;
          font-size: 11px; background: rgba(249, 115, 22, 0.12); color: #f97316;
          text-transform: uppercase; letter-spacing: 0.4px; }
  table { width: 100%; border-collapse: collapse; margin-top: 10px; }
  th, td { text-align: left; padding: 7px 10px; border-bottom: 1px solid #20262e; }
  th { color: #97a1ac; font-weight: 500; font-size: 11.5px;
       text-transform: uppercase; letter-spacing: 0.4px; }
  .muted { color: #6b7480; font-size: 12px; }
</style>
</head>
<body>
<div class="wrap">
  <span class="pill">go2api · API 服务</span>
  <h1 style="margin-top:10px">这是后端 API 服务。</h1>
  <p class="sub">您访问的是 go2api 的 Go 后端。管理后台运行在另一个端口,由 Node.js Web UI 提供。</p>

  <div class="card">
    <div style="margin-bottom:8px"><strong>需要进入管理后台?</strong></div>
    <div>启动 Web UI 并访问:</div>
    <p style="margin:10px 0"><code>cd web &amp;&amp; npm start</code></p>
    <div>默认监听 <code>:8081</code>,登录后即可使用 dashboard 管理 Key、缓存和模型。</div>
  </div>

  <div class="card">
    <strong>可用接口</strong>
    <table>
      <thead><tr><th>方法</th><th>路径</th></tr></thead>
      <tbody>
        <tr><td>GET</td><td><code>/healthz</code></td></tr>
        <tr><td>POST</td><td><code>/v1/chat/completions</code> <span class="muted">(OpenAI 兼容)</span></td></tr>
        <tr><td>POST</td><td><code>/v1/messages</code> <span class="muted">(Anthropic 兼容)</span></td></tr>
        <tr><td>POST</td><td><code>/v1/responses</code> <span class="muted">(OpenAI Responses 兼容)</span></td></tr>
        <tr><td>GET</td><td><code>/v1/models</code></td></tr>
        <tr><td>GET</td><td><code>/admin/keys</code> <span class="muted">(需要鉴权)</span></td></tr>
        <tr><td>GET</td><td><code>/admin/stats</code> <span class="muted">(需要鉴权)</span></td></tr>
      </tbody>
    </table>
  </div>

  <p class="muted">通过 <code>Authorization: Bearer &lt;token&gt;</code> 或 <code>x-api-key: &lt;token&gt;</code> 传递管理员令牌。</p>
</div>
</body>
</html>`

const notFoundHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<title>404 · go2api</title>
<style>
  body { font: 14px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
         background: #0b0d10; color: #e6ebf0; margin: 0; padding: 80px 20px; text-align: center; }
  .code { font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 64px;
          color: #f97316; letter-spacing: -2px; margin-bottom: 10px; }
  h1 { font-size: 20px; margin: 0 0 6px; }
  .sub { color: #97a1ac; margin-bottom: 28px; }
  a { color: #f97316; text-decoration: none; }
  code { font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 13px;
         background: #1c2229; padding: 2px 6px; border-radius: 4px; }
</style>
</head>
<body>
  <div class="code">404</div>
  <h1>该路径没有对应的接口</h1>
  <p class="sub">这是 go2api API 服务。请使用上方的接口列表,或打开 Web UI 使用管理后台。</p>
  <p><a href="/">← 返回 API 总览</a></p>
</body>
</html>`
