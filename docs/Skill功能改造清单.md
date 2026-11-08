# Skill 功能改造清单（二期 · 待开始）

> **状态：🕓 待开始**（未排期，未动工）
> **文档定位**：二期唯一权威范围基准。动工前须逐条核对，禁止范围漂移。
> **前置依赖**：一期「SOP 可编辑化重构」已交付（`internal/sop` 真源层 + `data/sop` 覆盖层 + 双模式面板 + 热生效 + 不播种铁律）。
> **生成日期**：2026-11-08

---

## 一、二期唯一核心目标

一期把 **SOP** 打造成了「出厂内置 + 运行时覆盖 + 界面可编辑 + 热生效」。
二期是把 **Skill 对齐到同一套模型**，让技能库也能像 SOP 一样被调教。

> ⚠️ 二期**不是新增功能**，而是**消除 SOP 与 Skill 之间的架构不对称**。
> 任何超出此边界的提案（技能市场、自动生成、依赖组合等）一律驳回，见 §七。

---

## 二、现状盘点（已勘察确认的事实）

### 2.1 现有实现

| 项 | 现状 | 证据 |
|---|---|---|
| 落点 | `<ws>/.harness/skills/<name>/SKILL.md` | `internal/plugins/skill/factory.go:16` |
| 工具面 | `skill_list` / `skill_read` / `skill_save` | `factory.go:30-32` |
| tier | list/read = `deferred`；save = `on_demand` | `manifest.json:14,21,28` |
| 命名校验 | `[a-z0-9-_]`，≤64 字符 | `skill.go:13-24` |
| 存储格式 | `# <name>\n\n<description>\n\n<body>\n` | `skill.go:88-89` |
| 读取方式 | **全部经 `host.Workspace()`** | `skill.go:28,46,65,90` |
| 出厂内置 | ❌ **无** | 仅有 `manifest.json` 的 `go:embed` |
| 运行时覆盖 | ❌ **无** | 单层目录 |
| 界面编辑 | ❌ **无**（`ToolsPanel` 只展示分类徽章） | `ToolsPanel.tsx:23,37` |
| REST 端点 | ❌ **无** | — |

### 2.2 与 SOP 的架构不对称（二期要消除的差异）

| | SOP（一期已完成） | Skill（二期目标） |
|---|---|---|
| 真源层 | ✅ `internal/sop`（零依赖） | ❌ 逻辑全在插件里 |
| 读取位置 | 内核装配点（`cmd` 包，**有完整 os 权限**） | **插件工具内部**（`skill.go:28,46,65`） |
| 能否读 `data/skills` | ✅ 能 | ❌ **不能** |
| 三层覆盖 | ✅ 内置 < 运行时 | ❌ 单层 |
| 界面编辑 | ✅ 双模式面板 | ❌ 无 |
| 不播种铁律 | ✅ 有断言锁定 | ❌ 无 |
| 跨项目隔离 | ✅ 按会话 `wsPath` 读 | ❌ 走固化 root，**跨项目串味** |

---

## 三、技术难点（二期必须先解决，绕不过去）

**Skill 与 SOP 的根本差异在于「读取发生的层」不同：**

```
SOP  →  cmd/harness-go/host_wiring.go 读取（有完整 os 权限，可直读 data/sop）
Skill → internal/plugins/skill/*.go 读取（插件内部，受 Host 沙箱约束）
```

阻塞条款（`pkg/toolapi/host.go`）：

- `host.go:13` —— **「插件拿不到 `*sql.DB`、拿不到原始 `llm.Provider`、拿不到 os 级任意文件访问」**
- `host.go:18` —— **「Workspace 受沙箱约束的工作区文件访问（根固定为项目级，不做会话级动态根）」**

→ 结论：**插件在现有接口下读不到 `data/skills`。必须新增可选扩展接口（F1）。**

---

## 四、功能改造清单

### F1. 新增可选扩展接口（地基，必须最先做）

在 `pkg/toolapi/host.go` **追加**可选扩展接口，**不改动 `Host` 主接口的九项定制**：

