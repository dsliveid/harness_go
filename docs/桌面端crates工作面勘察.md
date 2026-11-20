# 桌面端 crates 工作面勘察（只读，2026-11 实测）

- **勘察对象**：`F:\WorkSpace\Other\harness_desktop\crates`（hd-core / hd-host / hd-protocol / hd-runner，附带工作区成员 hd-testkit）
- **HEAD commit**：`7cf58b6` — `fix(hd-host): 配对申报改引用协议层唯一真相源`（2026-11-20）
- **勘察日期**：2026-11-20
- **任务性质**：纯只读勘察。未修改任何 `.rs` / `.json` / `.toml`，未执行 `git add` / `git commit`。唯一产出文件为本 Markdown。
- **统计口径**：`Get-ChildItem crates -Recurse -Include *.rs`，**排除 `target/` 构建产物**；行数按物理行（`Measure-Object -Line`）真实跑出，非估算。
- **注意**：`src-tauri/` 目录在本仓**不存在**（实测 `Get-Content src-tauri\Cargo.toml` → PathNotFound）。桌面端壳即 `crates/hd-host`，顶层仅 `Cargo.toml`（workspace）+ `web/` + `docs/` + `scripts/`。

---

## 一、定位前提

**桌面端只做两件事：配对（配服务端地址 + 票据换取）与承载服务端 React Web 页面；桌面端自有 UI 仅内置基线设置页（`web/` 目录副本，约 4 文件 33.6KB）。** 因此 crates 的正当职责只剩三条线：**配对/会话**、**进程托管（Runner 子进程生命周期）**、**窗口导航与协议编解码**。任何超出这三条线的端侧执行能力都属于「待核准外延」，不构成自动立项理由。

---

## 二、crate 职责与规模总表

| crate | 职责（源码证据） | 文件数 | 总行数 | 关键文件（最大 3 个） |
|---|---|---|---|---|
| **hd-core** | 端侧执行面地基：执行器抽象 + 安全闸门。`src/lib.rs:1-15` 自述「唯一能触达执行面的领域层」「无网络依赖」「结果恒不可信」。出口 `Guarded<E>` / `Policy` / `AuditSink` | 7 | 1,573 | `src/security/guard.rs` 605、`src/executor/fs.rs` 410、`src/executor/mod.rs` 254 |
| **hd-protocol** | Go 侧 `internal/device/protocol.go` 的 Rust 镜像，**唯一职责**是把线上 JSON 映射成强类型结构体（`src/lib.rs:1-16`）。无 IO/网络/运行时依赖 | 8 | 1,178 | `src/payload.rs` 268、`src/envelope.rs` 219、`src/exit_code.rs` 155 |
| **hd-runner** | 执行引擎：WS 连接、welcome 握手、心跳、重连、IPC、任务派发路由、幂等回放、`fs.list` 帧编解 | 17 | 6,075 | `src/supervisor.rs` 1,495、`src/router.rs` 702、`src/idempot.rs` 560 |
| **hd-host** | 桌面宿主壳（Tauri 2）：令牌保管（vault）、Runner 守护（daemon/supervisor）、对外 command 面（commands）、配对客户端（pairing）、GUI 组装（gui，feature 可选） | 19 | 10,965 | `src/commands.rs` 3,280、`src/daemon.rs` 1,185、`src/pairing.rs` 1,098 |
| （hd-testkit） | 假服务端：本地 WS 回环，用于服务端 L1 未落地前验证端侧协议 | 1 | 205 | `src/lib.rs` 205 |
| **合计（四 crate）** | — | **51** | **19,791** | — |

> 合计含 `src/` 与 `tests/`；不含 hd-testkit（205 行）。

### 逐文件行数底账（真实跑出）

**hd-core（7 文件 / 1,573 行）**
`src/lib.rs` 22 · `src/executor/mod.rs` 254 · `src/executor/fs.rs` 410 · `src/security/mod.rs` 8 · `src/security/guard.rs` 605 · `src/security/paths.rs` 120 · `src/security/redact.rs` 154

**hd-protocol（8 文件 / 1,178 行）**
`src/lib.rs` 35 · `src/capability.rs` 66 · `src/error_code.rs` 70 · `src/envelope.rs` 219 · `src/exit_code.rs` 155 · `src/payload.rs` 268 · `tests/fs_list_golden.rs` 139 · `tests/golden_fixtures.rs` 226

