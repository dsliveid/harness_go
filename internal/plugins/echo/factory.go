// Package echo 是工具契约的一致性 fixture 插件（A-10 出口验证用）。
//
// 存在意义：用一个最小、无副作用、只读的工具证明整条链路可用——
// 清单声明 → 注册 → tier 注入 → 模型发起 tool_call → 审批闸门 → 执行 → tool_events 落库。
//
// 依赖约束：只 import pkg/toolapi（depguard 强制），不接触内核任何 internal 包。
package echo

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"harness-go/pkg/toolapi"
)

//go:embed manifest.json
var manifestJSON []byte

// ToolName 工具名（fixture 断言用）。
const ToolName = "echo"

// Manifest 返回插件清单（每次返回副本，避免调用方篡改全局状态）。
func Manifest() toolapi.Manifest {
	var m toolapi.Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		// 清单是编译期嵌入的常量，解析失败属于构建期缺陷，直接 panic 暴露
		panic("echo: 内嵌 manifest.json 解析失败: " + err.Error())
	}
	if m.ProtocolVersion == 0 {
		m.ProtocolVersion = toolapi.ProtocolVersion
	}
	return m
}

// Factory 返回插件工厂（cmd/harness-go/plugins.go 的唯一装配入口）。
func Factory() toolapi.Factory {
	return toolapi.FactoryFunc{M: Manifest(), NewF: func(_ toolapi.Host, _ toolapi.Config) (toolapi.Plugin, error) {
		return &plugin{spec: toolSpec()}, nil
	}}
}

// toolSpec 从清单取首个工具规格，保证「清单声明 = 运行时规格」单点真源。
func toolSpec() toolapi.Spec {
	m := Manifest()
	if len(m.Tools) == 0 {
		panic("echo: manifest 未声明工具")
	}
	return m.Tools[0].Normalized()
}

// plugin 插件实例。
type plugin struct {
	spec toolapi.Spec
}

// Start 实现 toolapi.Plugin（echo 无外部依赖，直接就绪）。
func (p *plugin) Start(context.Context) error { return nil }

// Stop 实现 toolapi.Plugin。
func (p *plugin) Stop(context.Context) error { return nil }

// Tools 实现 toolapi.Plugin。
func (p *plugin) Tools() []toolapi.Tool { return []toolapi.Tool{&echoTool{spec: p.spec}} }

// Health 实现 toolapi.Plugin。
func (p *plugin) Health() toolapi.Health {
	return toolapi.Health{Status: toolapi.HealthAvailable, Message: "回显 fixture 就绪", CheckedAt: time.Now()}
}

// echoTool echo 工具实现。
type echoTool struct{ spec toolapi.Spec }

// Spec 实现 toolapi.Tool。
func (t *echoTool) Spec() toolapi.Spec { return t.spec }

// Invoke 实现 toolapi.Tool：回显 text 参数。
func (t *echoTool) Invoke(_ context.Context, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Text string `json:"text"`
	}
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return &toolapi.Result{
				Content: fmt.Sprintf("参数不是合法 JSON 对象: %v", err),
				IsError: true,
			}, nil
		}
	}
	if args.Text == "" {
		return &toolapi.Result{Content: "参数 text 不能为空", IsError: true}, nil
	}
	return &toolapi.Result{
		Content: "echo: " + args.Text,
		Meta:    map[string]any{"tool_event_id": call.ToolEventID},
	}, nil
}
