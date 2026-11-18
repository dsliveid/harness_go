// Package service 的技能用例：把 `.harness/skills/<name>/SKILL.md` 这一**唯一真源**
// 暴露成 HTTP 能力，供 Web 端（服务端节点）列出 / 读取 / 安装 / 卸载技能。
//
// 为什么这么做（而不是另建目录）：技能的真源由 internal/plugins/skill 定义
// （factory.go:16 `SkillsDir = ".harness/skills"`），Agent 通过 skill_list /
// skill_read / skill_save 读写它。Web 端若另建目录，就会出现「Agent 沉淀的技能
// 界面看不到、界面装的技能 Agent 用不到」的双真源撕裂。
//
// 分层约束（.golangci.yml depguard）：service 不得 import internal/plugins，
// 因此本文件**不引用** skill 插件的任何符号，而是注入同一个 toolapi.Workspace
// 实例 —— 与插件共用同一根、同一套沙箱防护（越界即拒绝），天然同源。
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"harness-go/internal/apperr"
	"harness-go/pkg/toolapi"
)

// SkillsDirRel 技能库相对目录（与 internal/plugins/skill.SkillsDir 必须逐字一致）。
//
// 之所以这里再写一份常量而不 import 插件包：depguard 禁止 service → plugins，
// 且注释已双向标注，改动任一处必须同步另一处（测试 SkillDirMatchesPlugin 守门）。
const SkillsDirRel = ".harness/skills"

// SkillFileName 技能正文文件名（与插件一致）。
const SkillFileName = "SKILL.md"

// SkillView 技能视图（供前端渲染）。
type SkillView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Origin 来源：builtin = 随程序发布的内置技能；local = 本地目录导入/Agent 沉淀。
	// 与工具包的 origin 口径保持一致，前端据此区分「内置」与「市场/已安装」。
	Origin   string `json:"origin"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	ModTime  int64  `json:"mod_time"`
}

// SkillServiceOptions 依赖。
type SkillServiceOptions struct {
	// Workspace 服务端工作区（与技能插件共用同一实例，保证单一真源）。
	Workspace toolapi.Workspace
}

// SkillService 技能用例。
//
// 🚨 执行根语义（重要，别踩坑）：
//   技能插件走 `Workspace.List/ReadFile/WriteFile(ctx, ...)`，hostimpl 在这些方法里
//   用 `ResolveFor(ctx, ...)` → **优先会话根**（tools.Runner 按会话 project_dir 注入）。
//   而本 HTTP 通道没有会话上下文，ctx 里没有会话根 → 一律落到**进程根**（启动目录）。
//
//   因此「界面看到的技能库」= 进程根下的 .harness/skills，
//   而「Agent 在某项目会话里沉淀的技能」= 该项目目录下的 .harness/skills。
//   这是**真实边界**，不是 bug：Web 端没有「当前会话」概念，不可能凭空知道项目根。
//   若将来要做「按项目查看技能」，正确做法是给这些端点加可选 query `?project_id=`，
//   由 ProjectService 解析出目录后 `toolapi.WithWorkspaceRoot(ctx, dir)` 再调用 —— 
//   即复用同一套 ctx 注入机制，绝不另造路径规则。
type SkillService struct {
	ws toolapi.Workspace
}

// Root 返回当前生效的技能库绝对根（供前端明示「技能落在哪里」）。
func (s *SkillService) Root() string {
	if s.ws == nil {
		return ""
	}
	return s.ws.Root()
}

// NewSkillService 创建技能用例。
func NewSkillService(opts SkillServiceOptions) *SkillService {
	return &SkillService{ws: opts.Workspace}
}

// List 列出技能库全部技能（按名称升序；目录不存在视为空，不是错误）。
func (s *SkillService) List(ctx context.Context) ([]SkillView, error) {
	entries, err := s.ws.List(ctx, SkillsDirRel)
	if err != nil {
		// 目录不存在 = 技能库为空（全新部署的正常状态），绝不报错。
		if errors.Is(err, os.ErrNotExist) {
			return []SkillView{}, nil
		}
		return nil, err
	}
	out := make([]SkillView, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		name := baseName(e.Path)
		if !validSkillName(name) {
			continue // 非法目录名不是技能（可能是用户手放的杂物），跳过而非报错
		}
		view := SkillView{
			Name:   name,
			Origin: "local",
			Path:   SkillsDirRel + "/" + name + "/" + SkillFileName,
		}
		if data, rerr := s.ws.ReadFile(ctx, view.Path); rerr == nil {
			view.Description = skillDescription(string(data))
			view.Size = int64(len(data))
		}
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Read 读取技能全文（含 SKILL.md 原文）。
func (s *SkillService) Read(ctx context.Context, name string) (SkillView, string, error) {
	if !validSkillName(name) {
		return SkillView{}, "", invalidArg("技能名非法（小写字母数字连字符，≤64 字符）: %q", name)
	}
	rel := SkillsDirRel + "/" + name + "/" + SkillFileName
	data, err := s.ws.ReadFile(ctx, rel)
	if err != nil {
		return SkillView{}, "", apperr.New(apperr.CodeNotFound, "技能 %s 不存在", name)
	}
	content := string(data)
	return SkillView{
		Name: name, Description: skillDescription(content), Origin: "local",
		Path: rel, Size: int64(len(data)),
	}, content, nil
}

// SaveInput 保存技能入参。
type SaveInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// Save 沉淀/覆盖技能：写 `.harness/skills/<name>/SKILL.md`。
//
// 文档结构必须与插件 skill_save 完全一致（`# name\n\n描述\n\n正文\n`），
// 否则 Agent 用 skill_read 读到的描述行会对不上。
func (s *SkillService) Save(ctx context.Context, in SaveInput) (SkillView, error) {
	if !validSkillName(in.Name) {
		return SkillView{}, invalidArg("技能名非法（小写字母数字连字符，≤64 字符）: %q", in.Name)
	}
	if strings.TrimSpace(in.Description) == "" || strings.TrimSpace(in.Body) == "" {
		return SkillView{}, invalidArg("description 与 body 不能为空")
	}
	desc := firstLine(in.Description, 200)
	body := strings.TrimSpace(in.Body)
	doc := fmt.Sprintf("# %s\n\n%s\n\n%s\n", in.Name, desc, body)
	rel := SkillsDirRel + "/" + in.Name + "/" + SkillFileName
	if err := s.ws.WriteFile(ctx, rel, []byte(doc), 0o644); err != nil {
		return SkillView{}, err
	}
	return SkillView{
		Name: in.Name, Description: skillDescription(doc), Origin: "local",
		Path: rel, Size: int64(len(doc)),
	}, nil
}

