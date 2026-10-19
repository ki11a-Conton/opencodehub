// management.go — a small read-only management panel.
//
// It exists so the operator can see, from the CPA control panel, exactly which
// models this plugin publishes and whether the upstream is currently
// reachable — the two facts that matter when the local proxy is toggled.
package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// managementRoute is one registered Management API route.
type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

// resourceRoute is one registered browser-navigable resource route.
type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

// managementRegistrationResponse is the management.register reply.
type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// managementRegistration declares the panel: a status JSON endpoint plus the
// HTML page that renders it.
func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []managementRoute{{
			Method:      http.MethodGet,
			Path:        "/plugins/" + providerName + "/status",
			Description: "opencodehub: published models and upstream reachability",
		}},
		Resources: []resourceRoute{{
			Path:        "/panel",
			Menu:        "OpenCode Zen",
			Description: "opencodehub status panel",
		}},
	}
}

// handleManagement serves the registered routes.
func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	if strings.HasSuffix(strings.TrimSuffix(req.Path, "/"), "/panel") {
		return okEnvelope(mgmtHTMLResponse(http.StatusOK, panelHTML)), nil
	}
	models := currentModels()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	body, _ := json.Marshal(map[string]any{
		"provider":  providerName,
		"version":   pluginVersion,
		"upstream":  "https://" + zenHost + zenChatPath,
		"transport": transportLabel(),
		"auth":      "anonymous (Bearer public)",
		"models":    ids,
		"count":     len(ids),
		"note":      "Models answer while the local proxy is running; if the proxy is down or its address is refused by the free tier, the plugin switches to a direct dial on its own.",
	})
	return okEnvelope(mgmtJSONResponse(http.StatusOK, body)), nil
}

// mgmtJSONResponse builds a ManagementResponse carrying JSON.
func mgmtJSONResponse(status int, body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

// mgmtHTMLResponse builds a ManagementResponse carrying an HTML page.
func mgmtHTMLResponse(status int, page string) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: []byte(page)}
}

// panelHTML is a self-contained page: it fetches the status endpoint and
// renders the published model list.
const panelHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>OpenCode Zen</title>
<style>
  body { font: 14px/1.6 -apple-system, "Segoe UI", sans-serif; margin: 24px; color: #1f2328; }
  h1 { font-size: 18px; margin: 0 0 4px; }
  .muted { color: #6b7280; }
  table { border-collapse: collapse; margin-top: 16px; width: 100%; max-width: 720px; }
  th, td { border: 1px solid #e5e7eb; padding: 6px 10px; text-align: left; }
  th { background: #f9fafb; font-weight: 600; }
  code { background: #f3f4f6; padding: 1px 4px; border-radius: 3px; }
</style>
</head>
<body>
<h1>OpenCode Zen 免费模型</h1>
<div class="muted" id="meta">加载中…</div>
<table id="tbl">
  <thead><tr><th>#</th><th>模型 id（客户端调用名）</th></tr></thead>
  <tbody></tbody>
</table>
<div class="muted" id="note" style="margin-top:16px"></div>
<script>
fetch('/v0/management/plugins/opencodehub/status')
  .then(function (r) { return r.json(); })
  .then(function (env) {
    var d = env.result || env;
    document.getElementById('meta').textContent =
      'provider=' + d.provider + ' v' + d.version + ' · 上游 ' + d.upstream +
      ' · 出口 ' + d.transport + ' · ' + d.count + ' 个模型';
    var tb = document.querySelector('#tbl tbody');
    (d.models || []).forEach(function (id, i) {
      var tr = document.createElement('tr');
      tr.innerHTML = '<td>' + (i + 1) + '</td><td><code>' + id + '</code></td>';
      tb.appendChild(tr);
    });
    document.getElementById('note').textContent = d.note || '';
  })
  .catch(function (e) {
    document.getElementById('meta').textContent = '无法读取状态：' + e;
  });
</script>
</body>
</html>
`
