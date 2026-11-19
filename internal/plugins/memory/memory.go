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

// recordMemory 沉淀一条长期记忆。
func recordMemory(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Layer      string  `json:"layer"`
		Title      string  `json:"title"`
		Body       string  `json:"body"`
		Importance float64 `json:"importance"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("record_memory: %v", err), nil
	}
	if !validLayer(args.Layer) {
		return pluginkit.ErrorResult("record_memory: layer 非法 %q（可选 core|notes|archive）", args.Layer), nil
	}
	if strings.TrimSpace(args.Title) == "" || strings.TrimSpace(args.Body) == "" {
		return pluginkit.ErrorResult("record_memory: title 与 body 不能为空"), nil
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
		return pluginkit.ErrorResult("record_memory: 写入失败: %v", err), nil
	}
	// ---- 写后同步索引（分轴：只影响"可见性"，不影响本轮"注入"）----
	//
	// 落盘后立刻把这一条推进检索索引，否则会话中新写的记忆要等**重启**
	// 才能被召回 —— "边读边记"在单次对话内等于不生效。
	//
	// 但**绝不**在这里触发注入：MemoryCore/MemoryTail 是 Run 边界冻结的快照，
	// 本轮保持不变。理由有两条，缺一都会出问题：
	//   ① 重复注入——模型刚写完就知道内容，本轮再喂一遍纯属自我重复；
	//   ② 击穿前缀缓存——MemoryCore 进 system 稳定前缀，中途变动会让
	//      provider 的前缀缓存全部失效（每轮重算，成本显著上升）。
	// 因此正确语义是"本轮写的记忆，下一轮起可被召回"。
	indexed, indexMsg := false, "索引同步能力未接线（本条已落盘，将在下次启动时同步）"
	if idx, ok := host.(toolapi.MemoryIndexer); ok {
		indexed, indexMsg = idx.IndexMemoryFile(ctx, args.Layer, rel)
	}
	if indexed {
		return pluginkit.TextResult("已写入记忆 %s，并已同步进检索索引。\n"+
			"生效时机：本对话内不重复注入（避免把刚写的内容再喂回模型），下一轮起可被召回。", rel), nil
	}
	return pluginkit.TextResult("已写入记忆 %s（%s）。\n"+
		"生效时机：本对话内不重复注入，下一轮起可被召回。", rel, indexMsg), nil
}

// readMemory 检索记忆：有 query 按关键词匹配，无 query 列出文件清单。
func readMemory(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Query string `json:"query"`
		Layer string `json:"layer"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("read_memory: %v", err), nil
	}
	if args.Layer != "" && !validLayer(args.Layer) {
		return pluginkit.ErrorResult("read_memory: layer 非法 %q", args.Layer), nil
	}
	layers := []string{args.Layer}
	if args.Layer == "" {
		layers = []string{LayerCore, LayerNotes, LayerArchive}
	}
	query := strings.TrimSpace(args.Query)

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
		return pluginkit.TextResult("记忆库为空（先用 record_memory 写入第一条）"), nil
	}
	sort.Strings(files)

	// 无 query：列清单（是"有哪些记忆"，不是"记忆内容"）。
	if query == "" {
		return pluginkit.TextResult("记忆文件（%d 条）：\n%s", len(files), strings.Join(files, "\n")), nil
	}

	// 有 query：逐文件匹配标题与正文，返回命中摘要。
	// 这里做的是**文件级关键词匹配**，与内核 memory.Store 的向量检索是两条通道：
	// 后者面向自动召回（按相关性打分），本工具面向"模型主动查阅指定主题"。
	type hit struct {
		file  string
		title string
		line  string
	}
	var hits []hit
	needle := strings.ToLower(query)
	for _, f := range files {
		data, err := host.Workspace().ReadFile(ctx, MemoryDir+"/"+f)
		if err != nil {
			continue
		}
		text := string(data)
		if !strings.Contains(strings.ToLower(text), needle) {
			continue
		}
		hits = append(hits, hit{file: f, title: firstHeading(text), line: matchLine(text, needle)})
	}
	if len(hits) == 0 {
		return pluginkit.TextResult("没有记忆命中 %q（共 %d 条记忆文件）。可省略 query 列出全部。", query, len(files)), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "命中 %d 条记忆（关键词 %q）：\n", len(hits), query)
	for _, h := range hits {
		fmt.Fprintf(&b, "- [%s] %s：%s\n", h.file, h.title, h.line)
	}
	return pluginkit.TextResult("%s", strings.TrimRight(b.String(), "\n")), nil
}

// firstHeading 取 Markdown 首个标题（无标题回落文件名语义）。
func firstHeading(text string) string {
	for _, ln := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(ln); strings.HasPrefix(t, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(t, "# "))
		}
	}
	return "(无标题)"
}

// matchLine 返回首个命中关键词的行（截断到 100 字符，避免单条吃掉预算）。
func matchLine(text, needle string) string {
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if strings.Contains(strings.ToLower(t), needle) {
			if r := []rune(t); len(r) > 100 {
				return string(r[:100]) + "…"
			}
			return t
		}
	}
	return ""
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}
