// Package sop 是 SOP（标准作业程序）的真源解析层。
//
// # 为什么存在这个包（架构意图）
//
// 改造前存在三处架构瑕疵，本包是它们的统一收口点：
//
//	① 层次倒置：内核装配点（cmd/harness-go/host_wiring.go）直接 import 插件包
//	   internal/plugins/sop 来读取 SOP —— 内核依赖插件，方向错误。
//	② root 固化：旧实现经 host.Workspace() 读 SOP，而该 root 在 NewHost 时
//	   一次性定格（internal/tools/hostimpl/host.go），跨项目切换会话必然串味。
//	③ 真源散落：「内置 SOP」实际是 main.go 里的 const 常量，与文件 SOP 是
//	   两条互不知情的注入路径，无统一入口。
//
// 本包只依赖标准库（**严禁** import toolapi / plugins / service），因此内核与
// 插件都可以安全地向下依赖它，依赖方向永远单向。
//
// # 三层真源与解析优先级
//
//	L0 出厂  internal/sop/builtin/agent-core.md（go:embed，只读，永不落盘）
//	L1 运行时 <dataDir>/sop/agent-core.md      （存在即覆盖 L0；删除即回落）
//
// 「模式」不是独立状态字段，而是「L1 文件是否存在」的派生 —— 从根上杜绝
// 「settings 说自定义、文件却被删了」这类双真源漂移。
//
// # 铁律
//
//   - **绝不主动播种**：任何启动/读取路径都不得把 L0 内容写入磁盘。只有用户
//     显式执行 SaveCore（界面点「切换为自定义编辑」）时才会 fork 出 L1。
//     这是「升级二进制后永远跑最新出厂内容」的唯一保证。
//   - **超限显式拒绝，绝不静默截断**（D10）：写入侧超总预算直接报错返回，
//     读取侧（internal/prompt）超条数走 Degrade 显式记录，两端口径一致。
package sop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// 文件与目录约定。
const (
	// CoreSlug 核心 SOP 的条目 ID（内置与运行时覆盖共用）。
	CoreSlug = "agent-core"

	// RuntimeSubdir 运行时覆盖层在 dataDir 下的子目录。
	RuntimeSubdir = "sop"

	// EntriesDir 项目级 SOP 条目目录（工作区相对）。
	//
	// 历史沿革：该目录原由插件包定义后迁至本包，语义不变。
	EntriesDir = ".harness/sop"

	// MaxEntries 项目级 SOP 条数硬上限。
	//
	// 注意：这里只管「条数」，**不再有单条 token 上限** —— 单条上限已按需求
	// 取消，改为在保存核心 SOP 时校验总预算（见 Loader.SaveCore）。
	MaxEntries = 6
)

// Source 标明核心 SOP 的实际来源。
type Source string

const (
	// SourceBuiltin 出厂内置（embed）。
	SourceBuiltin Source = "builtin"
	// SourceCustom 运行时自定义（data/sop/agent-core.md）。
	SourceCustom Source = "custom"
)

// ErrBudgetExceeded 内容超出 token 预算（显式拒绝，绝不截断）。
var ErrBudgetExceeded = errors.New("sop: 内容超出 token 预算")

// Core 核心 SOP 的生效快照。
type Core struct {
	// Content 实际生效的正文（已 TrimSpace）。
	Content string `json:"content"`
	// Source 来源：builtin / custom。
	Source Source `json:"source"`
	// Tokens 正文的启发式 token 估算。
	Tokens int `json:"tokens"`
	// Budget 当前生效的 token 预算（用于前端展示 ≈tokens / budget）。
	Budget int `json:"budget"`
	// BuiltinContent 出厂原文。前端「系统内置」页签直接渲染它，
	// 从而天然保证「切页签不产生任何读写」，绝不误删用户的自定义内容。
	BuiltinContent string `json:"builtin_content"`
	// Overridden 是否存在运行时覆盖（等价于「当前处于自定义模式」）。
	Overridden bool `json:"overridden"`
}

// Entry 项目级 SOP 条目。
type Entry struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Tokens int    `json:"tokens"`
}

// Loader 核心 SOP 读写器。
//
// 全部路径由调用方以**显式绝对路径**注入，绝不触碰 toolapi.Host —— 这既是
// 零依赖约束的结果，也顺带根治了「host root 启动期固化导致跨项目串味」。
type Loader struct {
	// dataDir 数据根目录（config.EnsureDataDir 的返回值，绝对路径）。
	dataDir string
	// budget 核心 SOP 的 token 总预算（<=0 表示不限）。
	budget int
}

// NewLoader 构造 Loader。dataDir 为空时退化为「仅内置」模式（不报错，
// 便于测试与降级场景：此时 SaveCore/ResetCore 返回 ErrNoDataDir）。
func NewLoader(dataDir string, budget int) *Loader {
	if budget < 0 {
		budget = 0
	}
	return &Loader{dataDir: strings.TrimSpace(dataDir), budget: budget}
}

// ErrNoDataDir 未配置数据目录，无法读写运行时覆盖层。
var ErrNoDataDir = errors.New("sop: 未配置数据目录，运行时覆盖不可用（仅出厂内置生效）")

// Budget 返回当前 token 预算（0 = 不限）。
func (l *Loader) Budget() int { return l.budget }

// RuntimePath 返回运行时覆盖层的绝对路径；未配置数据目录时返回空串。
func (l *Loader) RuntimePath() string {
	if l.dataDir == "" {
		return ""
	}
	return filepath.Join(l.dataDir, RuntimeSubdir, CoreSlug+".md")
}