**hd-runner（17 文件 / 6,075 行）**
`src/lib.rs` 40 · `src/capabilities.rs` 270 · `src/cli.rs` 335 · `src/frames/mod.rs` 101 · `src/frames/fs_list.rs` 512 · `src/heartbeat.rs` 190 · `src/idempot.rs` 560 · `src/ipc.rs` 368 · `src/progress.rs` 103 · `src/reconnect.rs` 127 · `src/router.rs` 702 · `src/supervisor.rs` 1,495 · `src/transport.rs` 344 · `tests/ipc_e2e.rs` 80 · `tests/router_e2e.rs` 467 · `tests/supervisor_e2e.rs` 162 · `tests/transport_e2e.rs` 219

**hd-host（19 文件 / 10,965 行）**
`build.rs` 97 · `src/lib.rs` 50 · `src/main.rs` 144 · `src/bin/fake_runner.rs` 165 · `src/boot_state.rs` 200 · `src/bootstrap.rs` 692 · `src/commands.rs` 3,280 · `src/daemon.rs` 1,185 · `src/gui.rs` 946 · `src/instance_lock.rs` 151 · `src/lifecycle.rs` 482 · `src/pairing.rs` 1,098 · `src/profile.rs` 265 · `src/supervisor.rs` 355 · `src/vault.rs` 604 · `tests/bootstrap_e2e.rs` 192 · `tests/config_guard.rs` 329 · `tests/daemon_e2e.rs` 303 · `tests/supervise_e2e.rs` 427

---

## 三、逐 crate 详情

### 3.1 hd-core —— 执行面地基（无网络、无 DB）

**入口/出口**（`src/lib.rs:17-24`）：
```rust
pub use executor::fs::{list_dirs, DirEntry, DirListing, ListDirsExecutor, FS_MAX_DIRS};
pub use executor::{ExecCtx, ExecError, ExecOutput, Executor, Limits, NoopSink, ProgressSink, SourceTrust};
pub use security::guard::{AuditEvent, AuditOutcome, AuditSink, Guarded, NullAuditSink, Policy};
```

**设计约束写进类型系统**（`src/lib.rs:6-14`）：依赖树无 reqwest / tokio-tungstenite，tokio 不启 `net` feature；`SourceTrust` 只有 `Untrusted` 一个变体（端侧结果恒不可信，构造不出「可信」）；审计走 `AuditSink` 本地 sink；**router 只持有 `Guarded<E>`，拿不到裸 executor**（`src/lib.rs:13-14`）。

**完成度**：已实现 `fs.list`（`executor/fs.rs` 410 行，`list_dirs`）、路径白名单闸门（`security/paths.rs` 120 行）、令牌打码（`security/redact.rs` 154 行，含 84 行 `panic!("期望字符串…")` 仅测试断言）、`Guard` 605 行主逻辑含单测。
**未实现**：`fs.glob` / `fs.grep` / 原子写 —— `Cargo.toml:26-31` 明确写「以下按设计文档 §14.3 预留（后续 fs.glob / fs.grep / 原子写要用），**当前未使用**」，依赖为 `walkdir` / `globset` / `tempfile` / `dirs`。

**被谁调用**：`hd-runner`（`src/router.rs:25`、`src/capabilities.rs:175,194`、`src/cli.rs:203-209`、`src/progress.rs:34`）。hd-host 不直接依赖 hd-core 执行器（只经 runner 侧）。

### 3.2 hd-protocol —— 线格式镜像

**出口**（`src/lib.rs:24-37`）：`capability_for_tool` / `WORKSPACE_LIST`、`Envelope` / `MessageType` / `compatible_version` / `CURRENT_PROTOCOL_VERSION` / `MIN_PROTOCOL_VERSION`、7 个 `EXIT_*` 常量 + `requires_repair` / `should_restart`、以及全部 payload（`HelloPayload` / `WelcomePayload` / `TaskDispatchPayload` / `TaskResultPayload` / `TaskProgressPayload` / `FsListPayload` / `FsListResultPayload` / `LeaseRevokedPayload` / `ErrorPayload` / `PingPayload` / `PongPayload` / `Capability` / `SourceTrust`）。

