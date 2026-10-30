# 协议清单与形状冻结（v1，阶段 A 出口）

> 本文件是阶段 A-10 的产物：**7 类协议的形状定义 + fixture 位置 + 冻结规则**。
> 阶段 A 出口第 ⑤ 道门要求「7 类协议形状冻结 + 各有 1 个 fixture 生效」，本文件即该门的验收依据。
>
> 冻结规则（铁律）：形状一旦冻结，**只允许向后兼容地新增字段**（新增可选字段 + 明确默认值）。
> 破坏性变更必须走三步：改 `api/openapi.yaml` → 重新生成/校对 `web/src/lib/apiTypes.ts` → 双端同步。
> 公共版本号：`observ.ProtocolVersion`（HTTP/健康检查暴露）与 `events.ProtocolVersion`（事件 `v` 字段）当前均为 `1`。

---

## 0. 总览

| # | 协议 | 定义位置 | fixture（可执行证据） | 状态 |
| --- | --- | --- | --- | --- |
| 1 | 工具契约 | `pkg/toolapi` | `internal/plugins/echo` + `internal/api/tools_integration_test.go` | ✅ 冻结 |
| 2 | Run 事件协议 | `internal/events` | `internal/events/bus_test.go`（回放/背压/补发） | ✅ 冻结 |
| 3 | HTTP API 契约 | `api/openapi.yaml` → `web/src/lib/apiTypes.ts` | `internal/api/integration_test.go` + 路径一对一校验 | ✅ 冻结 |
| 4 | SSE 传输协议 | `internal/api/sse.go` + `handler.go:runEvents` | `TestEndToEndChatFlow`（断线重连补发） | ✅ 冻结 |
| 5 | 审批协议 | `internal/approval` | `internal/approval/gate_test.go` + `TestEchoToolInvokedAndRecorded` | ✅ 冻结（跨端仲裁留阶段 C） |
| 6 | 设备协议 | 本文件 §6（形状已定，表结构与 WS 留阶段 C） | 假 Runner（阶段 C 提供） | 🟡 形状冻结、实现待 C |
| 7 | 插件协议 | `pkg/toolapi.Manifest` + 本文件 §7 | `internal/plugins/echo`（内置形态）+ `registry_test.go` | 🟡 内置形态冻结、MCP/HTTP/包待 E |

---

## 1. 工具契约协议（Tool Contract）

### 1.1 定义（`pkg/toolapi`，零内核依赖，可被第三方 import）

```go
type Spec struct {
    Name         string
    Namespace    string          // builtin | mcp:<server> | pkg:<id>
    Description  string
    SchemaJSON   json.RawMessage // JSON Schema（全额计入注入成本）
    Tier         Tier            // core | deferred | on_demand
    Executor     Executor        // server | device | any
    Capabilities []string        // workspace.read / workspace.write / process / network / llm / device
    CostTokens   int             // 单条注入成本（估算，>0）
    Risk         Risk            // readonly | write | command | danger
    Requires     []string        // 运行期依赖（chromium、git …）
    Version      string
}

type Tool interface {
    Spec() Spec
    Invoke(ctx context.Context, call *Call) (*Result, error)
}

type Call struct { CallID, RunID, SessionID, ToolEventID, Name string; Args json.RawMessage; AccessMode string }

type Result struct { Content string; IsError bool; BlobRef string; CostTokens int; Meta map[string]any }
```

### 1.2 三条硬约定

1. **结果必须是合法 JSON**：内核把 `Result` 封装为 `{"content": ..., "meta"?: ..., "cost_tokens"?: ...}` 写入
   `tool_events.result`（列类型为 `json.RawMessage`）。直接塞纯文本会让事件序列化失败
   （曾导致 `GET /tool-events` 返回 **200 + 空响应体**，已在 `internal/tools/invoke.go` 修复并有测试守护）。
2. **失败不返回 error**：`IsError=true` + `Content` 写「给模型看的失败原因」，由引擎回灌模型继续；
   `error` 只用于真实故障（仍会翻译成 `tool_events.status=error`）。这样保证 `tool_events` 只落一次。
3. **能力必须经 `Host`**：`Host` 共 9 项 —— `Workspace / Exec / LLM / Blobs / Events / Config / Spawn / Tasks / Snapshots`。
   `Spawn`（嵌套 Run）、`Tasks`（长任务）、`Snapshots`（快照）在阶段 A 是**协议占位**，调用返回
   `toolapi.ErrNotImplemented`，阶段 F/G 填实。插件拿不到 `*sql.DB`、拿不到原始 `llm.Provider`。

