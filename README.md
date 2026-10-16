# opencodehub

把 **OpenCode Zen 免费模型**接进 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的插件。

它是 [OpenCodeProxyHub](https://github.com/ki11a-Conton/OpenCodeProxyHub)（TypeScript + Fastify 独立网关，MIT）的 **Go 重写版**，从"再跑一个网关进程"变成"一个 DLL 插件"：不占端口、不需要 API Key、不需要 Docker、不用改 CLIProxyAPI 本体。

> **English TL;DR** — A Go port of OpenCodeProxyHub packaged as a CLIProxyAPI plugin. It exposes the OpenCode Zen free tier through the host's existing OpenAI-compatible endpoint. No auth files, no extra process, no port. Build a DLL, drop it in `plugins/`, add one config block, restart. Single-file port: ~2.7k lines of Go, 25 tests.

---

## 为什么值得做成插件

原版 OpenCodeProxyHub 是个完整网关：Fastify 服务、API Key 管理、出口代理池、Redis 限流、React 控制台，默认监听 `0.0.0.0:6446`。功能很全，但如果你**已经在跑 CLIProxyAPI**，就等于为了几个免费模型多养一个进程、多占一个端口、多一套 Key 要管。

CLIProxyAPI 的插件 ABI 正好能装下这件事，而且有几个刚好契合的性质：

- **`executor_model_scope: "static"`** —— 宿主允许静态模型在没有选中 auth 的情况下执行，直接以 `auth == nil` 调用插件 executor（`internal/pluginhost/executor_route.go` 注释原文：*"allow static execution without selected auth"*）。Zen 免费层本来就是匿名的，这个模型完全对得上。
- **`model_router: true`** —— 光有静态模型还不够：请求会落到 auth 选择器，而本插件没有任何 auth 记录，结果是 `auth_unavailable: no auth available`（HTTP 503）。路由器让插件先认领自己的模型、直接送回自己的 executor，**整条 auth 选择路径被绕开**。

所以本插件：**`auths/` 目录零新增文件**，没有登录、没有 token 刷新、没有过期。

---

## 特性

| | |
|---|---|
| **12 个免费模型** | `big-pickle`、`cat-jam-free`、`deepseek-v4.1-flash-free` 等，编译内置 |
| **零凭证** | 上游匿名 `Authorization: Bearer public`，无 auth 文件 |
| **启动无关代理** | 模型清单编译内置，注册过程零网络 I/O；代理开不开都不影响 CLIProxyAPI 启动 |
| **流式 / 非流式** | 上游强制 SSE，插件侧自动折叠非流式调用 |
| **工具调用** | 完整实现上游 `read`/`bash` 工具门禁，双向改名 |
| **管理面板** | CLIProxyAPI 控制台内一个只读页，显示已发布模型与出口路径 |
| **可配出口** | `OPENCODEHUB_PROXY` 环境变量覆盖代理，或 `direct` 直连 |

---

## 快速开始

### 1. 编译

```bash
git clone https://github.com/ki11a-Conton/opencodehub.git
cd opencodehub
```

需要 Go 1.26+ 和 C 编译器（`-buildmode=c-shared` 需要 cgo）。仓库自带 `build.sh`（Linux / macOS）和 `build.cmd`（Windows），会自动检测编译器：

```bash
# Linux / macOS
./build.sh /path/to/cpa/plugins

# Windows
build.cmd C:\path\to\cpa\plugins
```

也可以手写完整命令：

```bash
# Linux / macOS
CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -ldflags="-s -w" \
  -o /path/to/cpa/plugins/opencodehub.so .

# Windows (WinLibs MinGW)
set CGO_ENABLED=1
set CC=C:\path\to\mingw64\bin\gcc.exe
go build -buildmode=c-shared -trimpath -ldflags="-s -w" ^
  -o "C:\path\to\cpa\plugins\opencodehub.dll" .
```

> Windows 上产物是 `.dll`，Linux 是 `.so`，macOS 是 `.dylib`。文件名（不含扩展名）必须与插件 id 一致。

### 2. 启用

在 CLIProxyAPI 的 `config.yaml` 里加一段：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    opencodehub:
      enabled: true
```

### 3. 重启

```bash
curl http://127.0.0.1:8317/v1/models -H "Authorization: Bearer <你的-api-key>"
```

应该能看到 12 个新增模型（`owned_by: opencodehub`）。

调用方式和任何 OpenAI 兼容模型一样：

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <你的-api-key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}'
```

---

## 模型列表

12 个免费层模型编译内置，因此**首次启动、完全无网络时 `/v1/models` 也是正确的**：

```
big-pickle
cat-jam-free
deepseek-v4.1-flash-free
jev-1.13-free                    (System One 专用，见下)
longcat-2.5-preview-free
mimo-v2.6-flash-free
muse-spark-1.2-contributor-free
muse-spark-1.3-contributor-free
nemotron-3.5-lightning-free
nemotron-3.5-super-vl-free
space-bunny-free
step-5-preview-free
```

后台每 10 分钟拉一次 `https://opencode.ai/zen/v1/models`，**只增不减**：上游新发布的 `*-free` 模型会自动补进来，已有模型永远不会被移除。刷新失败时继续用内置清单，错误日志最多 30 秒一条。

> ⚠️ `jev-1.13-free` 上游只支持 System One 接口，走 `/v1/chat/completions` 会被拒绝。它出现在列表里是为了完整性。

模型 id 与现有渠道**无冲突**：本插件只注册带 `-free` 后缀的 id 和 `big-pickle`，CLIProxyAPI 里同名的 `space-bunny`、`step-5-preview`（无 `-free`）属于其它渠道，路由器不会碰。

---

## 出口与代理

默认走本机代理 `http://127.0.0.1:7897`（Clash Verge 混合端口）。用 `OPENCODEHUB_PROXY` 覆盖：

| 值 | 效果 |
|---|---|
| 不设置 | 使用 `http://127.0.0.1:7897` |
| `http://127.0.0.1:1080` | 改用该代理 |
| `direct` / `none` / `off` | 直连，不走代理 |

TCP 连接有 3 秒拨号超时，代理**按请求解析**（改了立刻生效，不用重启）。

**插件从不触碰代理进程本身。** 关掉 Clash Verge 的后果只有一条：模型调用返回干净的传输错误（HTTP 500，message 里带 `connectex: No connection could be made`）。CLIProxyAPI 启动、其它插件、其它渠道全部不受影响 —— 已实测验证（见下）。

---

## 验证过的行为

### 代理关闭时启动

把出口指向一个死端口来模拟"Clash Verge 没开"（全程不碰真实代理进程）：

```
plugin registered:  7 / 7          (mimo, opencodehub, qoder, trae, workbuddy-bridge, workbuddy, zcode)
failed to load:     0
Access denied:      0
models for auth:    0
模型总数:           91，其中 opencodehub 12
插件日志:           仅 1 行限频错误
调用模型:           HTTP 500（预期），CPA 本身健康
```

对照：修复前的 workbuddy-bridge 在同样场景会刷 31 条 `models for auth ... failed` 警告。本插件注册路径**零网络 I/O**，不存在这个失败模式。

### 代理开启时

```
big-pickle                 -> HTTP 200  "OK"
cat-jam-free               -> HTTP 200  "OK"
deepseek-v4.1-flash-free   -> HTTP 200  "OK"
mimo-v2.6-flash-free       -> HTTP 200  "OK"

流式:    14 个 data 块 + [DONE]
工具门禁: finish_reason=tool_calls，名称还原为客户端拼写的 "Bash"
不劫持:  space-bunny 正确路由回它自己的 provider
```

---

## 上游协议（为什么代码长这样）

这些是移植过程中**逐条验证过的硬约束**，不是风格选择：

### 请求头（10 个，全部必需）

```
Accept-Encoding: identity
Content-Type: application/json
Content-Length: <UTF-8 字节数>
Authorization: Bearer public
User-Agent: opencode/latest/2.0.29/cli
x-opencode-client: cli
x-opencode-project: global
x-opencode-request: msg_<升序时间戳编码>
x-opencode-session: ses_<降序时间戳编码>
x-opencode-session-id: <与上一行完全相同>
```

### Session ID 必须降序 ⚠️

这是整个移植里**最容易踩的坑**。上游会校验 id 的编码形状，必须是 OpenCode 自己 `SessionID.create()` 产出的那种：

```
ses_ + 12 位 hex + 14 位 base62
时间部分 = ~(ms * 0x1000 + counter)，截断到 48 位
```

**用升序时间戳（或全 0）会被判成"不是从 OpenCode 里调用的"**，返回 HTTP 403：

```json
{"type":"error","error":{"type":"FreeTierError",
 "message":"OpenCode's free tier can only be used from within OpenCode. Download it at https://opencode.ai and keep building."}}
```

换 UA、加 `Authorization`、补齐所有头都没用 —— 只有时间戳方向对才行。每次上游尝试（含重试）都生成新 session id。

### 上游永远流式

`stream: true` 无条件写进请求体，**不透传下游的 `stream` 字段**。下游要非流式时，插件把 SSE 折叠回单个 JSON 响应体。所以：上游成功响应永远是 SSE，而错误可能是裸 JSON。

### 工具门禁

上游只接受**同时声明了 `read` 和 `bash`**（大小写敏感、必须小写）的请求。插件：

- 客户端写 `Read`/`Bash` → 出站改名成小写，入站改回客户端拼写
- 客户端没声明 → 注入 placeholder（description 明说"不可用、绝不能被调用"）
- 模型真调了 placeholder → **从响应里丢弃**，若因此没有工具调用，`finish_reason` 从 `tool_calls` 改回 `stop`
- 其它工具名一律原样透传，不做任何字符集过滤

### 错误语义

- HTTP 200 但 body 是错误对象 → 当错误处理，不当空回答
- HTTP 200 但聚合不出任何内容 → 报错
- 传输失败 → 500，message 保留原始 `connectex` 细节便于排查

---

## 开发

```
opencodehub-0.1.0-source/
├── plugin.go              C ABI 导出、RPC 信封、方法分派、能力声明
├── zen.go                 Zen 线协议：10 个头、id 编码、请求体构造
├── tools.go               read/bash 工具门禁（双向改名 + placeholder 丢弃）
├── models.go              内置清单 + 后台 catalog 刷新（只增不减、不阻塞）
├── router.go              model.route：认领自己的模型，绕开 auth 选择器
├── executor.go            执行 / 流式泵 / SSE 清洗 / 非流式聚合
├── httpclient.go          传输层：按请求解析代理、直连模式、超时
├── management.go          只读管理面板
└── *_test.go              25 个测试
```

```bash
go test -race ./...
gofmt -l .
go vet ./...
```

测试覆盖：session id 编码与方向、10 个请求头、强制流式、工具门禁双向、SSE 块清洗、补全聚合、路由范围、**代理关闭时的降级**、以及**路由认领集合与 `model.static` 发布集合永远一致**（这两个曾经不一致，会导致上游新发现的模型 503）。

---

## 升级 CLIProxyAPI 会不会坏

不会。插件只依赖两样东西：

1. `plugins/opencodehub.{dll,so,dylib}` —— 独立文件
2. `config.yaml` 里一个 `opencodehub:` 配置块

CLIProxyAPI 本体一个字节都不用改。替换 `cli-proxy-api.exe` 升级后插件照常加载。唯一需要注意的是插件 ABI 版本（本插件按 `pluginabi.ABIVersion` 声明，宿主会在不兼容时拒绝加载）。

---

## 免责声明

- 本项目仅供个人学习与技术研究使用，用于理解 OpenAI 兼容协议、CLIProxyAPI 插件 ABI 与流式转换的实现细节。
- 使用本项目对接任何第三方服务时，使用者须自行阅读并遵守该服务的服务条款、使用政策以及所在地区的相关法律法规。
- 因使用、修改或分发本项目而产生的任何后果与责任，均由使用者自行承担，本项目作者及贡献者不承担任何责任。
- 本项目与 OpenCode、OpenCodeProxyHub、CLIProxyAPI 及任何上游服务方均无隶属关系。

## 致谢

- [OpenCodeProxyHub](https://github.com/ki11a-Conton/OpenCodeProxyHub) —— 上游协议实现与踩坑记录的直接来源
- [opencode-free-proxy](https://github.com/bigdata2211it-web/opencode-free-proxy) —— OpenCodeProxyHub 的前身
- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) —— 宿主与插件 SDK

## 许可

MIT，继承自 OpenCodeProxyHub。