```go
// ProjectStore 项目级只读资源访问（技能库等）。
//
// 可选扩展：内核实装、插件按需类型断言；未实装时插件退回「仅工作区」视图。
// 与既有 SnapshotRegistrar / TaskNoteCheckpointer 同款模式，主接口零改动。
type ProjectStore interface {
    ListProjectSkills(ctx context.Context) ([]ProjectSkillEntry, error)
    ReadProjectSkill(ctx context.Context, name string) ([]byte, error)
}
```

**设计约束**：

- 只读（写侧仍走 `host.Workspace()` 沙箱，保持沙箱语义正确）。
- 已核实存在 4 个同款先例，不破例：`host.go:303 TaskNoteCheckpointer`、`host.go:308 TaskLister`、`host.go:313 TaskCheckpointReader`、`host.go:339 SnapshotRegistrar`。

**涉及文件**：

- `[MODIFY] pkg/toolapi/host.go`（追加接口 + 类型）
- `[MODIFY] internal/tools/hostimpl/host.go`（实现 + `SetProjectStore`）
- `[MODIFY] cmd/harness-go/main.go`（装配注入）
- `[NEW] pkg/toolapi/host_project_test.go` 或并入既有测试

---

### F2. `internal/skill` 真源层（对齐 `internal/sop`）

与 `internal/sop` **完全同构**，零外部依赖（仅标准库）：

```
internal/skill/
  ├─ builtin/<name>/SKILL.md     ← go:embed 出厂技能（可选，见 §六 待确认①）
  └─ skill.go                    ← Loader：LoadAll / Read / Save / Reset / Validate
```

**合并优先级**（建议，与 SOP 一致：更具体者优先）：

```
L0 内置（embed，只读，永不落盘）
      ↓ 被同名覆盖
L1 本机级 <dataDir>/skills/<name>/SKILL.md   （gitignore，本机私有）
      ↓ 被同名覆盖
L2 项目级 <ws>/.harness/skills/<name>/SKILL.md  （可 git 提交，团队共享）
```

**必须复用的一期铁律**：

- ✅ **绝不播种**：内置技能不得在启动/读取时写入磁盘。这是「升级二进制后永远最新」的唯一保证。需**断言锁定**（参照 `internal/sop/sop_test.go` 的 `TestLoadCoreNeverSeeds`）。
- ✅ **超限显式拒绝，绝不静默截断**。
- ✅ **「模式」由「文件是否存在」派生**，不新增独立状态字段，杜绝双真源漂移。

**涉及文件**：

- `[NEW] internal/skill/skill.go`（Loader + Validate + EstimateTokens 复用）
- `[NEW] internal/skill/embed.go`（`go:embed builtin/**`）
- `[NEW] internal/skill/builtin/<name>/SKILL.md`（视 §六 待确认① 而定）
- `[NEW] internal/skill/skill_test.go`（单测，含不播种断言）
- `[MODIFY] internal/plugins/skill/{factory.go,skill.go}`（**降薄为适配器**，`SkillsDir` 常量迁至 `internal/skill`，插件留别名）

---

### F3. 修跨项目串味（与一期同类 bug，二期必修）

二期若照抄现状，`skill_list`/`skill_read` 仍走 `host.Workspace()` 的**启动期固化 root**，跨项目切会话必然读到别的项目的技能。

**修法**（与一期 `sopBodies` 一致）：

- 插件侧的**沙箱写入**保留 `host.Workspace()`（语义正确）。
- **读取路径**改为「显式 `wsPath`」：由 `internal/skill.Loader` 接受显式绝对路径，经 F1 的 `ProjectStore` 注入。

**涉及文件**：

- `[MODIFY] internal/plugins/skill/skill.go`（读侧改走 Loader）
- `[MODIFY] internal/tools/hostimpl/host.go`（提供显式 wsPath 的只读访问）

---

### F4. REST 端点（5 个）

| 方法 | 路径 | 语义 |
|---|---|---|
| GET | `/api/v1/skills` | 列表：name / description / 来源（builtin\|global\|project）/ tokens |
| GET | `/api/v1/skills/{name}` | 读取全文 + 来源 |
| PUT | `/api/v1/skills/{name}` | 保存（新建/更新；首次保存即 fork 到 L1） |
| DELETE | `/api/v1/skills/{name}` | 重置：删除覆盖层回落内置；无内置则彻底删除（**须区分语义，UI 明示**） |
| GET | `/api/v1/skills/effective` | 生效预览（对齐 `/sop/effective`） |

