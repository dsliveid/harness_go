// Package pluginkit 提供内置插件的共享助手（阶段 F）。
//
// 定位：插件之间禁止互相 import（depguard），公共助手收敛到本包——
// 它不是插件，是插件的基础设施（参数解析 / 结果构造 / diff 计算 / 行截断）。
// 铁律：本包只依赖 pkg/toolapi 与标准库，不依赖任何内核实现。
package pluginkit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"harness-go/pkg/toolapi"
)

// DecodeArgs 把调用参数解码到结构体（非法 JSON / 字段缺失由调用方校验语义）。
func DecodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil // 空参数合法（全默认值工具）
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%w: 参数解析失败: %v", toolapi.ErrInvalidArgs, err)
	}
	return nil
}

// TextResult 构造纯文本成功结果。
func TextResult(format string, args ...any) *toolapi.Result {
	return &toolapi.Result{Content: fmt.Sprintf(format, args...)}
}

// ErrorResult 构造失败结果（IsError=true，回传模型让其自我纠正，不中断 Run）。
func ErrorResult(format string, args ...any) *toolapi.Result {
	return &toolapi.Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// DataResult 构造「主文本 + 结构化数据」结果（Data 供 UI 渲染，如 diff）。
func DataResult(content string, data any) *toolapi.Result {
	raw, err := json.Marshal(data)
	if err != nil {
		return TextResult("%s", content)
	}
	return &toolapi.Result{Content: content, Meta: map[string]any{"data": json.RawMessage(raw)}}
}

// TruncateRunes 按 rune 截断（防切坏 UTF-8），超长加省略标记。
func TruncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + fmt.Sprintf("…[已截断，原 %d 字符]", len(r))
}

// LineSpec 摘要一行（首行 + 截断）——deferred 索引与列表输出共用。
func FirstLine(s string, max int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return TruncateRunes(strings.TrimSpace(s), max)
}

// ---------- diff 计算（前端 FileDiff 契约：path/hunks/additions/deletions） ----------

// DiffLine 单行（前端 DiffLine：{type: 'add'|'del'|'ctx', num1?, num2?, text}）。
type DiffLine struct {
	Type string `json:"type"` // add | del | ctx
	Num1 int    `json:"num1,omitempty"`
	Num2 int    `json:"num2,omitempty"`
	Text string `json:"text"`
}

// DiffHunk 变更块（前端 DiffHunk：{id, header, before, after}）。
type DiffHunk struct {
	ID     string     `json:"id"`
	Header string     `json:"header"`
	Before []DiffLine `json:"before"`
	After  []DiffLine `json:"after"`
}

// FileDiff 文件级 diff（前端 FileDiff 契约，字段名严格一致）。
type FileDiff struct {
	Path      string     `json:"path"`
	Hunks     []DiffHunk `json:"hunks"`
	Additions int        `json:"additions"`
	Deletions int        `json:"deletions"`
}

// LCS edit script 的经典动态规划（O(n*m)，工具场景 n,m ≤ 数千行可接受；
// 超过 maxLines 时退化为整文件替换 diff，不卡死）。
func lineDiff(oldLines, newLines []string) []op {
	const maxLines = 5000
	if len(oldLines) > maxLines || len(newLines) > maxLines {
		return []op{
			{kind: opDel, oldIdx: 0, count: len(oldLines)},
			{kind: opAdd, newIdx: 0, count: len(newLines)},
		}
	}
	n, m := len(oldLines), len(newLines)
	// dp[i][j] = oldLines[i:] 与 newLines[j:] 的 LCS 长度
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case oldLines[i] == newLines[j]:
			ops = appendOp(ops, op{kind: opCtx, oldIdx: i, newIdx: j, count: 1})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = appendOp(ops, op{kind: opDel, oldIdx: i, count: 1})
			i++
		default:
			ops = appendOp(ops, op{kind: opAdd, newIdx: j, count: 1})
			j++
		}
	}
	if i < n {
		ops = appendOp(ops, op{kind: opDel, oldIdx: i, count: n - i})
	}
	if j < m {
		ops = appendOp(ops, op{kind: opAdd, newIdx: j, count: m - j})
	}
	return ops
}