// Builtin 返回出厂原文（不带 IO，纯内存）。
func Builtin() string { return strings.TrimSpace(builtinCore) }

// LoadCore 解析并返回当前生效的核心 SOP。
//
// 裁决唯一规则：L1 存在且去空白后非空 → 用 L1；否则 → 用 L0。
// 本方法**只读**，绝不创建任何文件或目录。
func (l *Loader) LoadCore() Core {
	builtin := Builtin()
	core := Core{
		Content:        builtin,
		Source:         SourceBuiltin,
		Tokens:         EstimateTokens(builtin),
		Budget:         l.budget,
		BuiltinContent: builtin,
		Overridden:     false,
	}
	path := l.RuntimePath()
	if path == "" {
		return core
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// 文件不存在 / 不可读 → 回落出厂（这是正常路径，不是错误）。
		return core
	}
	body := strings.TrimSpace(string(data))
	if body == "" {
		// 空文件视同未覆盖：避免「用户清空后以为生效、实际注入了空系统提示」。
		return core
	}
	core.Content = body
	core.Source = SourceCustom
	core.Tokens = EstimateTokens(body)
	core.Overridden = true
	return core
}

// SaveCore 写入运行时覆盖层（首次调用即完成「从内置 fork 到自定义」）。
//
// 超预算时返回包装了 ErrBudgetExceeded 的错误，且**绝不修改磁盘文件**
// （拒绝不截断：宁可失败也不留下一个被悄悄裁剪的 SOP）。
func (l *Loader) SaveCore(content string) (Core, error) {
	body := strings.TrimSpace(content)
	if body == "" {
		return Core{}, errors.New("sop: 内容不能为空（恢复出厂请用 ResetCore）")
	}
	if l.budget > 0 {
		if tokens := EstimateTokens(body); tokens > l.budget {
			return Core{}, fmt.Errorf(
				"%w: 当前约 %d token，超出预算 %d —— 请精简内容，或调大 config.sop.core_max_tokens",
				ErrBudgetExceeded, tokens, l.budget)
		}
	}
	path := l.RuntimePath()
	if path == "" {
		return Core{}, ErrNoDataDir
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Core{}, fmt.Errorf("sop: 创建目录失败: %w", err)
	}
	// 0600：SOP 属本机私有配置，不做全局可读。
	if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
		return Core{}, fmt.Errorf("sop: 写入失败: %w", err)
	}
	return l.LoadCore(), nil
}

// ResetCore 删除运行时覆盖层，回落到出厂内置（完整 undo）。
//
// 文件本就不存在时不报错（幂等）。
func (l *Loader) ResetCore() error {
	path := l.RuntimePath()
	if path == "" {
		return ErrNoDataDir
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sop: 删除覆盖层失败: %w", err)
	}
	return nil
}

// EntriesPath 返回项目级 SOP 目录的绝对路径（wsPath 为空时返回空串）。
func EntriesPath(wsPath string) string {
	wsPath = strings.TrimSpace(wsPath)
	if wsPath == "" {
		return ""
	}
	return filepath.Join(wsPath, filepath.FromSlash(EntriesDir))
}

// LoadEntries 读取项目级 SOP 条目（按文件名排序，保证注入稳定）。
//
// 目录不存在 = 项目未配置 SOP（返回 nil，不是错误）。
// wsPath 为空（会话未绑定工作区）= 返回 nil。
func LoadEntries(wsPath string) []Entry {
	dir := EntriesPath(wsPath)
	if dir == "" {
		return nil
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]Entry, 0, len(items))
	for _, it := range items {
		if it.IsDir() || !strings.HasSuffix(it.Name(), ".md") {
			continue
		}
		data, rErr := os.ReadFile(filepath.Join(dir, it.Name()))
		if rErr != nil {
			continue
		}
		body := strings.TrimSpace(string(data))
		if body == "" {
			continue
		}
		id := strings.TrimSuffix(it.Name(), ".md")
		out = append(out, Entry{ID: id, Title: id, Body: body, Tokens: EstimateTokens(body)})
	}
	return out
}

// ValidateEntry 校验项目级条目（条数上限 + 非空）。existing 为同 ID 是否已存在。
//
// 本函数不碰 IO：插件的沙箱写入与任何未来的 REST 端点共用同一套校验口径，
// 从根上避免「两份实现漂移」。
func ValidateEntry(id, body string, existingIDs []string, known bool) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("sop: 条目 ID 不能为空")
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("sop: 正文不能为空")
	}
	if !known && len(existingIDs) >= MaxEntries {
		return fmt.Errorf("sop: 已有 %d 条（%s），达到条数上限 %d —— 请先删除或合并低价值条目",
			len(existingIDs), strings.Join(existingIDs, ", "), MaxEntries)
	}
	return nil
}

// EstimateTokens 启发式 token 估算：CJK/全角 ≈ 1 token/字符，其余 ≈ 1 token/4 字符。
//
// 只用于上限校验与前端提示（与 tokenizer.Heuristic 同数量级，不追求精确）。
func EstimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) ||
			(r > 0x2E80 && r <= 0x9FFF) || (r >= 0xFF00 && r <= 0xFFEF) {
			cjk++
			continue
		}
		if !unicode.IsSpace(r) {
			other++
		}
	}
	return cjk + (other+3)/4
}

// Slugify 标题 → 条目 ID（文件名安全；保留 CJK 与字母数字，其余折叠为 -）。
func Slugify(title string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.TrimSpace(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			lastDash = false
		case r == ' ' || r == '-' || r == '_' || r == '/' || r == '\\':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() > 80 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}
