package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"harness-go/internal/approval"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/model"
)

// ============ C-7：审批跨端仲裁（发起端归属 + 高风险绑定执行端） ============

// dangerCatalog 让工具风险级为 danger（触发「必须逐次确认」并绑定执行端设备）。
type dangerCatalog struct{}

func (dangerCatalog) Risk(string) (model.Risk, bool) { return model.RiskDanger, true }

// readOnlyCatalog 只读风险（自动放行，不应登记审批）。
type readOnlyCatalog struct{}

func (readOnlyCatalog) Risk(string) (model.Risk, bool) { return model.RiskReadonly, true }

// approvalRequestPayload 从事件流里取第一条 approval_request 的 payload。
func approvalRequestPayload(t *testing.T, bus *events.Bus) map[string]any {
	t.Helper()
	sub, err := bus.Subscribe("run-1", 0)
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer bus.Unsubscribe(sub)
	for ev := range sub.C() {
		if ev.Type != events.TypeApprovalRequest {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("解析 payload 失败: %v", err)
		}
		return payload
	}
	return nil
}

// 高风险 + 端侧发起 → 审批事件带 requested_by_device 与 required_device_id（同值）。
func TestEngineApprovalBindsExecutorDevice(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "run_command"}}},
		{{Delta: "已放弃"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}
	// 短超时闸门：审批必然超时拒绝，Run 继续完成（不影响断言事件内容）
	eng, _, bus, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Catalog = dangerCatalog{}
		d.Gate = approval.NewGate(60 * time.Millisecond)
	})

	req := baseRunRequest()
	req.AccessMode = approval.ModeFull // full 模式 danger 仍需审批
	req.LeaseHolder = "dev-7"
	req.RequesterDevice = "dev-7"
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	payload := approvalRequestPayload(t, bus)
	if payload == nil {
		t.Fatal("应有 approval_request 事件")
	}
	if got := payload["requested_by_device"]; got != "dev-7" {
		t.Fatalf("requested_by_device 应为 dev-7，得到 %v", got)
	}
	if got := payload["required_device_id"]; got != "dev-7" {
		t.Fatalf("required_device_id 应为 dev-7（高风险绑定执行端），得到 %v", got)
	}
}

// 服务端本地发起（无设备）→ 两字段均为空串（不绑定设备，浏览器可直接审批）。
func TestEngineApprovalNoDeviceWhenServerLocal(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "run_command"}}},
		{{Delta: "已放弃"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}
	eng, _, bus, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Catalog = dangerCatalog{}
		d.Gate = approval.NewGate(60 * time.Millisecond)
	})

	req := baseRunRequest()
	req.AccessMode = approval.ModeFull
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	payload := approvalRequestPayload(t, bus)
	if payload == nil {
		t.Fatal("应有 approval_request 事件")
	}
	if got := payload["requested_by_device"]; got != "" {
		t.Fatalf("服务端本地执行面 requested_by_device 应为空，得到 %v", got)
	}
	if got := payload["required_device_id"]; got != "" {
		t.Fatalf("未绑定设备时 required_device_id 应为空，得到 %v", got)
	}
}

// 端侧发起但工具只读 → 自动放行，不产生审批事件（绑定只作用于需审批的高风险）。
func TestEngineApprovalNotBoundForReadonly(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "fs_read"}}},
		{{Delta: "读完了"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}
	eng, _, bus, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Catalog = readOnlyCatalog{}
	})

	req := baseRunRequest()
	req.AccessMode = approval.ModeConfirm
	req.RequesterDevice = "dev-1"
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if payload := approvalRequestPayload(t, bus); payload != nil {
		t.Fatalf("只读工具不应产生审批事件：%v", payload)
	}
	if tools.calls != 1 {
		t.Fatalf("只读工具应执行 1 次，得到 %d", tools.calls)
	}
}

// highRiskNeedsDevice 口径：仅 danger 视为高风险（write/command 不绑定设备）。
func TestHighRiskNeedsDevice(t *testing.T) {
	cases := map[model.Risk]bool{
		model.RiskReadonly: false,
		model.RiskWrite:    false,
		model.RiskCommand:  false,
		model.RiskDanger:   true,
	}
	for risk, want := range cases {
		if got := highRiskNeedsDevice(risk); got != want {
			t.Fatalf("%s 期望 %v，得到 %v", risk, want, got)
		}
	}
}

// approvalRequester 优先级：RequesterDevice > LeaseHolder > 空串。
func TestApprovalRequesterResolution(t *testing.T) {
	if got := approvalRequester(RunRequest{RequesterDevice: "a", LeaseHolder: "b"}); got != "a" {
		t.Fatalf("应优先 RequesterDevice，得到 %q", got)
	}
	if got := approvalRequester(RunRequest{LeaseHolder: "b"}); got != "b" {
		t.Fatalf("应回退 LeaseHolder，得到 %q", got)
	}
	if got := approvalRequester(RunRequest{}); got != "" {
		t.Fatalf("都空时应返回空串，得到 %q", got)
	}
	if got := approvalRequester(RunRequest{RequesterDevice: "  "}); !strings.EqualFold(got, "") {
		t.Fatalf("空白应归一为空串，得到 %q", got)
	}
}