**硬门禁（一期已踩过）**：

> ⚠️ 新增端点**必须同步** `api/openapi.yaml` 的 `paths` 与 `web/src/lib/apiTypes.ts` 的 `API_PATHS`，否则 `TestContractPathsNoDrift` 直接 FAIL。
> 一期实测该测试解析规则：openapi 侧匹配 `^  (/\S*):\s*$`（两空格缩进）；TS 侧解析 `API_PATHS` 常量块内的路径字面量。

**涉及文件**：

- `[NEW] internal/service/skill_service.go`
- `[NEW] internal/api/skill_handler.go`
- `[NEW] internal/api/skill_integration_test.go`
- `[MODIFY] internal/api/router.go`
- `[MODIFY] internal/api/handler.go`（`Deps` 加 `Skills` 字段，nil → unavailable）
- `[MODIFY] api/openapi.yaml`（5 paths + schemas）
- `[MODIFY] web/src/lib/apiTypes.ts`（`API_PATHS` + 类型）

---

### F5. 前端技能管理面板

在设置弹窗新增 `skills` 页签（或改造 `ToolsPanel` 的 skill 分类）：

- **列表**：名称 / 描述首行 / 来源徽章（内置·本机·项目）/ token
- **编辑**：Markdown 全文编辑器（**复用 `SopPanel.tsx` 的编辑模式**）
- **操作**：保存 / 重置 / 复制全文

**沿用一期双模式红线（必须）**：

| 动作 | 是否删文件 |
|---|---|
| 切到「内置」页签 | ❌ **绝不删**，只切只读预览 |
| 点「重置为内置」 | ✅ 删除 + 二次确认 + 明确文案 |

**涉及文件**：

- `[NEW] web/src/features/settings/SkillPanel.tsx`
- `[MODIFY] web/src/features/settings/SettingsModal.tsx`（挂载页签）
- `[MODIFY] web/src/lib/api.ts`（3 处：类型导入 / `ApiClient` 接口 / `realApi` + `mockApi` 实现）

> ⚠️ **一期血泪教训**：`api.ts` 有三处必须同步（导入、接口声明、两个实现），漏一处即 `npm run build` 失败。另：编辑该文件时**注意不要改动整体换行符**（一期曾因 `edit_file` 把 LF 改写成 CRLF，产生 609 行假 diff，需事后用 `[System.IO.File]::WriteAllText` + UTF8 无 BOM 修复）。

---

### F6. Skill token 预算与可观测

- 定义技能正文单条上限与**技能库总预算**（技能按需加载，但 `skill_list` 的描述首行会进上下文）。
- 新增配置项（对齐一期 `config.SOP.CoreMaxTokens`）：建议 `Skill.MaxTokens` / `Skill.MaxEntries`。
- **可观测**：`skill_list` 的描述索引应纳入 `Tools.Injection` 的 token 计量，界面上可见。
- 超限**显式拒绝不截断**。

**涉及文件**：

- `[MODIFY] internal/config/config.go`（+ `SkillConfig` + 校验 + env 绑定 + 测试用例）
- `[MODIFY] internal/tools/loader`（token 计量可见性）

---

## 五、文件改动总览（预估 18~23 个）

| 阶段 | 新增 | 修改 |
|---|---|---|
| F1 扩展接口 | 1 | 3 |
| F2 真源层 | 4 | 2 |
| F3 修串味 | 0 | 2 |
| F4 REST | 3 | 5 |
| F5 前端 | 1 | 3 |
| F6 预算 | 0 | 2~3 |

---

## 六、待你拍板的 3 个决策点（动工前必须确认）

| # | 决策 | 建议 |
|---|---|---|
| ① | **是否要出厂内置技能？** | 建议**不带**或只带 1 个极短的（如「代码审查」）。技能正文通常比 SOP 长，内置过多会挤占 `skill_list` 索引预算。也可**先留空壳目录，后续填充**。 |
| ② | **两层还是三层？** | 建议**三层**，与 SOP 完全一致（内置 / `data/skills` / `.harness/skills`）。若只要两层（内置 + 项目级），F2 可简化，但会失去「本机私有技能」能力。 |
| ③ | **是否复用一期不播种铁律？** | **强烈建议复用**。内置技能永不落盘，`data/skills` 存在即覆盖，删除即回落。 |