**为什么必须存在而非复用**（`src/lib.rs:7-11` 原文理由）：Go `internal/device` 是 internal 包，跨模块禁止 import；服务端二进制 `//go:embed web/dist` 无法瘦身成客户端 → 协议在 Rust 侧重写，用 `fixtures/` 黄金样本（`tests/golden_fixtures.rs` 226 行、`tests/fs_list_golden.rs` 139 行）锁死漂移。

**向后兼容铁律**（`src/lib.rs:13-16`）：只允许新增字段，所有 payload 字段带 `#[serde(default)]`。

**完成度**：骨架完整、零未完成标记。**这是唯一「不可删」的 crate**——配对申报与 fs.list 帧都要经它（HEAD 提交本身就是「配对申报改引用协议层唯一真相源」）。

### 3.3 hd-runner —— 执行引擎

**出口**（`src/lib.rs:26-42`）：`run_cli` / `EXIT_NO_AUTH`、`capabilities_hash` / `capabilities_of` / `capabilities_snapshot`、`error_envelope` / `fs_list_result_envelope` / `FrameOutcome`、`HeartbeatPolicy`、`ClaimGuard` / `ClaimResult` / `IdempotStore`、`parse_auth_first_line` / `parse_command` / `IpcCommand` / `IpcReader`、`ChannelSink`、`ReconnectDecision` / `ReconnectPolicy`、`Router` / `CollectingSink` / `STATUS_SUCCESS` / `STATUS_FAILED` / `CODE_DEVICE_TIMEOUT`、`Supervisor` / `WsConnector` / `TokioSleeper` / `StopReason` / `FS_FRAME_MAX_CONCURRENCY`、`Connection` / `Session` / `TransportError`。

**模块分工**：`transport.rs`(344) WS 连接与 Session；`supervisor.rs`(1,495) 守护主循环（最大文件，含重连/心跳/派发编排）；`heartbeat.rs`(190) 心跳节奏（间隔必须来自 welcome，被钉死在单测）；`reconnect.rs`(127) 退避决策；`ipc.rs`(368) stdin/stdout 控制面（首行 auth 握手）；`router.rs`(702) 工具派发 + 安全闸门接入 + 幂等回放；`idempot.rs`(560) TTL 幂等存储与并发占位（`claim_guard` 单锁内「查缓存+占位」）；`frames/fs_list.rs`(512) fs.list 帧编解；`capabilities.rs`(270) 能力快照与执行器注册（`ListDirsExecutor`）。

**完成度**：**优于其自身文档**。`src/lib.rs:3-5` 仍写「当前阶段（P2）已实现：WS 连接、welcome 握手、心跳节奏驱动、重连决策。尚未实现（P3+）：任务派发路由、执行器、安全闸门」——但实测 `router.rs` 702 行、`frames/fs_list.rs` 512 行、`capabilities.rs` 270 行均已落地，且 HEAD 前一提交 `fcf9dbe feat(hd-protocol,hd-runner): 接通 fs.list 帧链路 —— 目录列举端到端可用`。**该段注释是仓库内唯一一处「自述未实现但实际已实现」的失真源**（见第四节 C-1）。

**被谁调用**：`hd-host` 复用 `reconnect::ReconnectPolicy/ReconnectDecision`（`bootstrap.rs:674`、`daemon.rs:39,881`、`supervisor.rs:35,76,88`、`gui.rs:143,949`、`commands.rs:643`）、`heartbeat::is_heartbeat_control_line` / `heartbeat_control_line`（`daemon.rs:856,1227,1297`）、`ipc::redact_token`（`pairing.rs:26`、`vault.rs:70`）、`run_cli`（`main.rs:54`）。**依赖方向 hd-host → hd-runner，无循环。**

### 3.4 hd-host —— 宿主壳（唯一「配对 + 承载 Web」的执行体）

