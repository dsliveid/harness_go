// Package toolapi 定义工具契约（公开层，零内核依赖）。
//
// 依赖约束（.golangci.yml depguard 强制）：
//   - 本包只依赖标准库，禁止 import harness-go/internal/*；
//   - 第三方插件只依赖本包即可实现工具，无需接触内核。
//
// 契约冻结：形状定义见 docs/protocols.md §1。冻结后只允许**向后兼容地新增字段**，
// 破坏性变更必须走「改 api/openapi.yaml → 重生成类型 → 双端同步」。
package toolapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Tier 工具注入档位（D11 三档）。
//
// 宽进严出：档位只决定「注入深度」，不决定「能否执行」——
// 未注入的已注册工具仍可被显式调用，避免检索漏召回时直接失败。
type Tier string

const (
	// TierCore 基础工具：全量定义（name + description + schema）注入 tools 数组。
	TierCore Tier = "core"
	// TierDeferred 按需工具：仅「名称 + 一行描述」进 system 索引段（约 18 token/条）。
	TierDeferred Tier = "deferred"
	// TierOnDemand 手动工具：零注入，仅 UI 可启用。
	TierOnDemand Tier = "on_demand"
)

// ParseTier 解析档位字符串；空值与未知值一律回落 TierDeferred。
func ParseTier(s string) Tier {
	switch Tier(strings.TrimSpace(strings.ToLower(s))) {
	case TierCore:
		return TierCore
	case TierOnDemand:
		return TierOnDemand
	case TierDeferred:
		return TierDeferred
	default:
		return TierDeferred
	}
}

// Valid 判断档位是否为已知值。
func (t Tier) Valid() bool {
	return t == TierCore || t == TierDeferred || t == TierOnDemand
}

// Executor 执行面归属（多端协同 HALO）。
type Executor string

const (
	// ExecutorServer 服务端执行（默认）。
	ExecutorServer Executor = "server"
	// ExecutorDevice 端侧执行（HALO Runner）。
	ExecutorDevice Executor = "device"
	// ExecutorAny 任一端可执行。
	ExecutorAny Executor = "any"
)

// Risk 工具风险级（取值与 internal/model.Risk 保持一致，便于引擎直接落库）。
type Risk string

const (
	// RiskReadonly 只读：无需审批（访问模式为 readonly / confirm 时自动放行）。
	RiskReadonly Risk = "readonly"
	// RiskWrite 写文件：confirm / full 模式下需审批。
	RiskWrite Risk = "write"
	// RiskCommand 执行命令：默认需审批。
	RiskCommand Risk = "command"
	// RiskDanger 危险操作：任何访问模式下都必须审批。
	RiskDanger Risk = "danger"
)

// ParseRisk 解析风险级；空值与未知值保守回落 RiskWrite。
func ParseRisk(s string) Risk {
	switch Risk(strings.TrimSpace(strings.ToLower(s))) {
	case RiskReadonly:
		return RiskReadonly
	case RiskCommand:
		return RiskCommand
	case RiskDanger:
		return RiskDanger
	default:
		return RiskWrite
	}
}

// 能力标签（router 据此判定本端能否执行）。
const (
	// CapWorkspaceRead 读取工作区文件。
	CapWorkspaceRead = "workspace.read"
	// CapWorkspaceWrite 写入工作区文件。
	CapWorkspaceWrite = "workspace.write"
	// CapProcess 派生子进程。
	CapProcess = "process"
	// CapNetwork 出网访问。
	CapNetwork = "network"
	// CapLLM 调用模型。
	CapLLM = "llm"
	// CapDevice 需要端侧执行面。
	CapDevice = "device"
)

// HealthStatus 健康态（生命周期：registered → validating → available/degraded/unavailable）。
type HealthStatus string

const (
	// HealthAvailable 可用。
	HealthAvailable HealthStatus = "available"
	// HealthDegraded 降级可用（依赖缺失但有兜底）。
	HealthDegraded HealthStatus = "degraded"
	// HealthUnavailable 不可用。
	HealthUnavailable HealthStatus = "unavailable"
)

// Health 健康检查结果。
type Health struct {
	Status    HealthStatus `json:"status"`
	Message   string       `json:"message,omitempty"`
	CheckedAt time.Time    `json:"checked_at"`
}

// Spec 工具规格（唯一的工具描述真源，内置插件 / 市场包 / MCP 转换共用）。
type Spec struct {
	Name         string          `json:"name"`
	Namespace    string          `json:"namespace,omitempty"` // 例：builtin / pkg:<id> / mcp:<server>
	Description  string          `json:"description"`
	SchemaJSON   json.RawMessage `json:"schema_json"`
	Tier         Tier            `json:"tier"`
	Executor     Executor        `json:"executor"`
	Capabilities []string        `json:"capabilities,omitempty"`
	CostTokens   int             `json:"cost_tokens"`
	Risk         Risk            `json:"risk"`
	Requires     []string        `json:"requires,omitempty"` // 运行期依赖（如 chromium、git）
	Version      string          `json:"version,omitempty"`
}

// Normalized 返回补齐默认值后的规格副本（不修改接收者）。
func (s Spec) Normalized() Spec {
	out := s
	out.Name = strings.TrimSpace(out.Name)
	if out.Tier == "" {
		out.Tier = TierDeferred
	}
	if out.Executor == "" {
		out.Executor = ExecutorServer
	}
	if out.Risk == "" {
		out.Risk = RiskWrite
	}
	if out.Namespace == "" {
		out.Namespace = "builtin"
	}
	if out.CostTokens <= 0 {
		out.CostTokens = EstimateCostTokens(out.Name, out.Description, out.SchemaJSON)
	}
	return out
}

// Validate 校验规格（注册期调用，失败即拒绝注册）。
func (s Spec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("工具名为空")
	}
	if !s.Tier.Valid() {
		return fmt.Errorf("工具 %s 的 tier 非法: %q", s.Name, s.Tier)
	}
	switch s.Executor {
	case ExecutorServer, ExecutorDevice, ExecutorAny, "":
	default:
		return fmt.Errorf("工具 %s 的 executor 非法: %q", s.Name, s.Executor)
	}
	if len(s.SchemaJSON) > 0 && !json.Valid(s.SchemaJSON) {
		return fmt.Errorf("工具 %s 的 input schema 不是合法 JSON", s.Name)
	}
	return nil
}

// EstimateCostTokens 估算单条工具定义的注入成本（≈ 字符数 / 3，中文偏保守）。
//
// 只用于「成本可见」与软上限告警，不作为计费依据。
func EstimateCostTokens(name, description string, schema json.RawMessage) int {
	n := len([]rune(name)) + len([]rune(description)) + len(schema)/3
	if n <= 0 {
		return 1
	}
	return n/3 + 1
}
