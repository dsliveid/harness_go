# 阶段 D 剩余功能缺口清单（v1.0，2026-11）

> 本文档是长任务「完成阶段D剩余功能」的执行锚点。每项缺口都可追溯到
> `docs/开发实施计划.md` v2.7 或具体提交，禁止凭印象新增或臆造功能。

## 0. 技术栈与构建命令（实测，务必以此为准）

| 项目 | 事实 |
| --- | --- |
| 语言/框架 | **Go 1.22+**（标准库 `ServeMux`）+ React/Vite（`web/`） |
| 模块名 | `harness-go`（`go.mod` 在仓库根） |
| 构建 | `go build ./...` 或 `make build`（产物 `bin/harness-go.exe`） |
| 单元测试 | `go test ./...`（**无 `-race`**：本机无 gcc/CGO） |
| 静态检查 | `go vet ./...` + `gofmt -l .`（应为空） |
| 换行符检查 | `make check-eol`（仅 Linux/CI，Windows 跳过） |
| 单个包测试 | `go test ./internal/config -v` |

> ⚠️ **长任务蓝图中的 `dotnet` / `src/config/` / `dotnet test --filter` 全部不适用**：
> 本仓库是 Go 工程，无 `src/` 目录、无 `dotnet`。凡出现此类命令的门禁一律以
> `go test` 等价命令替代，勿按错误前提改造目录结构。

## 1. 阶段 D 完成度总览

| 子项 | 状态 | 证据 |
| --- | --- | --- |
| D-1 检索基础设施（FTS5 trigram + RRF + 向量表） | ✅ 完成 | `0003_retrieval.sql`、`internal/retrieval/{vector,rrf,indexer}.go`、`store/repo_retrieval.go` |
| D-2 动态工具注入（tier 三档 + promoteActive） | ✅ 完成 | `retrieval/selector.go`、`service/tool_selector.go`、`prompt/builder.go` |
| SOP 全量注入 + 上限 | ✅ 完成 | `prompt/builder.go` 的 `MaxSOP` 二次校验 |
| 配置契约收尾（禁静默兜底） | ✅ 完成 | 提交 `31fc2c9`（DynamicTopK / offline_after_s 硬校验） |
| **D-3 记忆四层** | ⬜ **未开始** | `internal/memory` 不存在 |
| **D-4 预算闸门五级降级** | ⬜ **未开始** | 见 §2 缺口 1 |

## 2. 剩余缺口（逐项可验证）

### 缺口 1：D-4 预算闸门五级降级未串成链路（优先级 P0）

**现象（实测，非推测）**：

- `retrieval.ClampTopK(opts, halve)`（`retrieval/selector.go:123`）已实现且
  `selector_test.go:98 TestClampTopK` 已覆盖，但**生产代码零调用** —— 全仓
  grep 仅命中定义与测试，属**死代码**。降级第二级「topK 减半」实际不生效。
- `prompt.BuildResult.OverBudget`（`builder.go:81`）只被 `builder_test.go` 消费，
  **service/agent 层没有任何降级或报错动作**。超出预算后程序照常把所有内容
  塞给模型 —— 注释写「上层决定压缩或明确报错，禁止静默截断」，但"上层"并不存在。

**设计要求的五级顺序**（`开发实施计划.md` §5 与项目记忆一致）：
①省略 L2/L3 记忆 → ②topK 减半 → ③常驻工具降到 4 → ④旧 tool_result 转引用 → ⑤强制压缩 → 仍超则**明确报错（严禁静默截断）**

**落位方案**：
- `internal/prompt/budget.go`：定义 `DegradeLevel` 五档枚举 + 单级降级**纯函数**
  （可单测，不依赖 store/llm）
- `internal/service/budget_gate.go`：把五级串成链路（service 同时依赖 prompt 与
  retrieval，是唯一合法落点；**prompt 不得 import retrieval**，否则内核反向依赖）
- `internal/config`：新增预算参数（`max_total` / `history_ratio` 等）+ 硬校验
- ④⑤ 涉及改历史消息与外部压缩，`prompt` 层只产出**明确信号**，由 service 决定
  调用压缩或报错（不在本轮接 LLM 压缩）

**验收**：`ClampTopK` 不再是死代码；构造超预算输入时按序降级；五级用尽仍超
→ 返回明确错误码，绝不静默放行。

### 缺口 2：D-3 记忆四层未开始（优先级 P0）

**现象**：`internal/memory` 目录不存在，L0~L3 零代码。

**已具备的前置条件**（可直接复用，勿重复造）：
- `retrieval.KindMemoryL2` / `KindMemoryL3` 常量已定义（`vector.go:26,28`）
- `retrieval_docs.kind` 注释已写明 `tool | memory_l2 | memory_l3`
  （`0003_retrieval.sql:18`），`UNIQUE(kind,key)` 与 `idx_retrieval_docs_kind` 就绪
- `retrieval_vectors`（float32 小端 BLOB）+ `Cosine` + `RRF` 可复用
- `last_ref_at` 字段已存在（**时间衰减打分直接用**）

**待实现**：
- L0 核心常驻（≤800 token，全量注入）
- L1 项目摘要（注入 3 行）
- L2 情景 digest（向量检索主战场）
- L3 档案（检索 + `path:line` 引用）
- 四分类去重（ADD/UPDATE/DELETE/NOOP，相似度 >0.92 判 NOOP）
- 检索打分 = `0.6*cos + 0.25*时间衰减(半衰 14d) + 0.15*重要性`，命中 <3 条不注入

**依赖**：缺口 1 的降级链第①级需要"可省略的记忆段"——两者应相互配合，
但**不互为阻塞**（D-4 可先用占位钩子实现第①级）。

### 缺口 3：`GET /api/v1/tools/injection` 实测断言缺失（优先级 P1）

计划 §5 要求 D 阶段出口门禁含「固定开销 ≤8k、cache_hit>60%、工具选择准确率
不低于全量基线」，当前只有端点存在性，无**注入预算明细的实测断言**。
待 D-3/D-4 落地后补 e2e。

## 3. 已核实的「伪待办」（勿再重复派发）

| 伪待办 | 核实结论 |
| --- | --- |
| 冻结 `RetryConfig` | 全仓零命中。`internal/` 唯一重试是 `llm/openai_compat.go` 的 `doWithRetry`（provider 私有行为，非可配置契约）。若确需 Engine 层重试，属**新功能提案**，不属阶段 D |
| 冻结 Engine 对外签名 | `Deps`(13 字段，逐项标注可否为 nil) / `RunRequest`(11 字段) / `New()`(5 必需依赖 + 4 默认值) / `Execute()` 注释已写明「error 只表示执行异常，终态以 DB 为准」——**已是完备契约**，无需改动 |
| 设备域 `DeviceConfig` 治理 | 实际名为 `DevicesConfig`，属**已完成的阶段 C**；其一致性校验已在 `31fc2c9` 补齐 |

## 4. 推进顺序

1. **D-4 预算闸门五级降级**（本轮，形成闭环止损：当前超预算静默放行是真实风险）
2. **D-3 记忆四层**（下一轮，体量大）
3. D 阶段出口门禁 e2e（上述完成后收口）

## 5. 风险与约束

- **`prompt` 禁止 import `retrieval`/`store`**（内核不得反向依赖适配器）；
  降级链只能放在 `service`
- **严禁静默截断**：五级降级每一级都必须记录实际动作，用尽后必须报错
- 本机无 CGO → 不能 `go test -race`，并发正确性靠人工审查 + 现有测试
- `web/dist/.gitkeep` 与 `.gitattributes` 机制不得破坏（干净检出可编译的前提）