**出口**（`src/lib.rs:36-53`）：
- `boot_state`: `plan_boot` / `BootStateKind` / `BootUiState`
- `bootstrap`: `default_ws_url` / `needs_pairing` / `Bootstrap` / `BootstrapOutcome`
- `commands`: `device_status` / `get_workspace_roots` / `list_commands` / `runner_status` / `set_workspace_roots` / `start_pairing` / `FORBIDDEN_COMMANDS` / `SUPPORTED_COMMANDS`
- `daemon`: `locate_runner` / `Daemon` / `build_auth_line` / `build_reload_policy_line` / `LineRing` / `RuntimeStats` / `StdinHandle`
- `pairing`: `ws_url_from` / `PairedDevice` / `PairingClient`
- `profile`: `FileProfileStore` / `MemoryProfileStore` / `Profile`
- `vault`: `default_vault` / `DeviceToken` / `PlatformVault` / `TokenVault` / `KEYRING_SERVICE`

**关键职责与证据**：
1. **配对面**（核心定位）：`pairing.rs`(1,098) `PairingClient` + `commands.rs` 的 `start_pairing`(2091) / `start_pairing_real`(2169) / `validate_pair_input`(2136) / `normalize_server_base_url`(1512) / `set_server_base_url`(1597) / `core_sign_in`(1089) / `sign_out`(1046) / `reinject_session_cookie`(887) / `unpair_device`(1388) / `converge_revoked_to_pairing`(1764) / `handle_terminal_revoked`(1808)。
2. **进程托管**：`daemon.rs`(1,185) `locate_runner`(345) / `Daemon::spawn`(588)（`Command::new(exe).spawn()` 617-633，pump stdout/stderr 656-665）；`supervisor.rs`(355) 守护循环；`lifecycle.rs`(482) `RunnerPhase::allows_spawn`(129) 防并发重复起进程；`instance_lock.rs`(151) 单实例锁。
3. **窗口导航**：`gui.rs`(946) `open_baseline`(567) + `baseline_url_snapshot`(598)（快照导航同源维护，注释 596-597）+ 托盘/菜单事件路由(799-805)；`commands.rs:601-607` 的 `HostState.baseline_url` 为「GUI setup 写入、`open_baseline` 消费」的跨层快照。`plan_web_ui_navigation`(1313) / `open_web_ui_target`(1323) 决定「配对后进 Web UI」。
4. **工作区根目录**：`set_workspace_roots`(1935) / `set_workspace_roots_and_push`(2004) / `classify_push_outcome`(2035) / `is_runner_not_running`(2074) → 直接喂 `hd-core::Policy` 白名单。
5. **GUI 可选 feature**：`Cargo.toml:9` `gui = ["dep:tauri", "dep:tauri-plugin-single-instance"]`；`Cargo.toml:19-26` 记录「漏 `custom-protocol` 会导致 release 构建成功但双击闪退」的血训，并把 `release = gui + custom-protocol` 写成惯例。

---

## 四、未完成标记清单（文件:行号 + 原文）

勘察手段：`Select-String` 对 `*.rs` 匹配 `TODO|FIXME|unimplemented!|todo!|not implemented|未实现|占位|待办|placeholder|stub`。

### A. 真实 `unimplemented!()` / `todo!()` 宏
**零命中。** 全仓 crates 下不存在 `unimplemented!()` 或 `todo!()` 宏调用，也不存在 `TODO` / `FIXME` 标记。**结论：不存在「写了一半留空壳」的函数。**

### B. 设计性「未实现」提示（非缺陷）
| 位置 | 原文 | 判定 |
|---|---|---|
| `hd-runner/src/router.rs:228,234` | `"收到未实现工具的派发，已回 failed"` / `"未实现该工具: {}（端侧未注册对应 executor）"` | **设计行为**，失败收敛而非 panic；`tests/router_e2e.rs:367` 与 `router.rs:550` 用断言钉死该文案 |
| `hd-host/src/commands.rs:1201` | `// 未实现的 start_pairing 之外的「禁止实现」三兄弟永不出现（见模块文档）。` | 禁止清单（`FORBIDDEN_COMMANDS`）的注释说明 |
| `hd-host/src/commands.rs:2089` | `/// 前端调它不会拿到「command 不存在」的异常。而「永远只返回 NotImplemented」的…` | 接口契约说明 |
| `hd-runner/src/supervisor.rs:1605` | `// 服务端新增、本端未实现的帧` | 前向兼容丢弃逻辑 |