type opKind int

const (
	opCtx opKind = iota
	opAdd
	opDel
)

type op struct {
	kind   opKind
	oldIdx int
	newIdx int
	count  int
}

func appendOp(ops []op, o op) []op {
	if len(ops) > 0 {
		last := &ops[len(ops)-1]
		if last.kind == o.kind {
			last.count += o.count
			return ops
		}
	}
	return append(ops, o)
}

const hunkContext = 3 // 变更块上下文行数

// BuildFileDiff 计算两版本文本的前端契约 diff（空 old 表示新文件）。
//
// 每个变更块带 hunkContext 行上下文；无变更返回 hunks 为空（additions/deletions=0）。
func BuildFileDiff(path, oldText, newText string) FileDiff {
	oldLines := splitLines(oldText)
	newLines := splitLines(newText)
	ops := lineDiff(oldLines, newLines)

	d := FileDiff{Path: filepathToSlash(path)}
	hunkID := 0
	// 收集变更块：把连续的 add/del op（允许中间 ≤ 2*hunkContext 的 ctx 间隔）合并为一个 hunk
	type change struct {
		ops []op
	}
	var changes []change
	var cur []op
	ctxSince := 0
	for _, o := range ops {
		if o.kind == opCtx {
			if len(cur) > 0 {
				ctxSince += o.count
				if ctxSince > 2*hunkContext {
					changes = append(changes, change{ops: cur})
					cur = nil
					ctxSince = 0
				}
			}
			continue
		}
		if len(cur) == 0 {
			// 回补前文上下文
			back := o.oldIdx
			if o.kind == opAdd {
				back = o.newIdx
			}
			if back > 0 {
				n := hunkContext
				if back < n {
					n = back
				}
				startOld := o.oldIdx - n
				if o.kind == opAdd {
					startOld = o.oldIdx
				}
				cur = append(cur, op{kind: opCtx, oldIdx: startOld, newIdx: o.newIdx - n, count: n})
			}
			ctxSince = 0
		}
		cur = append(cur, o)
	}
	if len(cur) > 0 {
		changes = append(changes, change{ops: cur})
	}

	for _, ch := range changes {
		hunkID++
		h := DiffHunk{ID: fmt.Sprintf("h%d", hunkID)}
		var before, after []DiffLine
		for _, o := range ch.ops {
			for k := 0; k < o.count; k++ {
				switch o.kind {
				case opCtx:
					txt := oldLines[o.oldIdx+k]
					before = append(before, DiffLine{Type: "ctx", Num1: o.oldIdx + k + 1, Num2: o.newIdx + k + 1, Text: txt})
					after = append(after, DiffLine{Type: "ctx", Num1: o.oldIdx + k + 1, Num2: o.newIdx + k + 1, Text: txt})
				case opDel:
					before = append(before, DiffLine{Type: "del", Num1: o.oldIdx + k + 1, Text: oldLines[o.oldIdx+k]})
					d.Deletions++
				case opAdd:
					after = append(after, DiffLine{Type: "add", Num2: o.newIdx + k + 1, Text: newLines[o.newIdx+k]})
					d.Additions++
				}
			}
		}
		h.Before, h.After = before, after
		oldN, newN, txt := firstChangedLine(h)
		h.Header = fmt.Sprintf("@@ -%d +%d @@ %s", oldN, newN, FirstLine(txt, 60))
		d.Hunks = append(d.Hunks, h)
	}
	return d
}

