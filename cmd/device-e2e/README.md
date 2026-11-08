# cmd/device-e2e —— 服务端 ↔ 桌面端 设备协同联调脚本

一条命令把「**配对 → WS 握手 → 心跳 → 任务派发 → 端侧执行 → 结果回执收敛**」整条设备协同主链
真实跑一遍，并对每一步做断言，最后打印 `PASS / FAIL / SKIP` 汇总。

这是**服务端视角**的联调工具：脚本自己扮演「桌面端/移动端」这一侧。
Rust 侧（`harness_desktop`）另有 `hd-testkit` 假**服务端**，方向正好相反 —— 那边测的是「真桌面端
对不对假服务端」，这边测的是「真服务端对不对假桌面端」。两者互补，不重复。

---

## 1. 快速开始

```powershell
# 工作区根目录：D:\WorkSpace\Project\harness_go
go run ./cmd/device-e2e
```

预期结尾：

```
汇总：PASS 10 / FAIL 0 / SKIP 0
结论：PASS ✅ 设备协同主链（配对 → WS → 心跳 → 派发 → 回执）全部通过
```

**前置条件：无。** 不需要先启动服务端、不需要建库、不需要改 `config.yaml`。

---

## 2. 为什么默认是「自包含」而不是「连已启动的服务端」

自包含模式下，脚本会在**自己进程内**装配一套真实服务端：

| 组成 | 用的什么 |
| --- | --- |
| HTTP 路由 | `api.NewRouter(api.Deps{...})` —— 真实路由 + 真实中间件链（Recover/TraceID/AccessLog/Auth/RateLimit） |
| 数据库 | 真实 sqlite（`store.OpenMain`），数据目录为系统临时目录，脚本退出即删 |
| 设备域 | 真实 `service.DeviceService`（配对签发/兑换、令牌鉴权、在线落库） |
| WS 网关 | 真实 `device.NewGateway`（升级前鉴权 + welcome + ping/pong + 帧校验） |
| 在线表 | 真实 `device.NewHub`（心跳巡检 + 离线判定） |
| 任务派发器 | 真实 `device.NewDispatcher`（幂等缓存 + task.dispatch 下发 + 终态收敛） |
| 监听 | `httptest.NewServer`，绑定 **127.0.0.1**（不是 localhost） |

**根本原因**：服务端目前**没有**触发 `dispatcher.Dispatch` 的 HTTP 端点 —— 它只在单测里被调用过。
本脚本的核心目标恰恰是「派发 → 端侧执行 → 回执收敛」这一段，所以必须进程内直连派发器。
既然无论如何都要有个进程内服务端，就索性把整条链都做成自包含，换掉「用户手动起服务端」这个
最易出错的前置。

> 代价：自包含模式验证的是「本仓库当前代码」，不是「你正在跑的那个已部署服务端」。
> 要验证已部署实例，用下面的外部模式（但派发段会 SKIP）。

---

## 3. 用法

```
go run ./cmd/device-e2e [flags]
```

| flag | 默认 | 说明 |
| --- | --- | --- |
| `-mode` | `full` | `full`=跑到派发与回执；`pair-only`=只跑到配对 + WS 握手 + 心跳；`runner`=拉起**真实 hd-runner 二进制**做两端真实对撞（见第 4.5 节） |
| `-external` | `false` | `true`=连已启动的服务端（先 `/healthz` 探活）；`false`=进程内自起。**不可与 `-mode runner` 同用** |
| `-base-url` | `http://127.0.0.1:5611` | 仅外部模式生效。**必须用 `127.0.0.1`，不要用 `localhost`**（Windows 上 `localhost` 可能解析到 `::1`，服务端只监听 IPv4 时表现为连接被拒） |
| `-user` / `-password` | `e2e-admin` / `e2e-password-123` | 登录账号。自包含模式会自动创建该账号 |
| `-data-dir` | 空（临时目录） | 自包含模式的数据目录；指定后可保留文件用于排查 |
| `-v` | `false` | 打印被跳过的帧（`type` / `id` / `v`） |
| `-disable-devices` | `false` | 自包含模式下**故意**关闭设备域，用于自检 `503 device.disabled` 的提示文案 |
| `-runner-bin` | 空（自动探测） | 仅 `-mode runner`：hd-runner 可执行文件路径。空时按 `HD_RUNNER_BIN` 环境变量 → `../harness_desktop/target/debug/hd-runner[.exe]` → `.../release/...` 依次探测 |
| `-runner-no-dev` | `false` | 仅 `-mode runner`：不给二进制传 `--dev`。默认传，因为本地自包含服务端是 `ws://` 明文 |

