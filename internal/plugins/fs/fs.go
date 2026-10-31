package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

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

// invokeReadFile 读取文件（行号标注 + offset/limit 分段）。
func invokeReadFile(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
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
	offset := args.Offset
	if offset < 1 {
		offset = 1
	}
	limit := args.Limit
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

// invokeEditFile 精确替换编辑（old_string 唯一命中语义）。
func invokeEditFile(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
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
	switch count {
	case 0:
		return pluginkit.ErrorResult("edit_file: old_string 在 %s 中未命中（先 read_file 确认原文）", args.Path), nil
	case 1:
	default:
		return pluginkit.ErrorResult("edit_file: old_string 在 %s 中命中 %d 次，必须唯一（扩大上下文或改用 write_file）", args.Path, count), nil
	}
	newText := strings.Replace(text, args.OldString, args.NewString, 1)
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

// invokeGlob 模式匹配（支持 ** 跨目录）。
func invokeGlob(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Limit   int    `json:"limit"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("glob: %v", err), nil
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return pluginkit.ErrorResult("glob: pattern 不能为空"), nil
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultGlobLimit
	}
	root := host.Workspace().Root()
	matches, err := globWalk(root, filepath.ToSlash(strings.TrimSpace(args.Pattern)), limit+1)
	if err != nil {
		return pluginkit.ErrorResult("glob: 模式非法: %v", err), nil
	}
	truncated := ""
	if len(matches) > limit {
		matches = matches[:limit]
		truncated = fmt.Sprintf("\n…[超过 %d 条，已截断；请缩小模式范围]", limit)
	}
	if len(matches) == 0 {
		return pluginkit.TextResult("无匹配文件（模式 %s）", args.Pattern), nil
	}
	return pluginkit.TextResult("匹配 %d 个文件：\n%s%s", len(matches), strings.Join(matches, "\n"), truncated), nil
}

// globWalk 用 ** 语义遍历（标准库 filepath.Match 不支持 **，自行展开）。
func globWalk(root, pattern string, limit int) ([]string, error) {
	parts := strings.Split(pattern, "/")
	var matches []string
	var walk func(dir, rel string, idx int) error
	walk = func(dir, rel string, idx int) error {
		if len(matches) >= limit {
			return nil
		}
		if idx == len(parts) {
			return nil
		}
		part := parts[idx]
		last := idx == len(parts)-1
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil // 目录不可读不致命
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".git") {
				continue
			}
			childRel := name
			if rel != "" {
				childRel = rel + "/" + name
			}
			if part == "**" {
				// ** 匹配零层或多层目录
				if !last {
					if err := walk(dir, rel, idx+1); err != nil {
						return err
					}
				}
				if e.IsDir() {
					if err := walk(filepath.Join(dir, name), childRel, idx); err != nil {
						return err
					}
				}
				continue
			}
			ok, err := filepath.Match(part, name)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if last {
				if !e.IsDir() {
					matches = append(matches, childRel)
				}
				continue
			}
			if e.IsDir() {
				if err := walk(filepath.Join(dir, name), childRel, idx+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, "", 0); err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// invokeGrep 全文检索。
func invokeGrep(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Literal    bool   `json:"literal"`
		MaxResults int    `json:"max_results"`
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
	root := host.Workspace().Root()
	absBase, err := host.Workspace().Resolve(base)
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
			if name == ".git" || name == "node_modules" || name == "data" {
				return filepath.SkipDir
			}
			return nil
		}
		if isSkippableExt(filepath.Ext(name)) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil || len(data) > 2<<20 || isBinary(data) {
			return nil
		}
		scanned++
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		for i, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), i+1, pluginkit.FirstLine(line, 200)))
				if len(hits) >= maxHits {
					break
				}
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
