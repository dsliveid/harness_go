//go:build ignore

// gen-fixtures 生成设备协议的黄金 JSON 样本，供 harness_desktop 的 hd-protocol
// crate 做反序列化门禁（防 Go/Rust 双写漂移）。
//
// 用法（在工作区根目录）：
//
//	go run scripts/gen-fixtures.go
//	go run scripts/gen-fixtures.go -out ../harness_desktop/crates/hd-protocol/fixtures
//
// 设计要点（改动前务必读完）：
//  1. **禁止手写 JSON**：所有样本一律经由 device.MustEnvelope / 各 Payload 结构体
//     真实序列化产出。手写的 fixture 只能验证「我以为的协议」，无法发现结构体改动漏改
//     Rust 侧的问题 —— 那就完全失去了门禁意义。
//  2. 本文件带 `//go:build ignore`，不参与 go build / go vet / go test ./... 的常规编译，
//     因此不会污染服务端构建；但它仍在 `scripts/` 下可被 go run 直接执行。
//  3. 时钟固定（不取 time.Now），保证重复执行产出一致 —— 否则 fixture 每次都变，
//     黄金样本失去「基准」含义，CI 会产生无意义 diff。
//  4. 文件名与 Rust 侧 tests/golden_fixtures.rs 中的 include_str! 一一对应，
//     改名必须两侧同步。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"harness-go/internal/device"
	"harness-go/internal/model"
)

// fixedTS 固定时间戳（2024-01-01T00:00:00Z，毫秒），避免 fixture 抖动。
const fixedTS = int64(1704067200000)

func main() {
	out := flag.String("out", filepath.Join("crates", "hd-protocol", "fixtures"),
		"fixture 输出目录（默认相对当前目录，即 harness_desktop 仓内路径）")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail("创建输出目录失败: %v", err)
	}

	// 顺序执行，任一失败立即退出，避免产出半套 fixture。
	for _, gen := range []func(string) error{
		genHello,
		genWelcome,
		genPing,
		genPong,
		genError,
		genTaskDispatch,
		genTaskProgress,
		genTaskResult,
		genLeaseRevoked,
	} {
		if err := gen(*out); err != nil {
			fail("%v", err)
		}
	}

	fmt.Printf("✅ fixture 已生成到 %s\n", *out)
}

// ============ 各帧样本 ============

func genHello(dir string) error {
	return writeEnvelope(dir, "hello.json", device.MsgHello, "f-hello-1", device.HelloPayload{
		Name:             "我的工作站",
		Platform:         "windows-amd64",
		AppVersion:       "1.0.0",
		CapabilitiesHash: "sha256:abc123",
		ProtocolVersion:  device.CurrentProtocolVersion,
		Capabilities: []model.Capability{
			{Name: "workspace.read", Version: "1"},
			{Name: "workspace.list", Version: "1"},
			{Name: "process", Version: "1"},
		},
	})
}

func genWelcome(dir string) error {
	return writeEnvelope(dir, "welcome.json", device.MsgWelcome, "f-welcome-1", device.WelcomePayload{
		DeviceID:        "dev_0123456789abcdef",
		ServerTime:      fixedTS,
		ProtocolVersion: device.CurrentProtocolVersion,
		HeartbeatS:      15,
		OfflineAfterS:   45,
		Pending:         2,
		LeaseEpoch:      7,
	})
}

func genPing(dir string) error {
	return writeEnvelope(dir, "ping.json", device.MsgPing, "f-ping-1", device.PingPayload{
		// Load 是 json.RawMessage：端侧上报的负载快照，形状由端侧决定，服务端透传。
		Load:             json.RawMessage(`{"cpu":0.42,"mem":0.61,"running":1}`),
		CapabilitiesHash: "sha256:abc123",
		ProtocolVersion:  device.CurrentProtocolVersion,
	})
}

func genPong(dir string) error {
	return writeEnvelope(dir, "pong.json", device.MsgPong, "f-pong-1", device.PongPayload{
		ServerTime:      fixedTS + 1000,
		Pending:         0,
		ProtocolVersion: device.CurrentProtocolVersion,
	})
}

func genError(dir string) error {
	return writeEnvelope(dir, "error.json", device.MsgError, "f-error-1", device.ErrorPayload{
		Code:    device.FrameCodeProtocolMismatch,
		Message: "端侧协议版本 99 不受支持（服务端支持 1~1）",
		Fatal:   true,
	})
}

func genTaskDispatch(dir string) error {
	return writeEnvelope(dir, "task_dispatch.json", device.MsgTaskDispatch, "te_abc123", device.TaskDispatchPayload{
		// 🔴 TaskID 与 ToolEventID 必须同值（protocol.go:235 注释：端侧据此做幂等）
		TaskID:      "te_abc123",
		RunID:       "run_xyz789",
		ToolEventID: "te_abc123",
		ToolName:    "fs.list",
		Args:        json.RawMessage(`{"path":"C:\\Users\\me\\work","max":500}`),
		Deadline:    fixedTS + 30000,
		Attempt:     1,
	})
}

func genTaskProgress(dir string) error {
	return writeEnvelope(dir, "task_progress.json", device.MsgTaskProgress, "te_abc123", device.TaskProgressPayload{
		TaskID:      "te_abc123",
		RunID:       "run_xyz789",
		ToolEventID: "te_abc123",
		Chunk:       "扫描目录 128/500…",
	})
}

func genTaskResult(dir string) error {
	return writeEnvelope(dir, "task_result.json", device.MsgTaskResult, "te_abc123", device.TaskResultPayload{
		TaskID:      "te_abc123",
		RunID:       "run_xyz789",
		ToolEventID: "te_abc123",
		Status:      "success",
		Result:      json.RawMessage(`{"dirs":[{"name":"src","path":"C:\\Users\\me\\work\\src"}],"truncated":false}`),
		// 🔴 端侧结果恒 untrusted（不变量 4）；即便本机产出也不得自封 trusted。
		SourceTrust: "untrusted",
	})
}

func genLeaseRevoked(dir string) error {
	return writeEnvelope(dir, "lease_revoked.json", device.MsgLeaseRevoked, "f-lease-1", device.LeaseRevokedPayload{
		SessionID: "sess_001",
		Epoch:     8,
		Reason:    "另一台设备接管了该会话",
	})
}

// ============ 公共写入 ============

// writeEnvelope 用服务端真实构造器生成信封并落盘。
//
// 刻意走 device.MustEnvelope 而非自己拼 map：这样 fixture 的 v / ts / type 等
// 信封级字段永远跟随服务端实现，服务端改了信封形状，fixture 立刻跟着变、Rust 侧
// 门禁立刻变红 —— 这正是我们要的。
func writeEnvelope(dir, name string, t device.MessageType, id string, payload any) error {
	env := device.MustEnvelope(t, id, payload)
	// newEnvelopeAt 用的是 time.Now，这里刻意用固定 ts 覆盖，保证产出稳定。
	env.TS = fixedTS

	raw, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 %s 失败: %w", name, err)
	}
	raw = append(raw, '\n')

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", name, err)
	}
	fmt.Printf("  · %s\n", name)
	return nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "❌ "+format+"\n", args...)
	os.Exit(1)
}