### 1.3 注入分层（D11）与软上限

| Tier | 注入内容 | 单条成本 | 软上限 |
| --- | --- | --- | --- |
| `core` | 全量定义进 `tools` 数组 | ~250–320 token | `tools.core_soft_limit`（默认 8） |
| `deferred` | 仅「名称 + 一行描述」进 system 索引 | ~18 token | `tools.deferred_soft_limit`（默认 60） |
| `on_demand` | 零注入 | 0 | 不限 |

- **软上限只告警、绝不阻断启动**；档位覆盖优先级：插件声明 `default_tier` < 主库 `settings.tools.tiers` <
  会话级 `session_settings`（阶段 D）。
- **宽进严出**：tier 只决定注入深度，不决定能否执行；未注入的已注册工具仍可被显式调用。

### 1.4 fixture

`internal/plugins/echo`：`manifest.json`（`//go:embed`）+ `factory.go`，`echo` 工具为 `core` / `server` /
`readonly`，参数 `{"text": "..."}` → 结果 `echo: <text>`。
验收测试 `TestEchoToolInvokedAndRecorded` 断言：模型发起 tool_call → 审批闸门（readonly 自动放行）→
执行 → `tool_events` 落 `status=ok / risk=readonly / executor=server / result.content=echo: hello` →
消息序列含 `tool` 消息与最终 `assistant` 消息。

---

## 2. Run 事件协议

```go
type Event struct {
    V       int             `json:"v"`      // 协议版本（当前 1）
    RunID   string          `json:"run_id"`
    Seq     int64           `json:"seq"`    // per-Run 单调递增
    Type    Type            `json:"type"`
    Payload json.RawMessage `json:"payload"`
}
```

| type | payload 关键字段 | 落库 |
| --- | --- | --- |
| `run.status` | `status`, `stop_reason`, `error_code`, `error_message` | 是（runs） |
| `message.start` | `message_id`, `role`, `seq` | 里程碑 |
| `message.delta` | `message_id`, `delta` | **否（仅预览）** |
| `message.end` | `message_id`, `content`, `token_count` | 是（messages） |
| `tool.started` | `tool_event_id`, `tool_name`, `args`, `risk` | 是（tool_events） |
| `tool.progress` | `tool_event_id`, `chunk` | 否 |
| `tool.result` | `tool_event_id`, `status`, `result`, `truncated` | 是 |
| `approval.request` | `approval_id`, `tool_event_id`, `tool_name`, `risk`, `reason` | 是（approvals） |
| `approval.decision` | `approval_id`, `decision`, `reason` | 是 |
| `resync` | `last_seq`, `message_count`, `run_status`, `close_reason` | 否 |
| `error` | `code`, `message` | 否（非致命，Run 可继续） |

**铁律**：`delta` 不落库，**消息最终内容以 DB 为准**；流式 delta 只是预览。

背压：per-Run 环形缓冲 500 + 每订阅 channel 256；缓冲满即**主动断开慢消费者**（绝不阻塞 Run）。
订阅 `fromSeq >= 0` 一律补发 `seq > fromSeq` 的历史事件（`-1` 表示只要新事件）。

fixture：`internal/events/bus_test.go` 覆盖「环形缓冲裁剪 / 慢消费者不断流 Publish / fromSeq 精确补发 / 多订阅者」。

---

## 3. HTTP API 契约

- 单一真源：`api/openapi.yaml`（22 条 path）。类型镜像：`web/src/lib/apiTypes.ts`。
- 错误体统一：`{"error":{"code","message","trace_id","details"}}`；`trace_id` 与日志一致。
- 鉴权：`Authorization: Bearer <token>` 优先，其次 Cookie `hg_sess`（HttpOnly / SameSite=Lax）。
  免鉴权路径仅 `/healthz`、`/api/v1/auth/login`、`/api/v1/auth/sso`。
- 时间戳一律 Unix 毫秒；分页统一 `{"items","limit","offset"}`；响应 `Cache-Control: no-store`。
- `tool_events.result` 与 `messages.tool_calls` 等「结构化列」必须合法 JSON（见 §1.2 第 1 条）。

**阶段 A 工具三端点**（本包新增）：

