# harness_go

Web 版 AI Agent 服务：Go 单二进制（内嵌前端产物）+ React/Vite 前端。

---

## 一、数据模式（REAL / DEMO）：先看这个

前端有两类"假数据"，必须分清：

1. **走开关的假**：`api = apiMode === 'real' ? realApi : mockApi`；
2. **绕过开关的假**：个别组件直接 `import from '../mock/*'`，real 模式下也仍是演示数据
   —— 这类面板在 REAL 模式下会显示**黄色提示条**明示降级（`components/RealModeNotice.tsx`）。

### 1.1 `resolveApiMode()` 的三段默认分支

唯一实现在 `web/src/lib/api.ts:61-73`，优先级从高到低：

| 优先级 | 来源 | 说明 |
| --- | --- | --- |
| 1 | `VITE_API_MODE`（构建期注入） | 取值 `real` / `mock`，CI 与预览用 |
| 2 | `localStorage['harness.apiMode']` | 运行期手动切换（顶栏角标可一键切换） |
| 3 | 默认 | **开发态 `mock`**（可离线调 UI）/ **构建产物 `real`**（单二进制内嵌产物直连本机后端） |

### 1.2 三种切换方式

| 方式 | 做法 | 适用 |
| --- | --- | --- |
| 界面切换 | 点顶栏 🟢REAL / 🟡DEMO 角标 → 选目标模式（写 localStorage 后整页重载） | 日常辨别真假 |
| 开发态跑真实数据 | `cd web; npm run dev:real`（加载 `.env.real`，注入 `VITE_API_MODE=real`） | 验证接线是否真通 |
| 手动兜底 | 控制台 `localStorage.setItem('harness.apiMode','real')` 后刷新 | 调试 |

> 顶栏角标是**任何页面都能一眼可辨**的模式事实来源；顶栏的 SSE 状态胶囊读的是
> `lib/sse.ts` 真实状态机（连接 / 退避 / 补同步），无订阅时显示「未订阅」，不含任何编造指标。

---

## 二、本地开发与验证

```powershell
# 后端（默认监听 127.0.0.1:8080）
go run ./cmd/harness-go

# 前端（dev 默认 mock；要直连后端用 npm run dev:real）
cd web
npm install
npm run dev          # 演示基线
npm run dev:real     # 真实后端（vite proxy 把 /api、/healthz 转发到后端）
```

### 质量门禁（改完必须都跑）

```powershell
# 后端
gofmt -l .          # 期望无输出
go vet ./...
go test ./... -count=1        # 注意：Windows 本地易超 120s，建议分包跑

# 前端（两者必须同时过！）
cd web
npx tsc -b
npm run build       # ⚠️ tsconfig 的 include 仅含 src，vite.config.ts 不在 tsc 覆盖内，
                    #    其语法错误只在 esbuild 阶段暴露 → 只跑 tsc 会漏
```

---

## 三、文档索引

| 文档 | 用途 |
| --- | --- |
| `docs/功能现状与推进计划.md` | **功能真假三层判定**（能用 / 后端有前端无 / 已规划未开始）+ 后续推进计划 |
| `docs/开发实施计划.md` | 路线图 A~G + P1 的 WBS 与验收口径（v3.2） |
| `docs/架构设计文档.md` | 总体架构与客户端状态机 |
| `docs/protocols.md` | HTTP / SSE / 插件协议契约 |
| `docs/工程避坑清单.md` | 历史踩坑与门禁口径 |