### C. 自述与实际矛盾（**唯一需处理项**）
| 位置 | 原文 | 实测 |
|---|---|---|
| `hd-runner/src/lib.rs:4-5` | `//! 尚未实现（P3+）：任务派发路由、执行器、安全闸门 —— 见 docs/桌面端架构与功能设计.md §19。` | **已实现**：`router.rs` 702 行（派发+闸门接入）、`frames/fs_list.rs` 512 行（执行器落地）、`capabilities.rs` 270 行（`ListDirsExecutor` 注册）；且 `fcf9dbe` 提交信息为「接通 fs.list 帧链路 —— 目录列举端到端可用」 |

### D. 依赖层「预留未使用」
| 位置 | 原文 |
|---|---|
| `hd-core/Cargo.toml:26` | `# 以下按设计文档 §14.3 预留（后续 fs.glob / fs.grep / 原子写要用），当前未使用但不算重型依赖` |
| 同上 27-30 | `walkdir = "2"` / `globset = "0.4"` / `tempfile = "3"` / `dirs = "5"` |

其余 grep 命中的 `xxx` 全部落在文档注释/测试断言/URL 示例中（如 `redact.rs:68`、`pairing.rs:786-787,943`、`vault.rs:433`、`ipc.rs:19`、`commands.rs:1558`），**非未完成标记**。

---

## 五、死代码与半成品判定

| 项 | 判定 | 依据 |
|---|---|---|
| `hd-core` 的 `walkdir`/`globset`/`tempfile`/`dirs` 依赖 | **确认未使用**（可删，或随 `fs.glob` 立项一并激活） | `hd-core/Cargo.toml:26` 自述「当前未使用」；`fs.rs` 仅实现 `list_dirs` |
| `hd-host/src/bin/fake_runner.rs`(165) | **测试支撑活代码**，非死代码 | 被 `daemon.rs:1128-1134` `locate_runner_prefers_explicit_path` 等单测引用 |
| `hd-testkit`(205) | **半成品但有意为之** | `Cargo.toml` 自述「用于在服务端 L1（fs.* 派发）未落地前验证端侧协议」；L1 已在 `fcf9dbe` 接通 → **其存在价值需重新核准**（见六档 O-2） |
| `commands.rs` 内 `StubGateway`(2870) / `StubUnpairGateway`(3278) | **测试桩**，非生产死代码 | 均在 `mod tests`(2275) 内 |
| `hd-core::SourceTrust` | **活代码（类型学约束）** | 仅 `Untrusted` 变体，被 `executor` / `payload` 双向使用，作用是「构造不出可信」 |
| `hd-runner::progress::ChannelSink` | **活代码** | `router.rs` 派发进度回传使用；`progress.rs:34` 引用 `hd_core::ProgressSink` |
| `hd-host::boot_state` / `BaselineStage` | **活代码** | `commands.rs:1171 plan_baseline_stage` 有 4 条断言（`commands.rs:2410-2416`），对应基线页三态流 |
| `hd-host::check_update`(1259) / `log_dir`(1277) | **边界模糊** | 属壳自身运维能力，与「配对+承载 Web」定位无直接关系但成本极低（< 40 行） |

**关键反面结论**：**不存在「pub 但零调用方」的大块模块**。四个 crate 的 `lib.rs` 重导出面逐一可在另一个 crate 或自身测试中找到消费方（证据：`hd_core::` 36 处、`hd_runner::` 31 处引用，见第三节各条 `被谁调用`）。这与移动端 R1/桌面端 R2 的教训方向一致：**本仓没有「先写内核后补 UI」的堆叠产物**。

---

## 六、剩余工作量三档归类

> 刻意不排期。每项仅给「判据」，供主进程按优先级自行决定取舍。

### 🔴 必须做（3 项）

| # | 事项 | 依据 | 成本估算 |
|---|---|---|---|
| **M-1** | 修正 `hd-runner/src/lib.rs:3-5` 过时的「尚未实现（P3+）」段，改为如实描述 router / frames / capabilities 已落地 | 第四节 C-1：全仓唯一「自述与实际矛盾」项；下一轮任何人读该 crate 都会误判工作量 | 改 3 行注释 |
| **M-2** | 核准 release 构建口径 `--features gui,custom-protocol` 是否已固化进 `scripts/` 与 `build.cmd/build.sh` | `hd-host/Cargo.toml:19-26` 明写「曾漏掉它，`hd-host.exe` release 构建成功但双击即死，且 `windows_subsystem="windows"` 把报错吞掉」。这是**桌面端唯一能上线的前提**，漏一次等于交付失败 | 读 3 个脚本文件，必要时补一行 |
| **M-3** | 端到端核准 `fs.list` 链路在真实服务端上可用（`cargo test -p hd-runner -p hd-protocol`，含 `tests/golden_fixtures.rs` / `tests/fs_list_golden.rs` 黄金样本） | `fcf9dbe` 刚接通该链路，属**最新改动面**；协议镜像的唯一门禁就是这两个 golden 测试 | 跑测试，不改码 |