| 端点 | 语义 |
| --- | --- |
| `GET /api/v1/tools` | 注册表快照：`total` / `core[]` / `deferred[]` / `on_demand[]` / `cost_*` / `limits` / `warnings` / `plugins[]` / `overrides` |
| `GET /api/v1/tools/injection` | 注入预算明细：`core` 全量、`deferred_index[]`（每条 ~18 token）、`on_demand`、`deferred_hidden[]`、`cost_core/cost_index/cost_total` |
| `PATCH /api/v1/tools/tiers` | 写主库 `settings.tools.tiers`，内存立即生效，**下一轮对话生效**（校验：未知工具/非法档位 → 400） |

零漂移校验：`apiTypes.ts` 的 `API_PATHS` 与 `openapi.yaml` 的 `paths` 必须一一对应（见 `make check` 说明与 A-10 验收脚本）。

---

## 4. SSE 传输协议

- 端点：`GET /api/v1/sessions/{sid}/runs/{rid}/events`
- 帧格式：`id: <seq>\nevent: <type>\ndata: <json>\n\n`；心跳 `: ping` 每 15s。
- 续传：请求头 `Last-Event-ID: <seq>` → 只补发 `seq > <seq>`。
- 终态对齐：Run 终态或慢消费者断开后，服务端发 `resync` 事件（含 `last_seq` / `run_status` / `close_reason`）再关闭连接；
  客户端据此重拉 `/messages` 快照，实现「DB 为准」的对齐。
- 反代要求：`X-Accel-Buffering: no`；服务端 `WriteTimeout=0`（SSE 长连接）。
- 客户端状态机（B-1 `lib/sse.ts`）：`idle → connecting → open → backoff → resync`；`visibilitychange` 回前台立即重连。

---

## 5. 审批协议

```go
type Request struct { ApprovalID, RunID, ToolEventID, ToolName string; Risk model.Risk; ArgsJSON string; AccessMode AccessMode; Rules []Rule }
type Verdict struct { Auto Decision; NeedApproval bool; Reason string }
```

判定顺序：**风险级 → 会话规则（`approval_rules`）→ 访问模式（readonly/confirm/full）→ danger 强制审批**。
`readonly` 一律自动放行；`allow_session` 决策会写入会话级规则（仅本会话生效）。

等待语义：`chan + ctx + 5min 超时`；**超时/cancel 一律视为拒绝**，拒绝原因回传模型（Run 不中断）。
决策取值：`allow` / `allow_session` / `deny`。

fixture：`internal/approval/gate_test.go`（超时拒绝 / ctx 取消拒绝 / 未知审批拒绝 / readonly 放行 / 会话规则生效）。

---

## 6. 设备协议（形状冻结，实现留阶段 C）

| 环节 | 形状 |
| --- | --- |
| 配对 | 6 位码，TTL 120s，一次性；配对成功返回 `device_id` + `device_token` |
| 长连接 | WS 出站（端 → 服），`ping{load, capabilities_hash, protocol_version}` 间隔 15s |
| 任务派发 | `task.dispatch{run_id, tool_event_id, tool_name, args, deadline}` → `task.progress{...chunk}` → `task.result{status, result, error}` |
| 租约 | `session_leases`（session_id, device_id, epoch, expires_at）替代本地 mutex，双端单写者 |
| 端侧结果 | 一律 `source_trust=untrusted`（服务端必须复检高风险结果） |
| 降级 | 端侧不可用时降级 `executor=server`（不变量 5），不向用户报错 |

数据库迁移 `0002` 将新增 `devices` / `capabilities` / `session_leases` 三表；
**字段现已预埋**：`runs.executor`、`runs.device_id`、`tool_events.executor`、`tool_events.device_id`、`tool_events.source_trust`。

---

## 7. 插件协议

```go
type Manifest struct {
    ID, Name, Version string
    Kind            Kind            // builtin | mcp | http | package | wasm（wasm 一期预留）
    DefaultTier     Tier
    ProtocolVersion int             // 当前 1
    Signature       string          // 市场包签名（内置为空）
    Requires        []string
    Tools           []Spec
}

type Factory interface { Manifest() Manifest; New(host Host, cfg Config) (Plugin, error) }
type Plugin  interface { Start(ctx) error; Stop(ctx) error; Tools() []Tool; Health() Health }
```

