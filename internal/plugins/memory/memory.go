package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

func validLayer(l string) bool {
	switch l {
	case LayerCore, LayerNotes, LayerArchive:
		return true
	}
	return false
}

// FileKey 生成稳定文件名（供装配点同步器计算 retrieval key）。
func FileKey(layer, title string, at time.Time) string {
	slug := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(strings.TrimSpace(title)))
	slug = strings.Trim(slug, "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	if slug == "" {
		slug = "memo"
	}
	return filepath.ToSlash(filepath.Join(layer, fmt.Sprintf("%s-%04d%02d%02d.md", slug, at.Year(), at.Month(), at.Day())))
}

func invokeWrite(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Layer      string  `json:"layer"`
		Title      string  `json:"title"`
		Body       string  `json:"body"`
		Importance float64 `json:"importance"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("memory_write: %v", err), nil
	}
	if !validLayer(args.Layer) {
		return pluginkit.ErrorResult("memory_write: layer 非法 %q（可选 core|notes|archive）", args.Layer), nil
	}
	if strings.TrimSpace(args.Title) == "" || strings.TrimSpace(args.Body) == "" {
		return pluginkit.ErrorResult("memory_write: title 与 body 不能为空"), nil
	}
	if args.Importance < 0 {
		args.Importance = 0.5
	}
	if args.Importance > 1 {
		args.Importance = 1
	}
	rel := FileKey(args.Layer, args.Title, time.Now())
	doc := fmt.Sprintf("# %s\n\n%s\n\n<!-- importance: %.2f -->\n", strings.TrimSpace(args.Title), strings.TrimSpace(args.Body), args.Importance)
	if err := host.Workspace().WriteFile(ctx, MemoryDir+"/"+rel, []byte(doc), 0o644); err != nil {
		return pluginkit.ErrorResult("memory_write: 写入失败: %v", err), nil
	}
	return pluginkit.TextResult("已写入记忆 %s（下次启动同步进检索索引，D-3 召回可命中）", rel), nil
}

func invokeList(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Layer string `json:"layer"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("memory_list: %v", err), nil
	}
	if args.Layer != "" && !validLayer(args.Layer) {
		return pluginkit.ErrorResult("memory_list: layer 非法 %q", args.Layer), nil
	}
	layers := []string{args.Layer}
	if args.Layer == "" {
		layers = []string{LayerCore, LayerNotes, LayerArchive}
	}
	var files []string
	for _, l := range layers {
		entries, err := host.Workspace().List(ctx, MemoryDir+"/"+l)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir {
				files = append(files, l+"/"+baseName(e.Path))
			}
		}
	}
	if len(files) == 0 {
		return pluginkit.TextResult("记忆库为空（memory_write 写入第一条）"), nil
	}
	sort.Strings(files)
	return pluginkit.TextResult("记忆文件（%d 条）：\n%s", len(files), strings.Join(files, "\n")), nil
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}
