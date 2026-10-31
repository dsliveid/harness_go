package growth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

// Entry 成长记录（jsonl 一行一条）。
type Entry struct {
	ID      int    `json:"id"`
	At      string `json:"at"`
	Lesson  string `json:"lesson"`
	Context string `json:"context,omitempty"`
	Applied bool   `json:"applied,omitempty"`
}

// read 解析全部记录（损坏行跳过）。
func read(ctx context.Context, host toolapi.Host) []Entry {
	data, err := host.Workspace().ReadFile(ctx, StorePath)
	if err != nil {
		return nil
	}
	var out []Entry
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var e Entry
		if json.Unmarshal([]byte(ln), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// write 整体重写（记录量小，追加语义由调用方拼接保证）。
func write(ctx context.Context, host toolapi.Host, entries []Entry) error {
	var b strings.Builder
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return host.Workspace().WriteFile(ctx, StorePath, []byte(b.String()), 0o644)
}

func invokeRecord(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Lesson  string `json:"lesson"`
		Context string `json:"context"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("record_growth: %v", err), nil
	}
	if strings.TrimSpace(args.Lesson) == "" {
		return pluginkit.ErrorResult("record_growth: lesson 不能为空"), nil
	}
	entries := read(ctx, host)
	nextID := 1
	if len(entries) > 0 {
		nextID = entries[len(entries)-1].ID + 1
	}
	entries = append(entries, Entry{
		ID: nextID, At: time.Now().Format(time.RFC3339),
		Lesson: strings.TrimSpace(args.Lesson), Context: strings.TrimSpace(args.Context),
	})
	if err := write(ctx, host, entries); err != nil {
		return pluginkit.ErrorResult("record_growth: 写入失败: %v", err), nil
	}
	return pluginkit.TextResult("已记录成长 #%d（共 %d 条）", nextID, len(entries)), nil
}

func invokeList(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Limit int `json:"limit"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("list_growths: %v", err), nil
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	entries := read(ctx, host)
	if len(entries) == 0 {
		return pluginkit.TextResult("暂无成长记录"), nil
	}
	// 未应用在前，各组内最近在前
	var pending, applied []Entry
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Applied {
			applied = append(applied, entries[i])
		} else {
			pending = append(pending, entries[i])
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "成长记录（共 %d 条，未应用 %d）：\n", len(entries), len(pending))
	for _, e := range pending {
		if limit <= 0 {
			break
		}
		fmt.Fprintf(&b, "#%d [待应用] %s（场景：%s）\n", e.ID, pluginkit.FirstLine(e.Lesson, 120), defaultStr(e.Context, "通用"))
		limit--
	}
	for _, e := range applied {
		if limit <= 0 {
			break
		}
		fmt.Fprintf(&b, "#%d [已应用] %s\n", e.ID, pluginkit.FirstLine(e.Lesson, 120))
		limit--
	}
	return pluginkit.TextResult("%s", strings.TrimRight(b.String(), "\n")), nil
}

func invokeApply(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		ID int `json:"id"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("apply_growth: %v", err), nil
	}
	entries := read(ctx, host)
	for i := range entries {
		if entries[i].ID == args.ID {
			if entries[i].Applied {
				return pluginkit.ErrorResult("apply_growth: #%d 已是应用状态", args.ID), nil
			}
			entries[i].Applied = true
			if err := write(ctx, host, entries); err != nil {
				return pluginkit.ErrorResult("apply_growth: 写入失败: %v", err), nil
			}
			return pluginkit.TextResult("成长 #%d 已标记应用（教训固化进流程）", args.ID), nil
		}
	}
	return pluginkit.ErrorResult("apply_growth: 编号 #%d 不存在", args.ID), nil
}

func defaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