// Delete 卸载技能（删除整个技能目录，含附属脚本）。
//
// 为什么删目录而不是只删 SKILL.md：技能可携带脚本（py/sh/ps1），
// 只删正文会留下孤儿文件，下次同名安装会混入旧脚本。
func (s *SkillService) Delete(ctx context.Context, name string) error {
	if !validSkillName(name) {
		return invalidArg("技能名非法（小写字母数字连字符，≤64 字符）: %q", name)
	}
	rel := SkillsDirRel + "/" + name
	if err := s.ws.Remove(ctx, rel); err != nil {
		return apperr.New(apperr.CodeNotFound, "技能 %s 不存在或删除失败", name)
	}
	return nil
}

// Has 判断技能是否已安装（供市场安装前查重）。
func (s *SkillService) Has(ctx context.Context, name string) bool {
	if !validSkillName(name) {
		return false
	}
	ok, err := s.ws.Exists(ctx, SkillsDirRel+"/"+name+"/"+SkillFileName)
	return err == nil && ok
}

// validSkillName 技能名校验（与 internal/plugins/skill.validSkillName 逐条一致）。
//
// 之所以重复实现：depguard 禁止 service → plugins。两边任一改动必须同步
// （其行为差异会被 skill_integration_test 的「非法名被拒」用例暴露）。
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

// skillDescription 取首个非标题非空行作为「何时该用」描述（与插件同名函数一致）。
func skillDescription(md string) string {
	for _, ln := range strings.Split(md, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return firstLine(t, 120)
	}
	return "（无描述）"
}

// firstLine 截断首行到 n 字符（与 pluginkit.FirstLine 语义一致）。
func firstLine(s string, n int) string {
	t := strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if len(t) > n {
		return t[:n]
	}
	return t
}

// baseName 取路径末段（同时兼容 / 与 \）。
func baseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}
