package fs

import (
	"context"
	"fmt"
	"strings"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

// invokeFileOutline 结构大纲：Markdown 标题 / Go 声明 / 通用缩进层级。
func invokeFileOutline(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("file_outline: %v", err), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return pluginkit.ErrorResult("file_outline: path 不能为空"), nil
	}
	data, err := host.Workspace().ReadFile(ctx, args.Path)
	if err != nil {
		return pluginkit.ErrorResult("file_outline: 读取 %s 失败: %v", args.Path, err), nil
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	ext := strings.ToLower(fileExt(args.Path))
	var lines []string
	switch {
	case ext == ".md" || ext == ".markdown":
		lines = outlineMarkdown(text)
	case ext == ".go":
		lines = outlineGo(text)
	case ext == ".json" || ext == ".yaml" || ext == ".yml":
		lines = outlineIndented(text, 3)
	default:
		lines = outlineIndented(text, 2)
	}
	if len(lines) == 0 {
		return pluginkit.TextResult("%s：未识别出结构（空文件或无大纲特征）", args.Path), nil
	}
	return pluginkit.TextResult("%s 大纲（%d 项）：\n%s", args.Path, len(lines), strings.Join(lines, "\n")), nil
}

// outlineMarkdown 标题树。
func outlineMarkdown(text string) []string {
	var out []string
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			level := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
			out = append(out, fmt.Sprintf("%d:%s%s %s", i+1, strings.Repeat("  ", level-1), strings.Repeat("#", level), strings.TrimSpace(trimmed[level:])))
		}
	}
	return out
}

// outlineGo 顶层声明（func/type/const/var 块 + 方法接收者）。
func outlineGo(text string) []string {
	var out []string
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "func "):
			out = append(out, fmt.Sprintf("%d:  %s", i+1, pluginkit.FirstLine(trimmed, 120)))
		case strings.HasPrefix(trimmed, "type ") && (strings.Contains(trimmed, " struct") ||
			strings.Contains(trimmed, " interface") || strings.Contains(trimmed, " =")):
			out = append(out, fmt.Sprintf("%d:  %s", i+1, pluginkit.FirstLine(trimmed, 120)))
		case strings.HasPrefix(trimmed, "const ") || strings.HasPrefix(trimmed, "var "):
			if strings.Contains(trimmed, "=") || strings.HasSuffix(trimmed, "(") {
				out = append(out, fmt.Sprintf("%d:  %s", i+1, pluginkit.FirstLine(trimmed, 120)))
			}
		}
	}
	return out
}

// outlineIndented 缩进层级大纲（取缩进 ≤ maxIndent 的行为节点）。
func outlineIndented(text string, maxIndent int) []string {
	var out []string
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") && extIsCommentStyle(text) {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent <= maxIndent {
			out = append(out, fmt.Sprintf("%d:%s%s", i+1, strings.Repeat("  ", indent), pluginkit.FirstLine(trimmed, 100)))
		}
	}
	return out
}

func extIsCommentStyle(text string) bool { return false } // 占位：JSON/YAML 无注释语义，保留扩展位

func fileExt(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			if j := strings.LastIndexAny(path[:i], "/\\"); j >= 0 || i == 0 {
				return path[i:]
			}
		}
		if path[i] == '/' || path[i] == '\\' {
			return ""
		}
	}
	return ""
}