### 🟡 可选增强（4 项）

| # | 事项 | 依据 | 判据 |
|---|---|---|---|
| **O-1** | `fs.glob` / `fs.grep` / 原子写执行器 | `hd-core/Cargo.toml:26-30` 已预留 `walkdir`/`globset`/`tempfile` | **必须先核服务端是否有对应 L1 派发端点与 Web 消费方**（否则重演「先写内核后补 UI」）。当前 Web 侧只见到目录列举（`DirectoryPicker`） |
| **O-2** | 重新界定 `hd-testkit` 的去留 | 其立项理由「服务端 L1 未落地前验证端侧协议」已被 `fcf9dbe` 消解；但 `eps` e2e 仍依赖假服务端 | 若真实服务端已能覆盖 `transport_e2e`/`supervisor_e2e` 场景 → 降级为 local-only；否则保留 |
| **O-3** | `hd-host/src/commands.rs`(3,280 行) 按域拆分（配对 / 会话续签 / 工作区 / 设备状态 / 解绑） | 单文件占 hd-host 全 crate 29.9%；已天然按 `// ──` 分段且含 60+ 单测 | 纯可读性收益，零功能收益 |
| **O-4** | `FS_FRAME_MAX_CONCURRENCY` 并发上限的压测 | `hd-runner/src/supervisor.rs` 定义 `FS_FRAME_MAX_CONCURRENCY` / `DEFAULT_MAX_CONCURRENCY` | 单测已覆盖语义，压测仅在真实设备慢盘下才有意义 |

### ⚫ 可删（2 项，均不改变功能）

| # | 事项 | 依据 | 风险 |
|---|---|---|---|
| **D-1** | `hd-core/Cargo.toml` 的 `walkdir` / `globset` / `tempfile` / `dirs` 四个未使用依赖 | 第四节 D 表（自述「当前未使用」）；workspace `Cargo.toml:17-22` 设 `opt-level="z"` + `lto` + `panic="abort"` + `strip`，说明体积是显式预算项 | 若近期要做 O-1 则先别删；删除后 `cargo check -p hd-core` 必须仍绿 |
| **D-2** | `hd-core/Cargo.toml` 顶部「依赖红线」注释可保留，但 `hd-host/src/bin/fake_runner.rs` 与 `hd-testkit` 的职责重叠部分应收敛为一份 | 两者都在「造假服务端/假 runner」 | 需先确认测试不跨依赖，收敛属重构 |

> **明确不属于上述三档**：`hd-protocol` 全量、`hd-host` 的 pairing/session/unpair/set_server_base_url、`daemon`+`supervisor`+`lifecycle`+`instance_lock`、`gui::open_baseline` 与基线页导航、`hd-runner` 的 `transport`/`heartbeat`/`reconnect`/`ipc`/`idempot`/`router` —— 这些是既定定位下的**必须保留骨架**（见第七节）。

---

## 七、结论与建议（只给事实与判据，不预设排期）

### 7.1 「必须保留的骨架」——删任何一条都会让桌面端失去既定功能