- **唯一一套 Manifest**：内置插件（`//go:embed manifest.json`）、市场包、MCP 转换、HTTP 插件复用同一结构。
- **生命周期**：`registered → validating → available / degraded / unavailable`（`Health.Status`）。
- **装配点唯一**：`cmd/harness-go/plugins.go` 的 `builtinFactories`（新增插件只加一行）。
- **分层守护**（depguard）：`internal/plugins/...` 与 `pkg/toolapi` **禁止** import
  `internal/{store,llm,agent,api,service}`。
- 阶段 E 待补：MCP 三传输（stdio/SSE/HTTP）、HTTP manifest 发现、本地工具包
  （`pkg.zip` = manifest + 多平台可执行 + 签名 → 子进程 stdio JSON-RPC，复用 MCP 代码路径）、信任分级与安装/卸载/回滚。

---

## 8. 前端静态资源与 embed 契约（B-1）

### 8.1 embed 契约

```go
// web/embed.go
package webdist

//go:embed all:dist
var Assets embed.FS
```

- `//go:embed all:dist` 在**编译期**要求 `web/dist` 目录存在 → 仓库必须保留 `web/dist/.gitkeep`（只占位、不缓存构建产物）。
- `.gitignore` 必须精确化，否则占位文件无法入库：
  ```
  /demo/dist/
  /web/dist/*
  !/web/dist/.gitkeep
  ```
- **未构建前端时不许编译失败、不许 500**：`index.html` 缺失则 `/` 返回 **200 提示页**（含构建命令），
  静态资源返回 404，并只打一次 `Warn` 日志。这保证 `go build` / `go test ./...` 在纯后端环境可用。

### 8.2 路由与缓存策略

| 路径 | 处理 | Cache-Control |
| --- | --- | --- |
| `GET /` | SSO 拦截（带 `user_name` 走 302）；否则返回 `index.html` | `no-store` |
| `GET /login` | SPA 入口（前端自行渲染 `LoginPage`） | `no-store` |
| `GET /assets/*` | 构建产物（文件名带内容 hash） | `public, max-age=31536000, immutable` |
| `GET /manifest.webmanifest` `/sw.js` `/icons/*` | PWA 资源 | `no-cache` |

### 8.3 两个不可回退的细节

1. **`.webmanifest` 的 MIME 必须显式映射为 `application/manifest+json`**：Windows 的 mime 注册表没有该扩展名，
   `mime.TypeByExtension` 会退化成 `application/octet-stream`，浏览器直接拒绝解析 PWA manifest（控制台报错）。
   实现见 `internal/api/static.go: contentTypeFor`。
2. **含 `..` 的原始路径直接 404**：`path.Clean("/../index.html")` 会归一成 `/index.html`，
   虽然 embed FS 不会越界，但放过它就是放过穿越意图，故在 Clean 之前显式拒绝。

前端适配层（B-1 新增，demo 无此文件）：
- `web/src/lib/api.ts`：`apiMode: 'mock' | 'real'`（`VITE_API_MODE` > `localStorage['harness.apiMode']` > 默认 `mock`），
  `ApiClient` 双实现同签名；real 侧 `fetch` 带 `credentials:'include'`、统一解析 `{error:{code,message,trace_id}}` 为 `ApiError`。
- `web/src/lib/sse.ts`：`EventSource` + 自管指数退避（不依赖浏览器自动重连），续传走 `?last_event_id=`（EventSource 无法设请求头，
  服务端 `runEvents` 同时支持 `Last-Event-ID` 头与该 query），收到 `resync` 后停止自动重连并回调上层重拉消息。

---

## 9. 阶段 A 出口自检清单（本文件覆盖部分）

| 门 | 判据 | 现状 |
| --- | --- | --- |
| ① lint + test | `gofmt -l .` 空、`go vet ./...` 干净、`go test ./...` 全绿 | ✅（`-race` 需 CGO/gcc，本机不可用，CI 跑 `make test-race`） |
| ② 单二进制冷启动 | 冷启动 → 多轮真实对话 | ✅ |
| ③ 重启不丢 | kill 后未完成 Run 置 `interrupted`（`RecoverInterrupted`） | ✅ |
| ④ 免登直达 | `GET /?user_name=&pwd=` → 302 + Cookie，地址栏无 query | ✅ |
| ⑤ 7 类协议冻结 + fixture | 本文件 §1~§7 | ✅（§6/§7 的形状冻结，实现随 C/E） |