func firstChangedLine(h DiffHunk) (old, new int, text string) {
	for _, l := range h.After {
		if l.Type != "ctx" {
			return maxInt(l.Num1, 1), l.Num2, l.Text
		}
	}
	for _, l := range h.Before {
		if l.Type != "ctx" {
			return l.Num1, maxInt(l.Num2, 1), l.Text
		}
	}
	return 1, 1, ""
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	// 保留「结尾无换行」的语义：最后一个空串去掉（Split 会多出一个）
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// UnifiedDiff 渲染标准 unified diff 文本（模型可读；给 result.Content 用）。
func UnifiedDiff(d FileDiff) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", d.Path, d.Path)
	for _, h := range d.Hunks {
		b.WriteString(h.Header + "\n")
		for _, l := range h.Before {
			if l.Type == "del" {
				b.WriteString("-" + l.Text + "\n")
			} else {
				b.WriteString(" " + l.Text + "\n")
			}
		}
		for _, l := range h.After {
			if l.Type == "add" {
				b.WriteString("+" + l.Text + "\n")
			}
		}
	}
	return b.String()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func filepathToSlash(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}

// ---------- 清单即规格（单点真源） ----------

// SpecFromManifest 从清单声明中按名取规格（「清单声明 = 运行时规格」单点真源，
// 与 echo fixture 同口径；未声明即构建期缺陷，返回错误由 Factory 暴露）。
func SpecFromManifest(m toolapi.Manifest, name string) (toolapi.Spec, error) {
	for _, s := range m.Tools {
		if s.Name == name {
			return s.Normalized(), nil
		}
	}
	return toolapi.Spec{}, fmt.Errorf("插件 %s 的 manifest 未声明工具 %q", m.ID, name)
}

// MustSpecsFromManifest 批量取规格（任一缺失 panic——清单是编译期常量）。
func MustSpecsFromManifest(m toolapi.Manifest, names ...string) []toolapi.Spec {
	out := make([]toolapi.Spec, 0, len(names))
	for _, n := range names {
		s, err := SpecFromManifest(m, n)
		if err != nil {
			panic(err)
		}
		out = append(out, s)
	}
	return out
}

// ManifestFromJSON 解析内嵌 manifest（构建期常量，解析失败 panic 暴露）。
func ManifestFromJSON(raw []byte) toolapi.Manifest {
	var m toolapi.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		panic("内嵌 manifest.json 解析失败: " + err.Error())
	}
	if m.ProtocolVersion == 0 {
		m.ProtocolVersion = toolapi.ProtocolVersion
	}
	return m
}

// PluginFromSpecs 用清单 + 规格表快速构造插件（所有 F 插件的公共骨架）。
// Host 在工厂 New 时注入并捕获到每个工具的调用里——插件经它拿受控能力。
type PluginFromSpecs struct {
	M       toolapi.Manifest
	Host    toolapi.Host
	Invokes map[string]InvokeFunc
}

// InvokeFunc 单工具调用实现。
type InvokeFunc func(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error)

// Tools 实现 toolapi.Plugin。
func (p *PluginFromSpecs) Tools() []toolapi.Tool {
	out := make([]toolapi.Tool, 0, len(p.Invokes))
	for name, fn := range p.Invokes {
		spec, err := SpecFromManifest(p.M, name)
		if err != nil {
			panic(err)
		}
		out = append(out, &kitTool{spec: spec, fn: fn, host: p.Host})
	}
	return out
}

// Start 实现 toolapi.Plugin（F 插件无长连接，幂等空操作）。
func (p *PluginFromSpecs) Start(context.Context) error { return nil }

// Stop 实现 toolapi.Plugin（幂等空操作）。
func (p *PluginFromSpecs) Stop(context.Context) error { return nil }

// Health 实现 toolapi.Plugin（文件型插件恒可用；browser 类自带探测）。
func (p *PluginFromSpecs) Health() toolapi.Health {
	return toolapi.Health{Status: toolapi.HealthAvailable, CheckedAt: time.Now()}
}

// Manifest 实现 toolapi.Plugin。
func (p *PluginFromSpecs) Manifest() toolapi.Manifest { return p.M }

type kitTool struct {
	spec toolapi.Spec
	fn   InvokeFunc
	host toolapi.Host
}

func (t *kitTool) Spec() toolapi.Spec { return t.spec }

func (t *kitTool) Invoke(ctx context.Context, call *toolapi.Call) (*toolapi.Result, error) {
	if t.fn == nil {
		return ErrorResult("工具 %s 未实现", t.spec.Name), nil
	}
	return t.fn(ctx, t.host, call)
}