```mermaid
graph TD
    subgraph 配对面[配对面：桌面端核心职责]
      A["gui::open_baseline + baseline_url 快照<br/>gui.rs:567/598"] --> B["基线页（web/ 副本）<br/>填服务端地址 + 配对码"]
      B --> C["commands::normalize_server_base_url<br/>commands.rs:1512"]
      C --> D["commands::start_pairing_real<br/>commands.rs:2169"]
      D --> E["pairing::PairingClient<br/>pairing.rs 1098 行"]
      E --> F["vault::PlatformVault 落票据<br/>vault.rs 604 行"]
      F --> G["reinject_session_cookie<br/>commands.rs:887（免登录）"]
    end
    subgraph 托管面[进程托管面]
      H["bootstrap::Bootstrap<br/>bootstrap.rs 692 行"] --> I["daemon::locate_runner<br/>daemon.rs:345"]
      I --> J["Daemon::spawn 子进程<br/>daemon.rs:588"]
      J --> K["lifecycle::allows_spawn 防重复起<br/>lifecycle.rs:129"]
      J --> L["supervisor 守护循环<br/>supervisor.rs 355 行"]
      G --> H
    end
    subgraph 执行面[执行面：经 stdio IPC]
      J --> M["hd-runner::ipc 首行 auth<br/>ipc.rs 368 行"]
      M --> N["hd-runner::transport WS<br/>transport.rs 344 行"]
      N --> O["hd-runner::router 派发<br/>router.rs 702 行"]
      O --> P["hd-core::Guarded + Policy<br/>guard.rs 605 行"]
      P --> Q["fs.list 执行器<br/>executor/fs.rs 410 行"]
    end
    N -.协议编解码.-> R["hd-protocol 线格式<br/>envelope/payload/exit_code"]
    Q -.fs.list 帧.-> S["hd-runner::frames::fs_list<br/>frames/fs_list.rs 512 行"]
    S -.-> R
```

**必须保留清单（附依据）**：
1. **hd-protocol 全量**（1,178 行）——配对申报与 fs.list 帧的唯一真相源（HEAD 提交本身即为此）。
2. **hd-host 配对面**：`pairing.rs` / `vault.rs` / `commands.rs` 的 `start_pairing*` / `normalize_server_base_url` / `set_server_base_url` / `core_sign_in` / `sign_out` / `reinject_session_cookie` / `unpair_device` / `converge_revoked_to_pairing`。
3. **hd-host 托管面**：`daemon.rs` / `supervisor.rs` / `lifecycle.rs` / `instance_lock.rs` / `bootstrap.rs`。
4. **hd-host 导航面**：`gui.rs::open_baseline` + `baseline_url_snapshot` + `plan_web_ui_navigation`。
5. **hd-runner 执行面**：`transport` / `heartbeat` / `reconnect` / `ipc` / `idempot` / `router` / `frames::fs_list` / `capabilities`（`fs.list` 已在 `fcf9dbe` 端到端接通，是当前唯一真实上线的端侧能力）。
6. **hd-core 闸门**：`security::guard` + `security::paths` + `executor::fs::list_dirs`。

### 7.2 三条事实性结论

1. **crates 里没有「半成品内核」债务**：零 `unimplemented!()` / `todo!()` / `TODO` / `FIXME`；`pub` 导出面均有消费方。剩余工作量的真实形态是**「1 处失真文档 + 1 项构建口径核准 + 1 组未使用依赖」**，而不是补功能。
2. **`hd-host` 是绝对重心（10,965 行 / 占四 crate 55.4%），且集中在配对面**。`commands.rs`(3,280) + `pairing.rs`(1,098) + `vault.rs`(604) = 4,982 行，与「桌面端只做配对」的定位**完全一致**——这是定位收窄后的自然结果，不是冗余。
3. **唯一可能新增功能的入口是 `fs.glob`/`fs.grep`（O-1），且门槛在服务端**。`hd-core/Cargo.toml` 的 4 个预留依赖必须与「服务端 L1 是否派发该工具」+「Web 侧是否有消费方」同时成立才能激活；单看 Rust 侧依赖已预留，**不构成立项理由**（此即移动端 R1 / 桌面端 R2 的同一类判据）。

### 7.3 建议的下一步核准顺序（不含排期）

1. 跑 `cargo test -p hd-protocol -p hd-runner` 与 `cargo test -p hd-host`，确认 `fcf9dbe` 之后全绿（M-3）。
2. 读 `scripts/` + `build.cmd` / `build.sh`，确认 release 带 `gui,custom-protocol`（M-2）。
3. 修正 `hd-runner/src/lib.rs:3-5` 注释（M-1）。
4. 再决定 D-1（删依赖）与 O-1（做 `fs.glob`）的先后——两者互斥。

---

*本报告为只读勘察产物，未对 `harness_desktop` 做任何写入。*
