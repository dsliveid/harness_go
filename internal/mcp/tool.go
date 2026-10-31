package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"harness-go/pkg/toolapi"
)

// jsonRaw 把字节还原为可反序列化值（SDK Arguments 为 any）。
func jsonRaw(b []byte) json.RawMessage { return json.RawMessage(b) }

// extractText 拼接 MCP content[] 中的文本块（image/resource 只记占位说明，
// 二进制落 CAS 属阶段 F Host 能力，当前不静默丢弃而是显式标注）。
func extractText(contents []sdkContent) string {
	var b strings.Builder
	for _, c := range contents {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			b.WriteString(tc.Text)
			continue
		}
		b.WriteString("[非文本内容（image/resource），已省略]\n")
	}
	return b.String()
}

// ToolName 外部工具的全局唯一名（与内置工具同一注册表键空间）。
func ToolName(prefix, serverKey, rawName string) string {
	return prefix + "__" + serverKey + "__" + sanitizeToolName(rawName)
}

// sanitizeToolName 规范化工具名段（防非法字符与分隔符冲突）。
func sanitizeToolName(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	name := strings.Trim(b.String(), "_")
	if name == "" {
		name = "tool"
	}
	return name
}

// truncateDescription 描述进上下文前统一截断（设计 §5：200 字符上限）。
func truncateDescription(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= MaxDescriptionRunes {
		return s
	}
	return string(r[:MaxDescriptionRunes]) + "…"
}

// buildSpec 把 MCP 工具描述转换为统一 Spec（命名空间 + 风险 + 成本）。
//
// 外部工具的 Risk 只能不低于配置默认（防自我降级）；
// Schema 原样透传，缺失/非法时回退空对象 schema 并保留原样可调用（宽进严出）。
func buildSpec(prefix, namespace, serverName string, rawName, description, schemaJSON, defaultRisk string) toolapi.Spec {
	if !json.Valid([]byte(schemaJSON)) || strings.TrimSpace(schemaJSON) == "" || schemaJSON == "null" {
		schemaJSON = `{"type":"object","properties":{}}`
	}
	key := ServerKey(serverName)
	return toolapi.Spec{
		Name:        ToolName(prefix, key, rawName),
		Namespace:   namespace,
		Description: truncateDescription("[" + serverName + "] " + strings.TrimSpace(description)),
		SchemaJSON:  json.RawMessage(schemaJSON),
		Tier:        toolapi.TierDeferred, // 外部工具默认 deferred（只进索引，检索命中才全量注入）
		Executor:    toolapi.ExecutorServer,
		Risk:        toolapi.ParseRisk(defaultRisk),
		CostTokens:  0, // Normalized 会按内容估算
	}
}

// TimeoutFor 供引擎侧参考的单工具超时（与 Server 配置一致）。
func TimeoutFor(cfg ServerConfig) time.Duration {
	cfg = cfg.Normalize()
	return time.Duration(cfg.TimeoutMS) * time.Millisecond
}

// 编译期保证：mcpTool 满足统一工具契约（与内置插件同构，M4）。
var _ toolapi.Tool = (*Tool)(nil)

// Tool MCP/HTTP 插件工具的统一包装：Invoke 经 Manager 路由到对应连接。
type Tool struct {
	mgr      *Manager
	serverID string
	rawName  string // MCP 侧原始工具名
	spec     toolapi.Spec
}

// Spec 实现统一契约。
func (t *Tool) Spec() toolapi.Spec { return t.spec }

// Invoke 实现统一契约：路由到所属 Server 的连接执行。
//
// 失败语义（设计 §7）：工具错误（isError=true）作为工具结果回传模型，不中断 Run；
// 连接级故障返回 error 并计入 Server 失败（连续 3 次转 degraded，移出注入集合）。
func (t *Tool) Invoke(ctx context.Context, call *toolapi.Call) (*toolapi.Result, error) {
	text, isErr, err := t.mgr.CallTool(ctx, t.serverID, t.rawName, call.Args)
	if err != nil {
		// 连接/协议级失败：返回带错误码的结果文本（引擎落 tool_events，模型可读）
		return &toolapi.Result{
			Content: err.Error(),
			IsError: true,
			Meta:    map[string]any{"mcp_code": errCodeOf(err), "mcp_server": t.serverID},
		}, nil
	}
	res := &toolapi.Result{Content: text, IsError: isErr}
	if isErr {
		res.Meta = map[string]any{"mcp_server": t.serverID}
	}
	return res, nil
}

func errCodeOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ErrCodeProtocolError
}