### 示例

```powershell
# 1) 默认：完整主链（推荐）
go run ./cmd/device-e2e

# 2) 只验证配对与握手（CI 里跑得快）
go run ./cmd/device-e2e -mode pair-only

# 3) 验证已部署的服务端
go run ./cmd/device-e2e -external -base-url http://127.0.0.1:5611 -user admin -password <你的口令>

# 4) 自检失败提示文案
go run ./cmd/device-e2e -disable-devices
```

---

## 4. 每一步验证了什么

| 步骤 | 动作 | 断言（不满足即 FAIL） |
| --- | --- | --- |
| `[0]` 登录 | `POST /api/v1/auth/login` | 返回非空 `token`（顺带验证鉴权中间件放行登录路径） |
| `[1]` 签发配对码 | `POST /api/v1/devices/pair-codes`（**带用户令牌**） | HTTP 201；`code` 恰为 6 位；`ttl_s > 0` |
| `[2]` 配对 | `POST /api/v1/devices/pair`（**免登录**） | 返回 `device_token` 且前缀为 `hgd_`；`protocol_version > 0` |
| `[3]` WS 升级 | `WS /api/v1/devices/ws?token=<device_token>` | 拿到 `101 Switching Protocols`（不是 401/503） |
| `[4]` welcome | 读首帧 | `heartbeat_s > 0`；`offline_after_s >= heartbeat_s`；`protocol_version == 服务端当前版本`；`device_id` 与配对返回一致 |
| `[5]` ping→pong | 上行 `ping` | **`pong.id` 与 `ping.id` 完全相同**（端侧靠它做请求应答配对）；`pending` / `protocol_version` 可解析 |
| `[6]` 派发 | 进程内 `Dispatcher.Dispatch(TaskRequest{...})` | 端侧收到 `task.dispatch`；`task_id` 一致；**`task_id == tool_event_id`**（跨仓不变量）；`tool_name` 一致；`deadline` 未过期 |
| `[7]` 端侧执行 | 上行 `task.progress` + `task.result` | 两帧写出成功（脚本扮演端侧执行 `fs.list`） |
| `[8]` 服务端终态 | 读 `Dispatch` 返回值 | `status=ok`；**`source_trust` 被强制为 `untrusted`**（端侧故意谎称 trusted，服务端必须覆盖）；`device_id` 与配对一致；`result` 是合法 JSON |
| `[9]` 幂等 | 重复上报同 `task_id` 的**失败**结果 + 重派 | 已收敛终态不被污染，仍是 `ok`（幂等缓存生效） |

---

## 4.5 `-mode runner`：拉起真实 hd-runner 二进制（两端真实对撞）

前三步 `[0]`/`[1]`/`[2]`（登录 / 签发配对码 / 配对换令牌）与普通模式**完全复用**，
之后分支为 `[R1]`~`[R5]`，端侧不再是 `client.go` 里的 Go 假端侧，而是**真的 `hd-runner` 进程**。

```
go run ./cmd/device-e2e -mode runner
```

| 步骤 | 动作 | 断言 |
| --- | --- | --- |
| `[R1]` | 探测二进制：`-runner-bin` → `HD_RUNNER_BIN` → `../harness_desktop/target/{debug,release}/hd-runner[.exe]` | 文件存在、非目录、非 0 字节。**找不到则 SKIP（退出码仍 0）**并打印构建命令 |
| `[R2]` | `exec.Command` 拉起进程，向 **stdin** 写 `{"op":"auth","token":...}`（**保持 stdin 打开**） | 进程在 1.2s 观察窗内**没有**自行退出；再 best-effort 下发 `reload_policy` 让 `fs.list` 守卫有白名单根 |
| `[R3]` | 轮询服务端 `Hub.Online(device_id)` | 设备进入在线表（真实 WS 握手 + 鉴权通过）；若进程提前死掉，立刻带 stderr 现场失败，不干等 |
| `[R4]` | 进程内 `Dispatcher.Dispatch(fs.list)` | 收到真实端侧 `task.result`：非 `Degraded`、**`source_trust` 恒为 `untrusted`**。端侧回 `error`（如 `fs.path_denied`）也算链路成立 → PASS 并亮出原因 |
| `[R5]` | stdin 写 `{"op":"shutdown"}` **并关闭写端**（双保险：EOF 也会自杀） | 退出码 0、Wait 已回收（无孤儿）；附带观察在线表是否同步移除 |

