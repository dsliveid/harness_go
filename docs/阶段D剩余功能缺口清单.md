# 阶段 D 剩余功能缺口清单（v1.1，2026-10-31 收口）

> 本文档是长任务「完成阶段D剩余功能」的执行锚点。每项缺口都可追溯到
> `docs/开发实施计划.md` v2.8 或具体提交，禁止凭印象新增或臆造功能。
> **v1.1：三个缺口全部关闭，阶段 D 完成**（本清单转为存档，勿再按缺口派发）。

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

## 1. 阶段 D 完成度总览（✅ 全部完成）

| 子项 | 状态 | 证据 |
| --- | --- | --- |
| D-1 检索基础设施（FTS5 trigram + RRF + 向量表） | ✅ 完成 | `0003_retrieval.sql`、`internal/retrieval/{vector,rrf,indexer}.go`、`store/repo_retrieval.go` |
| D-2 动态工具注入（tier 三档 + promoteActive） | ✅ 完成 | `retrieval/selector.go`、`service/tool_selector.go`、`prompt/builder.go` |
| SOP 全量注入 + 上限 | ✅ 完成 | `prompt/builder.go` 的 `MaxSOP` 二次校验 |
| 配置契约收尾（禁静默兜底） | ✅ 完成 | 提交 `31fc2c9`（DynamicTopK / offline_after_s 硬校验） |
| **D-4 预算闸门五级降级** | ✅ **完成（v1.1）** | 内核 `693be9c`（`prompt/budget.go`）+ 链路 `service/budget_gate.go` + 引擎 `budget_exhausted`（见 §2 缺口 1 收口记录） |
| **D-3 记忆四层** | ✅ **完成（v1.1）** | `internal/memory`（layer/classify/score/store）+ `retrieval.Kind` 扩展（见 §2 缺口 2 收口记录） |
| **D 出口门禁实测断言** | ✅ **完成（v1.1，部分项留待 F）** | `cmd/harness-go/d_exit_gate_test.go`（见 §2 缺口 3 收口记录） |

## 2. 缺口收口记录（v1.1）

### 缺口 1：D-4 预算闸门五级降级 —— ✅ 已关闭

**落位（与原方案一致）**：
- `internal/prompt/budget.go`（内核，提交 `693be9c`）：`DegradeLevel` 六档 + `PlanDegrade`
  逐档规划纯函数 + `Exhausted` 语义，12 个单测。
- `internal/service/budget_gate.go`（链路编排）：`BudgetGate` 实现 `agent.ContextBuilder`
  包装真实 Builder —— 「应用一档 → 重新 Build 计数 → 再规划下一档」循环；
  应用动作全部落在 **Input 副本**（不污染引擎跨步复用的 history）。
- `internal/agent/engine.go`：`OverBudget=true` → Run 以 `budget_exhausted` **明确失败**
  （provider 零调用）；每档动作发非致命 `error` 事件（scope=budget_gate）。
- `internal/config`：`budget.max_total`（默认 0=不限，闸门直通零开销）+
  `budget.history_ratio`（默认 0.6）+ 硬校验 + env 绑定。

**五级实现口径**：①省 L2/L3 记忆（`MemoryTail` 置空，L0 不省）→ ②topK 减半
（下限 `DynamicMinTopK`，绝不砍到 0）→ ③常驻工具降到 4（保序截断，宽进严出）→
④旧 tool_result 转引用（保留最近 4 条 + 短结果）→ ⑤强制压缩（本阶段无压缩器，
**不做假开关配置**，信号位已预留）→ 用尽即明确报错。

**验收结果**：`service/budget_gate_test.go` 12 测试（直通/顺序严格递增/用尽明确报错/
无动作不重建/不改调用方 Input/topK 下限/折叠口径/裁剪保序/汇总可读）全 PASS；
`agent/engine_budget_test.go` 4 测试（失败码/降级事件/记忆透传/零值回归）全 PASS；
五级用尽仍超 → `budget_exhausted`，**绝不静默放行**。

### 缺口 2：D-3 记忆四层 —— ✅ 已关闭

**落位**：新建 `internal/memory`（零 store 依赖，`Repo` 端口与 `*store.RetrievalRepo`
签名逐一对应，装配点零适配）：
- L0 核心常驻（≤800 token，超限按写入序丢弃尾部）/ L1 项目摘要（前 3 行非空文本）/
  L2 情景 digest（向量检索主战场）/ L3 档案（检索 + 来源 key 引用）。
- 四层全部落主库 `retrieval_docs`（`retrieval.Kind` 扩展 `memory_l0`/`memory_l1`；
  0003 迁移的 kind 列无 CHECK 约束，**零新增迁移**）；向量复用 `retrieval_vectors`。
