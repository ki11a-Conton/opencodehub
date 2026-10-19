# Changelog

本文件记录 opencodehub 的主要变更。格式参照 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [SemVer](https://semver.org/lang/zh-CN/)。

## [0.1.2] — 2026-10-19

出口被拒时的自愈。上游按**地址段**而不是按账号封禁免费档，插件原先对此毫无办法 —— 只能靠运维在启动脚本里写死 `OPENCODEHUB_PROXY=direct`。这一版把这件事收进插件内部。

### 修复

- **免费档按 IP 封禁时自动改走直连**。Clash 出口落在 AS31898（Oracle 斯德哥尔摩）时，10 个免费模型全部返回
  `403 FreeTierError:...abusive traffic from this IP address`；同一请求从家宽地址发出则 200 / 2.42 秒。
  现在聊天路径会在收到 `403` + `FreeTierError` 后挂起代理出口 10 分钟，**紧接着的那次重试直接拨号**，
  不给运维留任何手工动作，也不必再改启动脚本。
- **代理进程不在时同样自动直连**。经代理的拨号失败（Clash 关闭、端口改了）会挂起代理 5 分钟并改走直连，
  顺带把"代理关掉就必须手工切 direct"这条老规矩也去掉了。
- 免费档两种 403 措辞（`only from within OpenCode` / `abusive traffic from this IP`）都识别，
  判据只看 `FreeTierError` 类型标记；其它 403 不触发出口切换。

### 变更

- 插件版本 `0.1.1` → `0.1.2`。
- 管理面板 `transport` 字段会如实反映当前出口：代理正常时是代理地址，挂起时是
  `direct (proxy suspended: the free tier refused the proxy address)`。
- 新增 4 个用例（`TestFreeTierBodyDetection`、`TestFreeTierBlockSuspendsTheProxy`、
  `TestProxyBlockExpires`、`TestDirectModeIsUnaffectedByBlocks`），测试总数 26 → 30。

### 说明

- `OPENCODEHUB_PROXY=direct` 仍然有效，且优先级更高：写死直连后不会再有任何代理尝试，也不会有那一次探测性的 403。

## [0.1.1] — 2026-10-19

针对上游免费档收紧的一次集中修复。三个问题都来自真实调用日志，不是纸面推演。

### 修复

- **退役模型不再发布**。`muse-spark-1.2-contributor-free`、`muse-spark-1.3-contributor-free`（上游 410 `ModelDeprecated`）、`step-5-preview-free`（400 `Model is unavailable.`）、`jev-1.13-free`（400 `ModelProtocolUnsupported`）不再出现在 `/v1/models`。
  新增 `zenRetiredModels` 名单，`parseZenCatalog` 追加循环会跳过它们，所以后台刷新也不会把退役 id 复活。
  **所有权仍然保留**：客户端硬编码退役 id 时拿到的是上游自己的解释，而不是宿主的 `auth_unavailable` 兜底。
- **补入两个实测可用的模型**：`beam-free`、`glm-5.3-flash-free`。内置清单从 12 条（含 4 条死链）变成 10 条全可用。
- **响应头超时**。上游免费档会偶发静默卡死（不给响应头，一直挂着）。新增 `zenHeaderTimeout = 10s` 落进 `Transport.ResponseHeaderTimeout`。正常首字节 1.2–4 秒，10 秒切不到健康流。
- **请求重试**。新增 `zenMaxAttempts = 3`、`zenRetryBackoff = 400ms`，每次尝试**重新构造请求**（新 session id、新 msg id）。`retryableStatus` 只对 403 / 429 / 5xx 重试。
- **非流式聚合的 `role` 重复**。上游每个 SSE 帧都带 `"role":"assistant"`，`aggregateCompletion` 用 `WriteString` 累加，9 帧的答案返回 81 字符的脏 role。改为只取第一个非空值。

### 变更

- 插件版本 `0.1.0` → `0.1.1`。

### 文档

- 上游协议一节按 2026-10-19 的单变量隔离测试重写：
  - **新增**：免费档的门禁是请求里必须声明一个名为 `read`（精确小写）的工具。`bash` 可选，其它工具不加分。这是本项目最重要的协议约束 —— 客户端普通请求不会声明 `read`，所以插件的 placeholder 注入是免费档能通的前提，不是锦上添花。
  - **修正**：`Authorization: Bearer public` 不是必需的（全发但去掉它仍 200）。匿名免费层真实存在。
  - **修正**：session id 的**方向不是门禁**。升序时间戳实测照常 200。早期版本文档里"方向错了会 403"是归因错误 —— 当时真正起作用的是重试时重新构造了请求。编码形状仍然要对。
  - **新增**：`x-opencode-*` 是成组校验，单发其中一两个不够。

### 测试

- 26 个测试（+1）。新增 `TestAggregateCompletionRepeatedRole`；`TestAggregateCompletion` 补上 `role` 断言 —— 原夹具只有首帧带 `role`，所以那个 bug 漏过了。

## [0.1.0] — 2026-10-16

首个版本。OpenCodeProxyHub 的 Go 重写，作为 CLIProxyAPI 插件发布。

### 新增

- 12 个 Zen 免费模型编译内置，注册路径零网络 I/O（代理关着也不影响 CLIProxyAPI 启动）。
- `model_router: true` —— 插件认领自己的模型并直接送回 executor，绕开 auth 选择器，`auths/` 目录零新增文件。
- 完整实现上游 `read`/`bash` 工具门禁：双向改名、placeholder 注入与丢弃、`finish_reason` 修正。
- 上游强制 SSE，插件侧折叠非流式调用。
- 出口可配：`OPENCODEHUB_PROXY` 环境变量，或 `direct` 直连。
- 只读管理面板。
- `build.sh` / `build.cmd`（自动检测 C 编译器）、GitHub Actions 三平台 CI（gofmt / vet / test -race / c-shared 构建）。

[0.1.1]: https://github.com/ki11a-Conton/opencodehub/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/ki11a-Conton/opencodehub/releases/tag/v0.1.0
