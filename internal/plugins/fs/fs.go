package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"harness-go/internal/globsyntax"
	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

// 默认参数。
const (
	defaultReadLimit = 2000
	defaultGlobLimit = 200
	defaultGrepHits  = 100
	// maxReadBytes 单次读取上限（防巨型文件撑爆上下文）。
	maxReadBytes = 512 << 10
)

// invokeReadFile 读取文件（行号标注 + offset_line/max_lines 分段）。
func invokeReadFile(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Path      string `json:"path"`
		OffsetLine int   `json:"offset_line"`
		MaxLines   int   `json:"max_lines"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("read_file: %v", err), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return pluginkit.ErrorResult("read_file: path 不能为空"), nil
	}
	data, err := host.Workspace().ReadFile(ctx, args.Path)
	if err != nil {
		return pluginkit.ErrorResult("read_file: 读取 %s 失败: %v", args.Path, err), nil
	}
	if len(data) > maxReadBytes {
		data = data[:maxReadBytes]
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	offset := args.OffsetLine
	if offset < 1 {
		offset = 1
	}
	limit := args.MaxLines
	if limit <= 0 {
		limit = defaultReadLimit
	}
	if offset > len(lines) {
		return pluginkit.TextResult("%s：共 %d 行，offset %d 越界（文件为空或偏移过大）", args.Path, len(lines), offset), nil
	}
	end := offset - 1 + limit
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, strings.TrimSuffix(lines[i], "\r"))
	}
	truncated := ""
	if end < len(lines) {
		truncated = fmt.Sprintf("\n…[共 %d 行，已读 %d-%d；用 offset 继续读取]", len(lines), offset, end)
	}
	binaryNote := ""
	if isBinary(data) {
		binaryNote = "\n（检测到二进制内容，输出可能不可读）"
	}
	return pluginkit.TextResult("%s%s%s", b.String(), binaryNote, truncated), nil
}

// invokeListDir 列出目录。
func invokeListDir(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("list_dir: %v", err), nil
	}
	rel := args.Path
	if strings.TrimSpace(rel) == "" {
		rel = "."
	}
	entries, err := host.Workspace().List(ctx, rel)
	if err != nil {
		return pluginkit.ErrorResult("list_dir: 列举 %s 失败: %v", rel, err), nil
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir // 目录在前
		}
		return entries[i].Path < entries[j].Path
	})
	if len(entries) == 0 {
		return pluginkit.TextResult("%s：（空目录）", rel), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s（%d 项）：\n", rel, len(entries))
	for _, e := range entries {
		name := e.Path
		if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
			name = name[i+1:]
		}
		if e.IsDir {
			b.WriteString("[目录] " + name + "/\n")
			continue
		}
		fmt.Fprintf(&b, "%8d  %s\n", e.Size, name)
	}
	return pluginkit.TextResult("%s", strings.TrimRight(b.String(), "\n")), nil
}

// anchorSnapshots 写入前登记 before 锚点快照（S10 铁律：**先快照后落盘**，
// 与文件写事务一致——快照失败拒绝写入，否则回滚会丢内容）。
func anchorSnapshots(ctx context.Context, host toolapi.Host, tool, path string) (*toolapi.SnapshotRef, *toolapi.Result) {
	st := host.Snapshots()
	if st == nil {
		return nil, pluginkit.ErrorResult("%s: 快照存储未接线（内核未注入 Host.Snapshots），拒绝写入以防回滚丢内容", tool)
	}
	reg, ok := st.(toolapi.SnapshotRegistrar)
	if !ok {
		return nil, pluginkit.ErrorResult("%s: 快照存储不支持锚点登记，拒绝写入", tool)
	}
	b, err := reg.CreateAnchored(ctx, []string{path}, toolapi.SnapshotOpt{Kind: "before"})
	if err != nil {
		return nil, pluginkit.ErrorResult("%s: 写入前快照失败（拒绝写入）: %v", tool, err)
	}
	return &b, nil
}

// registerAfterSnapshot 写入成功后登记 after 锚点（reapply 方向）。
// 失败返回错误描述（调用方必须显式回错，不得静默）。
func registerAfterSnapshot(ctx context.Context, host toolapi.Host, path, beforeID string) (map[string]any, string) {
	after, err := host.Snapshots().(toolapi.SnapshotRegistrar).CreateAnchored(ctx, []string{path}, toolapi.SnapshotOpt{Kind: "after"})
	if err != nil {
		return nil, err.Error()
	}
	return map[string]any{"before": beforeID, "after": after.ID}, ""
}

// invokeWriteFile 写入（创建或覆盖）+ diff。
func invokeWriteFile(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("write_file: %v", err), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return pluginkit.ErrorResult("write_file: path 不能为空"), nil
	}
	ws := host.Workspace()
	old := ""
	exists, _ := ws.Exists(ctx, args.Path)
	if exists {
		data, rerr := ws.ReadFile(ctx, args.Path)
		if rerr == nil {
			old = string(data)
		}
	}
	before, eRes := anchorSnapshots(ctx, host, "write_file", args.Path)
	if eRes != nil {
		return eRes, nil
	}
	newContent := strings.ReplaceAll(args.Content, "\r\n", "\n")
	if err := ws.WriteFile(ctx, args.Path, []byte(newContent), 0o644); err != nil {
		return pluginkit.ErrorResult("write_file: 写入 %s 失败: %v", args.Path, err), nil
	}
	snaps, snapErr := registerAfterSnapshot(ctx, host, args.Path, before.ID)
	if snapErr != "" {
		return pluginkit.ErrorResult("write_file: 内容已写入 %s，但写入后快照登记失败（该写入可回滚、不可 reapply）: %s", args.Path, snapErr), nil
	}
	d := pluginkit.BuildFileDiff(args.Path, old, newContent)
	verb := "已写入"
	if exists {
		verb = "已覆盖"
	}
	content := fmt.Sprintf("%s %s（+%d/-%d 行）\n\n%s", verb, args.Path, d.Additions, d.Deletions, pluginkit.UnifiedDiff(d))
	res := pluginkit.DataResult(content, map[string]any{"diff": d, "snapshots": snaps})
	return res, nil
}

// invokeEditFile 精确替换编辑（old_string 唯一命中；replace_all=true 时全量替换）。
func invokeEditFile(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("edit_file: %v", err), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return pluginkit.ErrorResult("edit_file: path 不能为空"), nil
	}
	if args.OldString == "" {
		return pluginkit.ErrorResult("edit_file: old_string 不能为空（创建新文件请用 write_file）"), nil
	}
	ws := host.Workspace()
	data, err := ws.ReadFile(ctx, args.Path)
	if err != nil {
		return pluginkit.ErrorResult("edit_file: 读取 %s 失败: %v", args.Path, err), nil
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	count := strings.Count(text, args.OldString)
	switch {
	case count == 0:
		return pluginkit.ErrorResult("edit_file: old_string 在 %s 中未命中（先 read_file 确认原文）", args.Path), nil
	case count == 1:
		// 唯一命中：这两条路径行为等价，直接走单次替换。
	case args.ReplaceAll:
		// 显式要求全量替换：不再要求唯一性。
		// 这是"唯一命中"约束的唯一合法出口——缺了它，批量替换只能靠模型
		// 手动重复调用 N 次，既慢又容易漏（且每次都要求唯一，直接死循环）。
	default:
		return pluginkit.ErrorResult("edit_file: old_string 在 %s 中命中 %d 次，必须唯一"+
			"（扩大上下文，或传 replace_all=true 全量替换）", args.Path, count), nil
	}
	var newText string
	if args.ReplaceAll {
		newText = strings.ReplaceAll(text, args.OldString, args.NewString)
	} else {
		newText = strings.Replace(text, args.OldString, args.NewString, 1)
	}
	before, eRes := anchorSnapshots(ctx, host, "edit_file", args.Path)
	if eRes != nil {
		return eRes, nil
	}
	if err := ws.WriteFile(ctx, args.Path, []byte(newText), 0o644); err != nil {
		return pluginkit.ErrorResult("edit_file: 写回 %s 失败: %v", args.Path, err), nil
	}
	snaps, snapErr := registerAfterSnapshot(ctx, host, args.Path, before.ID)
	if snapErr != "" {
		return pluginkit.ErrorResult("edit_file: 内容已写回 %s，但写入后快照登记失败（该编辑可回滚、不可 reapply）: %s", args.Path, snapErr), nil
	}
	d := pluginkit.BuildFileDiff(args.Path, text, newText)
	content := fmt.Sprintf("已编辑 %s（+%d/-%d 行）\n\n%s", args.Path, d.Additions, d.Deletions, pluginkit.UnifiedDiff(d))
	return pluginkit.DataResult(content, map[string]any{"diff": d, "snapshots": snaps}), nil
}

// invokeGlob 模式匹配。
//
// 语义委托给 globsyntax 包（glob 是一门独立小语言，不归属任何具体工具）。
//
// 关键行为契约（此前踩过坑，务必保持）：
//   - 模式**非法** → ErrorResult，并在文案里点明不支持的写法。
//   - 模式合法但**无匹配** → TextResult「无匹配文件」。
//
// 二者的区别至关重要：旧实现用 filepath.Match 不认识 {a,b}，把「语法不支持」
// 混成「文件不存在」返回，模型据此误判项目无代码（会话 9ee34e00 首轮即中招）。
func invokeGlob(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Limit   int    `json:"limit"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("glob: %v", err), nil
	}
	pattern := strings.TrimSpace(args.Pattern)
	if pattern == "" {
		return pluginkit.ErrorResult("glob: pattern 不能为空"), nil
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultGlobLimit
	}
	// 编译期就把语法问题暴露出来，避免"跑到一半才发现"。
	pat, err := globsyntax.Compile(pattern)
	if err != nil {
		return pluginkit.ErrorResult("glob: 模式语法错误: %v。"+
			"支持的写法：* / ** / ? / {a,b} / [abc] / 反斜杠转义", err), nil
	}
	// glob 遍历的起点必须是**本次调用的生效根**：ctx 有会话根（用户选的项目目录）
	// 就用它，否则回落进程根。用 Root() 会忽略会话根，导致"选了目录仍只扫启动目录"。
	root := workspaceRootFor(ctx, host.Workspace())
	matches := globWalk(root, pat, limit+1)

	truncated := ""
	if len(matches) > limit {
		matches = matches[:limit]
		truncated = fmt.Sprintf("\n…[超过 %d 条，已截断；请缩小模式范围]", limit)
	}
	if len(matches) == 0 {
		return pluginkit.TextResult("无匹配文件（模式 %s）", pattern), nil
	}
	return pluginkit.TextResult("匹配 %d 个文件：\n%s%s", len(matches), strings.Join(matches, "\n"), truncated), nil
}