- 四分类去重：>0.92 判 NOOP（刷 last_ref_at）、0.80~0.92 判 UPDATE（覆盖到旧键，
  mem0 语义）、其余 ADD；DELETE 须上层语义判定（contradicts）后显式调用，
  本层绝不臆测删数据。
- 打分 = `0.6*cos + 0.25*衰减(半衰14d) + 0.15*重要性`；命中 <3 整层不注入；
  embedding 未配置时向量通道诚实停用（cos 计 0），无查询不检索。
- 接线：`service.MemorySource` → RunService 每 Run 召回（失败降级空块不阻断）→
  `RunRequest.MemoryCore/MemoryTail` → 引擎透传 → prompt（MemoryCore 进 system
  稳定前缀，MemoryTail 置尾）。

**验收结果**：`memory/classify_test.go` + `score_test.go` + `store_test.go`（fake repo）
共 17 测试全 PASS（四分类阈值边界/矛盾优先/L0 预算/L1 三行/最少命中/来源引用/
召回刷新 last_ref_at/仓储错误上抛）；`prompt/memory_injection_test.go` 位置铁律 +
空字段零回归 4 测试全 PASS。

### 缺口 3：D 出口门禁实测断言 —— ✅ 已关闭（可机器断言部分）

**落位**：`cmd/harness-go/d_exit_gate_test.go` 按真实装配链（真实注册表 + echo 插件 +
真实 Builder + 预算闸门）断言：
- 固定开销（零历史/零输入 prompt 总额）**≤8000 token**，且默认装配（max_total=0）
  不触发降级；
- `/api/v1/tools/injection` 的预算明细自洽（cost_total = cost_core + cost_index）
  且在门禁内、字段齐备（端点是 `loader.Injection` 直通视图，服务层断言即端点断言）。

**留待 F 阶段收口的两个评测项**（需真实 LLM 评测集，当前内置工具仅 echo 无评测意义）：
`cache_hit>60%` 与「工具选择准确率不低于全量基线」。已在计划文档「下一步」标注。

## 3. 已核实的「伪待办」（勿再重复派发）

| 伪待办 | 核实结论 |
| --- | --- |
| 冻结 `RetryConfig` | 全仓零命中。`internal/` 唯一重试是 `llm/openai_compat.go` 的 `doWithRetry`（provider 私有行为，非可配置契约）。若确需 Engine 层重试，属**新功能提案**，不属阶段 D |
| 冻结 Engine 对外签名 | `Deps`(13 字段，逐项标注可否为 nil) / `RunRequest`(11 字段) / `New()`(5 必需依赖 + 4 默认值) / `Execute()` 注释已写明「error 只表示执行异常，终态以 DB 为准」——**已是完备契约**，无需改动 |
| 设备域 `DeviceConfig` 治理 | 实际名为 `DevicesConfig`，属**已完成的阶段 C**；其一致性校验已在 `31fc2c9` 补齐 |
| 「`ClampTopK` 是死代码」 | v1.0 记录有误：`service/tool_selector.go` 自 D-2（`cf74f4b`）起即经 `HalveTopK` 开关调用它；真实缺口是**开关恒 false 且无动态降级** —— 已由 D-4 的 `BudgetGate` 第②级闭环 |

## 4. 推进顺序（已全部执行完毕）

1. ~~D-4 预算闸门五级降级~~ ✅（内核 `693be9c` → 链路 + 引擎 + 配置）
2. ~~D-3 记忆四层~~ ✅（`internal/memory` + Kind 扩展 + service/main 接线）
3. ~~D 阶段出口门禁 e2e~~ ✅（固定开销 ≤8k + 注入明细一致性；评测项留待 F）

## 5. 风险与约束（对后续阶段仍然有效）

- **`prompt` 禁止 import `retrieval`/`store`**（内核不得反向依赖适配器）；
  降级链只放在 `service`（`BudgetGate` 是唯一编排点）
- **严禁静默截断**：五级降级每一级都记录实际动作（事件 scope=budget_gate），
  用尽后必须报错（`budget_exhausted`）
- 本机无 CGO → 不能 `go test -race`，并发正确性靠人工审查 + 现有测试
- `web/dist/.gitkeep` 与 `.gitattributes` 机制不得破坏（干净检出可编译的前提）
- 已发布的迁移文件永不修改：`retrieval.Kind` 扩展 L0/L1 走「kind 无 CHECK 约束」
  的既有事实，不新增迁移（`0004` 留给阶段 E 的 `tool_packages`）
