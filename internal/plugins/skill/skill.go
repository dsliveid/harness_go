package skill

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

func validSkillName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// invokeList 列出技能（目录名 + 描述首行）。
func invokeList(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	entries, err := host.Workspace().List(ctx, SkillsDir)
	if err != nil || len(entries) == 0 {
		return pluginkit.TextResult("技能库为空（用 skill_save 沉淀第一个技能）"), nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir {
			names = append(names, baseName(e.Path))
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return pluginkit.TextResult("技能库为空（用 skill_save 沉淀第一个技能）"), nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("技能库（%d 项）：\n", len(names)))
	for _, n := range names {
		desc := ""
		if data, rerr := host.Workspace().ReadFile(ctx, SkillsDir+"/"+n+"/SKILL.md"); rerr == nil {
			desc = skillDescription(string(data))
		}
		fmt.Fprintf(&b, "- %s：%s\n", n, desc)
	}
	return pluginkit.TextResult("%s", strings.TrimRight(b.String(), "\n")), nil
}

// invokeRead 读取技能全文。
func invokeRead(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("skill_read: %v", err), nil
	}
	if !validSkillName(args.Name) {
		return pluginkit.ErrorResult("skill_read: 技能名非法 %q", args.Name), nil
	}
	data, err := host.Workspace().ReadFile(ctx, SkillsDir+"/"+args.Name+"/SKILL.md")
	if err != nil {
		return pluginkit.ErrorResult("skill_read: 技能 %q 不存在（skill_list 查看全部）", args.Name), nil
	}
	return pluginkit.TextResult("%s", string(data)), nil
}

// invokeSave 沉淀技能（写侧）。
func invokeSave(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("skill_save: %v", err), nil
	}
	if !validSkillName(args.Name) {
		return pluginkit.ErrorResult("skill_save: 技能名非法（小写字母数字连字符，≤64 字符）: %q", args.Name), nil
	}
	if strings.TrimSpace(args.Description) == "" || strings.TrimSpace(args.Body) == "" {
		return pluginkit.ErrorResult("skill_save: description 与 body 不能为空"), nil
	}
	doc := fmt.Sprintf("# %s\n\n%s\n\n%s\n", args.Name,
		pluginkit.FirstLine(args.Description, 200), strings.TrimSpace(args.Body))
	if err := host.Workspace().WriteFile(ctx, SkillsDir+"/"+args.Name+"/SKILL.md", []byte(doc), 0o644); err != nil {
		return pluginkit.ErrorResult("skill_save: 写入失败: %v", err), nil
	}
	return pluginkit.TextResult("已沉淀技能 %q（skill_read %q 可复读）", args.Name, args.Name), nil
}

// skillDescription 提取「何时该用」描述（首个非标题非空行）。
func skillDescription(md string) string {
	for _, ln := range strings.Split(md, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return pluginkit.FirstLine(t, 120)
	}
	return "（无描述）"
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}