// globWalk 遍历 root，收集相对路径匹配 pat 的**文件**（不含目录）。
//
// 与旧实现的根本差异：不再逐段 filepath.Match，而是整条相对路径与编译后的
// 正则一次性比对。这带来三个结构性好处：
//  1. ** 与 {a,b} 天然正确，无需为每种语法写特例；
//  2. 剪枝逻辑与匹配逻辑解耦，正确性可独立单测；
//  3. 命中上限在遍历侧就能提前收敛（reachLimit），不必先收集再截断。
func globWalk(root string, pat *globsyntax.Pattern, limit int) []string {
	var matches []string
	reached := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if reached || werr != nil || d == nil {
			return nil
		}
		if d.IsDir() {
			if skipGlobDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		if pat.Match(filepath.ToSlash(rel)) {
			matches = append(matches, filepath.ToSlash(rel))
			if len(matches) >= limit {
				reached = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	sort.Strings(matches)
	return matches
}

// skipGlobDir 判断目录是否应在 glob 遍历时剪枝。
//
// .git* 必须跳过（元数据不是用户代码，且体量巨大）；
// node_modules / target / dist 等构建产物目录同样跳过——这与 grep 侧
// 的跳过策略保持同一口径，避免两处规则漂移。
func skipGlobDir(name string) bool {
	if strings.HasPrefix(name, ".git") {
		return true
	}
	switch name {
	case "node_modules", "vendor", "target", "dist", "build":
		return true
	}
	return false
}

// invokeGrep 全文检索（支持文件名过滤与上下文行）。
func invokeGrep(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Pattern      string `json:"pattern"`
		Path         string `json:"path"`
		Literal      bool   `json:"literal"`
		MaxResults   int    `json:"max_results"`
		Include      string `json:"include"`
		ContextLines int    `json:"context_lines"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("grep: %v", err), nil
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return pluginkit.ErrorResult("grep: pattern 不能为空"), nil
	}
	maxHits := args.MaxResults
	if maxHits <= 0 {
		maxHits = defaultGrepHits
	}
	// context_lines 硬性夹在 0~5：上下文是"看清一行周围"，不是"把整个文件搬进上下文"。
	// 上限写死而非交给调用方，避免一次 grep 把预算吃光。
	ctxLines := args.ContextLines
	if ctxLines < 0 {
		ctxLines = 0
	}
	if ctxLines > 5 {
		ctxLines = 5
	}
	// include 用 globsyntax 编译（与 glob 工具同一套语法，避免"两处 glob 规则漂移"）。
	var includePat *globsyntax.Pattern
	if inc := strings.TrimSpace(args.Include); inc != "" {
		pat, cerr := globsyntax.Compile(inc)
		if cerr != nil {
			return pluginkit.ErrorResult("grep: include 模式语法错误: %v（支持的写法：* / ** / ? / {a,b} / [abc]）", cerr), nil
		}
		includePat = pat
	}
	expr := args.Pattern
	if args.Literal {
		expr = regexp.QuoteMeta(expr)
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return pluginkit.ErrorResult("grep: 正则非法: %v", err), nil
	}
	base := args.Path
	if strings.TrimSpace(base) == "" {
		base = "."
	}
	// grep 的根与基线同样取**生效根**（见 glob 处的说明）。
	root := workspaceRootFor(ctx, host.Workspace())
	absBase, err := workspaceResolveFor(ctx, host.Workspace(), base)
	if err != nil {
		return pluginkit.ErrorResult("grep: %v", err), nil
	}
	var hits []string
	scanned := 0
	_ = filepath.WalkDir(absBase, func(path string, d os.DirEntry, werr error) error {
		if len(hits) >= maxHits {
			return filepath.SkipAll
		}
		if werr != nil || d == nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			// 与 glob 侧共用同一张跳过表：两处各写一套必然漂移
			// （曾出现 grep 跳过 node_modules 而 glob 不跳的情形）。
			if name == "data" || skipGlobDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if isSkippableExt(filepath.Ext(name)) {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		// include 过滤：按相对路径匹配（支持 **/*.go 这类跨目录写法）。
		if includePat != nil && !includePat.Match(relSlash) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil || len(data) > 2<<20 || isBinary(data) {
			return nil
		}
		scanned++
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			hits = append(hits, formatGrepHit(relSlash, i, lines, ctxLines))
			if len(hits) >= maxHits {
				break
			}
		}
		return nil
	})
	if len(hits) == 0 {
		return pluginkit.TextResult("无命中（模式 %q，扫描 %d 个文件）", args.Pattern, scanned), nil
	}
	truncated := ""
	if len(hits) >= maxHits {
		truncated = fmt.Sprintf("\n…[达到上限 %d 条]", maxHits)
	}
	return pluginkit.TextResult("命中 %d 条：\n%s%s", len(hits), strings.Join(hits, "\n"), truncated), nil
}

// formatGrepHit 渲染一条命中；ctxLines>0 时附上前后文并标注命中行。
//
// 输出形态刻意保持 `path:line: 内容` 的既有契约（前端与模型都依赖），
// 上下文行以 `  ` 前缀缩进区分，命中行以 `>` 标记，保证机器与人都能读。
func formatGrepHit(rel string, lineIdx int, lines []string, ctxLines int) string {
	head := fmt.Sprintf("%s:%d: %s", rel, lineIdx+1, pluginkit.FirstLine(lines[lineIdx], 200))
	if ctxLines == 0 {
		return head
	}
	var b strings.Builder
	b.WriteString(head)
	start := lineIdx - ctxLines
	if start < 0 {
		start = 0
	}
	end := lineIdx + ctxLines
	if end >= len(lines) {
		end = len(lines) - 1
	}
	for i := start; i <= end; i++ {
		prefix := "  "
		if i == lineIdx {
			prefix = "> "
		}
		b.WriteString(fmt.Sprintf("\n  %s%d| %s", prefix, i+1, pluginkit.FirstLine(lines[i], 200)))
	}
	return b.String()
}

// workspaceRootFor 返回本次调用的生效执行根。
//
// ctx 有会话根（用户选的项目目录）就用它，否则回落进程根 —— 后者保证所有
// 未接会话上下文的调用点（测试、后台任务）行为与改造前完全一致。
func workspaceRootFor(ctx context.Context, ws toolapi.Workspace) string {
	if cw, ok := ws.(toolapi.ContextWorkspace); ok {
		return cw.RootFor(ctx)
	}
	return ws.Root()
}

// workspaceResolveFor 按生效根解析相对路径（沙箱防护仍由载体执行）。
func workspaceResolveFor(ctx context.Context, ws toolapi.Workspace, rel string) (string, error) {
	if cw, ok := ws.(toolapi.ContextWorkspace); ok {
		return cw.ResolveFor(ctx, rel)
	}
	return ws.Resolve(rel)
}

// 文本启发：含 NUL 字节即视为二进制。
func isBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

func isSkippableExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".png", ".jpg", ".jpeg", ".gif", ".ico", ".woff", ".woff2", ".ttf",
		".zip", ".gz", ".exe", ".dll", ".pdf", ".db", ".lock":
		return true
	}
	return false
}