### 二进制缺失时的降级行为

```
[R1] 探测 hd-runner 真实二进制
  ⏭  真实 Runner 二进制 —— SKIP：未找到 hd-runner 可执行文件（已尝试 4 个位置）
      · ..\harness_desktop\target\debug\hd-runner.exe  ← cargo debug 约定产物
      ...
  ℹ 请先构建：cd ..\harness_desktop && cargo build -p hd-runner --bin hd-runner（或 -runner-bin 指向已有二进制）
  ℹ 其余 [R2]~[R5] 一并跳过；自包含模式（不带 -mode runner）不受影响
```

**为什么是 SKIP 而不是 FAIL**：`hd-runner` 属于另一个仓库，未构建是**环境未就绪**，
不是本仓库的缺陷。若判 FAIL，`-mode runner` 就没法放进 CI（clean checkout 必红）。

### 与 `client.go` 的关键差异（易踩坑）

- **`ws_url` 不带 `?token=`**：Runner 侧 `transport.rs::build_url` 会自己附加。
  我们再拼一次就变成 `?token=A&token=B` → 偶发鉴权失败。令牌本体只走 auth 行的 `token` 字段。
- **必须在此处就分叉**：`[3]` 会用**同一 `device_id`** 再建一条脚本自己的 WS 连接，
  而 `Hub.Register` 对同 device_id 是**替换**语义 → 两条连接互相顶掉，真实 Runner 被判离线。
- **stdin 不能立刻关**：Runner 契约是「stdin EOF 自杀（防孤儿）」，写完 auth 就 Close 会让它刚连上就退出。
- **`--dev` 自动兜底**：先试带 `--dev`；若 1.2s 内因「参数不被接受」退出，自动去掉重试一次（版本差异自愈）。

### 进程安全

只对**本脚本自己 `exec.Command` 拉起的 PID** 调用 `cmd.Process.Kill()`（`ctx` 取消时也一样）。
**绝不**按镜像名全局杀（`taskkill /IM`、`pkill -f`）—— 那会连带打掉用户正在使用的桌面端或另一条 CI 的 Runner。

---

## 5. 如何解读失败

脚本每条 FAIL 都自带「该去改什么」，下面是完整对照表：

| 现象 | 含义与修复 |
| --- | --- |
| `设备域未启用（503 device.disabled）` | 外部服务端的 `config.yaml` 里 `devices.enabled` 为 `false` → 改成 `true` 后重启 |
| `配对码无效/已消费/已过期（401 device.pair_code_invalid）` | 配对码 TTL 默认 **120s** 且**一次性**。外部模式下重新跑脚本即可（脚本每次现签现用）；手工 curl 时注意别复用同一个码 |
| `设备令牌已被吊销（401 device.revoked）` | 该设备在管理端被 `DELETE /api/v1/devices/{did}` 删了 → 重新配对 |
| `WS 升级被拒 401` | 设备令牌无效/已吊销。注意 WS **不能**用用户令牌，必须用配对换来的 `device_token`，且只能经 `?token=` 传（WS 无法自定义头） |
| `WS 连接失败（未拿到 HTTP 响应）` | `-base-url` 不可达：服务端没起、端口不对，或写成了 `localhost`（IPv6 解析问题）。用 `127.0.0.1` |
| `等待 "welcome" 帧超时` | 升级成功但服务端没下发首帧 → 检查 `Gateway.serve` 是否被调用（`DeviceWS` 是否真的接到了路由器） |
| `等待 "pong" 帧超时` | 上行 ping 未被处理 → 检查 `Hub.Beat` / `handleFrame` 分支 |
| `pong.id 与 ping.id 不一致` | 服务端没有把请求帧的 `id` 回带 → 端侧无法配对请求应答 |
| `等待 "task.dispatch" 帧超时` | 派发帧没到端侧：Hub 里没有在线设备（WS 已断？）或 `Gateway.Handler` 没接派发器 |
| `等待 Dispatch 收敛超时（15s）` | 端侧已回执但服务端没收敛 → `GatewayOptions.Handler` 未接 `Dispatcher.HandleFrame`，或 `task_id` 与 `tool_event_id` 不匹配导致 `applyResult` 找不到在途任务 |
| `不变量被破坏：task_id 必须等于 tool_event_id` | 派发器 `TaskRequest.normalized()` 逻辑被改动 |
| `端侧结果必须被强制为 untrusted` | 不变量 4 被破坏：服务端不得信任端侧自报的 `source_trust` |
| `重复回执污染了终态` | 幂等缓存失效：`applyResult` / `cached` 被改动 |
| 汇总里出现 `SKIP` 且不是 `-mode=pair-only` | 你用了外部模式 —— 派发段因为「服务端无 HTTP 派发端点」被跳过。要跑完整链请去掉 `-external` |