---

## 七、明确不做（二期排除项，防范围膨胀）

- ❌ 技能**版本历史 / 快照回滚**（SOP 一期也刻意没做，删除即还原已够）
- ❌ 技能**依赖 / 组合 / 继承**（会引入解析器，收益不明）
- ❌ 技能**市场分发**（已有 `market` 子系统，属 Package 域，**严禁混入**）
- ❌ 技能**自动生成**（属 growth 插件范畴）
- ❌ 技能**渐进披露**（`docs/功能现状与推进计划.md:299` 已列为「非缺口·严禁派发」）

---

## 八、验收标准（动工后按此自检）

### 8.1 自动化

```powershell
go build ./... ; go vet ./... ; go test ./...
go test ./internal/skill/... -v
go test ./internal/api/ -run TestSkill -v
cd web ; npm run build
```

### 8.2 必须成立的断言

1. **零回归**：未创建 `<dataDir>/skills/` 时，`skill_list`/`skill_read` 结果与改造前一致（单层 `.harness/skills` 场景）。
2. **不播种**：任何只读路径都**不得**把内置技能写入磁盘；测试断言只读操作后 `<dataDir>/skills` 目录不存在。
3. **热生效**：PUT 后同一会话下一轮 `skill_read` 即为新内容（无需重启）。
4. **重置即还原**：DELETE 后再 GET，`source == "builtin"` 且内容与 embed 一致。
5. **超预算拒绝**：超限返回 400 且**磁盘文件不被修改**（拒绝不截断）。
6. **模式切换不删数据**：切「内置」页签不产生任何写/删请求（前端断言）。
7. **跨项目隔离**：在 A 项目会话读到的技能，不得包含 B 项目 `.harness/skills` 的内容。
8. **契约零漂移**：`TestContractPathsNoDrift` 绿。

### 8.3 人工验收

- 界面新建/编辑技能 → 保存 → **不重启** → 下一轮对话中 `skill_read` 读到新内容。
- 点「重置为内置」→ 二次确认 → 内容回落。
- 「复制全文」→ 贴回 `internal/skill/builtin/` → 重编译 → 重启 → 成为新出厂。

---

## 九、附：一期可复用的成果与教训

### 可直接复用

| 资产 | 位置 |
|---|---|
| 同构 Loader 设计范式 | `internal/sop/sop.go` |
| 不播种铁律 + 断言 | `internal/sop/sop_test.go::TestLoadCoreNeverSeeds` |
| 双模式面板范式 | `web/src/features/settings/SopPanel.tsx` |
| service / handler / 集成测试范式 | `internal/service/sop_service.go`、`internal/api/sop_handler.go`、`sop_integration_test.go` |
| `.gitignore` 例外规则 | `!.harness/skills/` **一期已加，直接可用** |

### 必须规避的坑

| 坑 | 现象 | 规避 |
|---|---|---|
| `edit_file` 破坏 `api.ts` 换行符 | 产生数百行假 diff | 编辑后检查；必要时用 `[System.IO.File]::WriteAllText` + UTF8 无 BOM 修复 |
| `api.ts` 三处漏改 | `npm run build` 报 `Property ... does not exist on type 'ApiClient'` | 导入 / 接口声明 / real+mock 实现，**四处都要改** |
| `openapi.yaml` 多行编辑错位 | 不小心把下一行内容接到当前行 | 优先追加式写入，改动后立刻跑 `TestContractPathsNoDrift` |
| 写入侧拒绝、读取侧静默截断 | 同一系统两套矛盾策略 | 两端口径必须一致，丢弃量要可观测 |
| `host.Workspace()` root 固化 | 跨项目串味 | 读侧一律传显式 `wsPath` |

---

> **本清单为待开始状态。动工前须先确认 §六 的 3 个决策点，再调用 `create_plan` 生成正式实施方案。**
