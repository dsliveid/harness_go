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

// listSkills 列出技能（目录名 + 描述首行）。
func listSkills(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	entries, err := host.Workspace().List(ctx, SkillsDir)
	if err != nil || len(entries) == 0 {
		return pluginkit.TextResult("技能库为空（用 save_skill 沉淀第一个技能）"), nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir {
			names = append(names, baseName(e.Path))
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return pluginkit.TextResult("技能库为空（用 save_skill 沉淀第一个技能）"), nil
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

// runSkill 加载技能全文到上下文，供模型按步骤执行。
//
// 命名说明：技能是**给模型照做的流程说明**（Markdown 步骤），不是可执行代码。
// 取名 run_skill 而非 read_skill，是为了贴合模型的真实动作语义——
// 它拿到文本后紧接着就会"照着跑一遍"，而不是"读完就搁置"。
// 因此这里刻意不声称"执行成功"：真正的执行发生在模型后续的工具调用里。
func runSkill(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("run_skill: %v", err), nil
	}
	if !validSkillName(args.Name) {
		return pluginkit.ErrorResult("run_skill: 技能名非法 %q", args.Name), nil
	}
	data, err := host.Workspace().ReadFile(ctx, SkillsDir+"/"+args.Name+"/SKILL.md")
	if err != nil {
		return pluginkit.ErrorResult("run_skill: 技能 %q 不存在（list_skills 查看全部）", args.Name), nil
	}
	return pluginkit.TextResult("已加载技能 %q 的步骤，请按下列流程执行：\n\n%s", args.Name, string(data)), nil
}

// saveSkill 沉淀技能（写侧）。
func saveSkill(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("save_skill: %v", err), nil
	}
	if !validSkillName(args.Name) {
		return pluginkit.ErrorResult("save_skill: 技能名非法（小写字母数字连字符，≤64 字符）: %q", args.Name), nil
	}
	if strings.TrimSpace(args.Description) == "" || strings.TrimSpace(args.Body) == "" {
		return pluginkit.ErrorResult("save_skill: description 与 body 不能为空"), nil
	}
	doc := fmt.Sprintf("# %s\n\n%s\n\n%s\n", args.Name,
		pluginkit.FirstLine(args.Description, 200), strings.TrimSpace(args.Body))
	if err := host.Workspace().WriteFile(ctx, SkillsDir+"/"+args.Name+"/SKILL.md", []byte(doc), 0o644); err != nil {
		return pluginkit.ErrorResult("save_skill: 写入失败: %v", err), nil
	}
	return pluginkit.TextResult("已沉淀技能 %q（run_skill %q 可加载）", args.Name, args.Name), nil
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