退出码：`0`=全 PASS（含 SKIP）；`1`=有 FAIL；`2`=参数错误。

---

## 6. 安全约定

`device_token` 是**长期凭证**（等于端侧身份），因此：

- 脚本输出一律打码为「**前 8 位 + `***`**」，从不打印完整令牌；
- 用户令牌同样打码；
- WS 令牌只经 `?token=` query 传递（协议限制），服务端 access log 侧已有 `observ.RedactQuery` 兜底；
- 自包含模式的临时库/临时目录在退出时删除（`-data-dir` 显式指定时除外）。

---

## 7. 已知限制

1. **服务端没有 HTTP 派发端点**。`dispatcher.Dispatch` 目前只被单测调用，所以派发段只能在进程内
   （自包含模式）跑。若将来加了 `POST /api/v1/devices/tasks` 之类的端点，本脚本的 `[6]` 步应改为
   走 HTTP，届时 `-external` 也能跑完整链。
2. **外部模式只到心跳**（`[0]`~`[5]`），派发与回执 SKIP。
3. 脚本扮演的端侧只执行 `fs.list` 这一种「假执行」（返回固定 JSON），不做真实文件系统操作 ——
   本脚本验证的是**协议与服务端收敛逻辑**，不是端侧工具实现。
4. `progress` 帧只验证「能上行且不影响终态」；服务端目前把 progress 交给 `NoopSink`/回调，不落库，
   也无法从外部观测，故不做内容断言。

---

## 8. 与其它联调资产的关系

| 资产 | 角色 |
| --- | --- |
| `cmd/device-e2e`（本目录） | **服务端视角**：真服务端 ↔ 假桌面端，覆盖全主链 |
| `harness_desktop` 的 `hd-testkit` | **桌面端视角**：假服务端 ↔ 真桌面端（Rust），验证端侧状态机 |
| `internal/api/device_ws_integration_test.go` | 真实路由 + 中间件链上的 WS 端到端（配对/升级/welcome/ping-pong/吊销断连） |
| `internal/device/gateway_chain_test.go` | Gateway + Hub + Dispatcher + 配对码 的装配级主链（含离线降级、重连） |
| `internal/device/dispatcher_test.go` | 派发器单点（幂等、超时、降级、结果强制 untrusted） |

本脚本的装配方式借鉴了上述测试的脚手架，但**没有** import 测试代码 —— 它用的是与生产一致的
公开构造函数（`api.NewRouter` / `device.NewGateway` / `device.NewDispatcher` / `service.NewDeviceService`），
因此不会因为测试辅助函数变更而失效。

---

## 9. 相关服务端配置

`config.yaml`（外部模式才需要改）：

```yaml
devices:
  enabled: true            # 关掉时所有设备端点返回 503 device.disabled
  pair_ttl_s: 120          # 配对码有效期
  heartbeat_s: 15          # 端侧 ping 间隔（welcome 里下发）
  offline_after_s: 45      # 无帧判离线阈值（应为 heartbeat_s 的 3 倍）
  task_ttl_s: 600          # 任务派发超时
  strict_approval_device: false
```

自包含模式内部使用 `pair_ttl=120s / heartbeat=15s / offline_after=45s / task_ttl=10s`
（`task_ttl` 调短是为了失败时脚本能快速退出，不影响任何断言）。
