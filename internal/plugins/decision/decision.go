package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

// Entry 决策记录。
type Entry struct {
	At           string   `json:"at"` // RFC3339
	Title        string   `json:"title"`
	Context      string   `json:"context,omitempty"`
	Choice       string   `json:"choice"`
	Alternatives []string `json:"alternatives,omitempty"`
	Reason       string   `json:"reason,omitempty"`
}

func invokeRecord(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args Entry
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("decision_record: %v", err), nil
	}
	if strings.TrimSpace(args.Title) == "" || strings.TrimSpace(args.Choice) == "" {
		return pluginkit.ErrorResult("decision_record: title 与 choice 不能为空"), nil
	}
	args.At = time.Now().Format(time.RFC3339)
	line, err := json.Marshal(args)
	if err != nil {
		return pluginkit.ErrorResult("decision_record: 序列化失败: %v", err), nil
	}
	old := ""
	if data, rerr := host.Workspace().ReadFile(ctx, StorePath); rerr == nil {
		old = string(data)
	}
	newText := old
	if newText != "" && !strings.HasSuffix(newText, "\n") {
		newText += "\n"
	}
	newText += string(line) + "\n"
	if err := host.Workspace().WriteFile(ctx, StorePath, []byte(newText), 0o644); err != nil {
		return pluginkit.ErrorResult("decision_record: 写入失败: %v", err), nil
	}
	return pluginkit.TextResult("已记录决策：%s → %s", args.Title, args.Choice), nil
}

func invokeList(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Limit int `json:"limit"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("decision_list: %v", err), nil
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	data, err := host.Workspace().ReadFile(ctx, StorePath)
	if err != nil {
		return pluginkit.TextResult("暂无决策记录"), nil
	}
	var out []string
	total := 0
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		total++
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// 最近在前
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		ln := strings.TrimSpace(lines[i])
		if ln == "" {
			continue
		}
		var e Entry
		if json.Unmarshal([]byte(ln), &e) != nil {
			continue // 损坏行跳过（追加式日志允许部分损坏）
		}
		out = append(out, fmt.Sprintf("%s  %s → %s", e.At, e.Title, e.Choice))
	}
	if len(out) == 0 {
		return pluginkit.TextResult("暂无决策记录"), nil
	}
	return pluginkit.TextResult("决策记录（共 %d 条，显示最近 %d 条）：\n%s", total, len(out), strings.Join(out, "\n")), nil
}
