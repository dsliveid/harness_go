package main

// 仓库级契约自检（无需 golangci-lint 与 Node 工具链即可运行）：
//
//   - TestContractRouterNoDrift：以 internal/api/router.go **实际注册**的路由为权威基准，
//     校验每条对外 API 路径都已在 api/openapi.yaml 登记（三方校验的根）；
//   - TestContractPathsNoDrift：api/openapi.yaml 的 paths 与 web/src/lib/apiTypes.ts 的 API_PATHS
//     必须一一对应（阶段 A 冻结后只允许向后兼容新增，禁止单边漂移）；
//   - TestPluginLayering：pkg/toolapi 与 internal/plugins/** 禁止 import
//     internal/{store,llm,agent,api,service}，插件之间亦禁止互相 import
//     （等价于 .golangci.yml 的 depguard 规则，本机无 golangci-lint 时的兜底门禁）。

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	openapiPath  = "../../api/openapi.yaml"
	routerPath   = "../../internal/api/router.go"
	apiTypesPath = "../../web/src/lib/apiTypes.ts"
	toolapiDir   = "../../pkg/toolapi"
	pluginsDir   = "../../internal/plugins"
	modulePrefix = "harness-go/"
)

// yamlPathRe 匹配 paths 段中的路径键（缩进两空格，以 / 开头，行尾冒号）。
var yamlPathRe = regexp.MustCompile(`^  (/\S*):\s*$`)

// tsPathRe 匹配 apiTypes.ts 中被引号或模板串包裹的路径字面量。
var tsPathRe = regexp.MustCompile("['\"`](/[^'\"`]*)['\"`]")

// tsTemplateVarRe 匹配路径中的 ${name} 模板变量（映射为 openapi 的 {name}）。
var tsTemplateVarRe = regexp.MustCompile(`\$\{(\w+)\}`)

// routerRouteRe 匹配 router.go 中 mux.Handle / mux.HandleFunc 的注册模式串字面量，
// 如 `mux.HandleFunc("GET /api/v1/sessions/{sid}", h.getSession)`。
// 🔴 两个捕获组：m[1]=HTTP 方法，m[2]=路径。改这里时必须同步修改索引，
// 当年把方法写成捕获组却仍取 m[1] 当路径，会把方法名当成路径塞进集合（本文件已踩过）。
// 注：整行以 // 开头的注释行在解析时被直接跳过（见 parseMuxRoutes），
// 因此 `// mux.HandleFunc("POST /api/v1/debug", ...)` 不会误入。
var routerRouteRe = regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+)\s+(/[^"]*)"`)

// openapiRouteRe 匹配 openapi.yaml paths 段下的 HTTP 方法键（缩进四空格，如 `    get:`）。
var openapiRouteRe = regexp.MustCompile(`^    (get|post|put|patch|delete|head|options):\s*$`)

// muxRoute 是一条「方法 + 路径」注册项。只看路径会漏掉方法维度漂移
// （实测存在 /api/v1/providers/{pid}/models：router=POST，openapi=GET），故两者都要比。
type muxRoute struct {
	Method string
	Path   string
}

func (r muxRoute) String() string { return r.Method + " " + r.Path }

// nonAPIRoutes 是**静态资源 / SPA 托管 / 探活**路由白名单：它们由 router.go 注册，
// 但**不属于对外 API 契约**，因此不要求出现在 api/openapi.yaml 的 paths 中。
//
// 🔴 这是显式逐条常量，禁止改成「结尾斜杠就跳过」「正则模糊放过」等隐式规则。
// 隐式规则会让新增的静态路由静默滑过，盲区将以另一种形式复发。
// value 必须是排除理由，后来者若要新增条目，必须回答「为什么它不是 API 契约」。
var nonAPIRoutes = map[string]string{
	"/{$}":                  "SPA 入口（ServeMux 精确根匹配写法，openapi 侧对应 /），非 JSON API",
	"/":                     "SPA 入口的 openapi 写法（router 用 /{$} 注册），非 JSON API",
	"/healthz":              "容器/负载均衡探活端点，裸文本响应，非业务契约",
	"/assets/":              "前端构建产物静态目录（ServeMux 尾斜杠前缀匹配），非 API",
	"/icons/":               "PWA 图标静态目录（ServeMux 尾斜杠前缀匹配），非 API",
	"/manifest.webmanifest": "PWA 清单文件静态资源，非 API",
	"/sw.js":                "Service Worker 脚本静态资源，非 API",
	"/login":                "SPA 登录页面托管入口（返回 HTML），非 API；真正的登录 API 是 /api/v1/auth/login",
}

// readOpenAPIPaths 读取 openapi.yaml 声明的全部 path 键（仅路径，供 apiTypes 双向比对使用）。
func readOpenAPIPaths(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(openapiPath)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", openapiPath, err)
	}
	out := make([]string, 0, 32)
	for _, line := range strings.Split(string(raw), "\n") {
		if m := yamlPathRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatalf("%s 未解析出任何 path（格式可能已变）", openapiPath)
	}
	return out
}

// readOpenAPIRoutes 读取 openapi.yaml 的「方法 + 路径」全集，用于与 router.go 做方法维度比对。
// 只按 YAML 缩进结构识别（pathKey 两空格 → 方法键四空格），不引入 yaml 依赖。
func readOpenAPIRoutes(t *testing.T) map[string]map[string]struct{} {
	t.Helper()
	raw, err := os.ReadFile(openapiPath)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", openapiPath, err)
	}
	out := map[string]map[string]struct{}{}
	var cur string
	for _, line := range strings.Split(string(raw), "\n") {
		if m := yamlPathRe.FindStringSubmatch(line); m != nil {
			cur = m[1]
			if out[cur] == nil {
				out[cur] = map[string]struct{}{}
			}
			continue
		}
		if cur == "" {
			continue
		}
		if m := openapiRouteRe.FindStringSubmatch(line); m != nil {
			out[cur][strings.ToUpper(m[1])] = struct{}{}
			continue
		}
		// 离开 paths 段（出现顶层键）或进入更深层结构时重置当前路径追踪。
		if line != "" && !strings.HasPrefix(line, " ") {
			cur = ""
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s 未解析出任何 path（格式可能已变）", openapiPath)
	}
	return out
}

// knownMethodDrifts 登记**已确认存在、但本轮不允许修改 openapi.yaml / router.go** 的方法维度漂移。
//
// 🔴 这不是「忽略规则」，而是显式台账：每条都必须写明实测事实与处置意见。
// 一旦有人修好其中一条，测试会因台账失效而报错，逼迫删除条目 —— 台账不会腐化。
//
// 当前为空：`/api/v1/providers/{pid}/models` 的方法漂移（router 注册 POST、openapi 曾声明 GET）
// 已于本轮修复 openapi 侧（改为 POST，并补齐 requestBody 与 query 兼容参数），故条目已删除。
var knownMethodDrifts = map[string]string{}

func readAPITypesPaths(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(apiTypesPath)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", apiTypesPath, err)
	}
	text := string(raw)
	// 只解析 API_PATHS 常量块，避免注释里的路径片段误入
	start := strings.Index(text, "export const API_PATHS")
	if start < 0 {
		t.Fatalf("%s 中未找到 API_PATHS", apiTypesPath)
	}
	block := text[start:]
	if end := strings.Index(block, "} as const;"); end > 0 {
		block = block[:end]
	}
	seen := map[string]struct{}{}
	for _, line := range strings.Split(block, "\n") {
		for _, m := range tsPathRe.FindAllStringSubmatch(line, -1) {
			// 模板变量名统一映射到 openapi 的 {name} 形式（${aid} → {aid}），
			// 新增路径参数时无需再改测试（原先硬编码 sid/rid 会静默漏判）。
			p := tsTemplateVarRe.ReplaceAllString(m[1], `{$1}`)
			seen[p] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatalf("%s 的 API_PATHS 未解析出任何路径", apiTypesPath)
	}
	return out
}

// parseMuxRoutes 从源码文本中解析 mux.Handle / mux.HandleFunc 注册的**全部**「方法 + 路径」（不过白名单）。
//
// 解析规则（两条硬约束，缺一即产生新的盲区）：
//  1. **整行注释行必须跳过**：被注释掉的 `// mux.HandleFunc("POST /api/v1/debug", ...)`
//     一旦被当成已注册，就会制造假阳性（要求 openapi 登记一个并不存在的端点）；
//  2. **未识别的 mux 注册必须报错**：任何含 "mux.Handle" 却未被正则吃掉的非注释行，
//     说明注册写法变了（正则路由、变量拼接、多行字符串等），必须让测试失败而不是静默漏过。
//     静默漏过 == 盲区依旧存在，这正是本测试要消灭的东西。
func parseMuxRoutes(t *testing.T, src, source string) ([]muxRoute, error) {
	t.Helper()
	seen := map[muxRoute]struct{}{}
	unparsed := []string{}
	inBlockComment := false
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if inBlockComment {
			if strings.Contains(trimmed, "*/") {
				inBlockComment = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "/*") {
			if !strings.Contains(trimmed, "*/") {
				inBlockComment = true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if !strings.Contains(line, "mux.Handle") {
			continue
		}
		matches := routerRouteRe.FindAllStringSubmatch(line, -1)
		if len(matches) == 0 {
			unparsed = append(unparsed, fmt.Sprintf("%s:%d: %s", source, i+1, trimmed))
			continue
		}
		for _, m := range matches {
			seen[muxRoute{Method: m[1], Path: m[2]}] = struct{}{}
		}
	}
	if len(unparsed) > 0 {
		return nil, fmt.Errorf("发现无法解析的 mux 注册写法（正则路由/变量拼接/多行?），解析规则必须同步更新，否则契约盲区依旧存在：\n  %s",
			strings.Join(unparsed, "\n  "))
	}
	out := make([]muxRoute, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out, nil
}

// readRouterRoutes 解析 internal/api/router.go 中实际注册的「方法 + 路径」（剔除白名单）。
//
// 这是契约自检的**权威基准**：openapi.yaml 与 apiTypes.ts 都是手写副本，
// 只有 router.go 里的 mux.Handle/mux.HandleFunc 才能证明端点真的存在。
// 因此「router 注册了但两侧都没写」必须报错，否则盲区依旧存在。
func readRouterRoutes(t *testing.T) []muxRoute {
	t.Helper()
	raw, err := os.ReadFile(routerPath)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", routerPath, err)
	}
	all, err := parseMuxRoutes(t, string(raw), routerPath)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(all) == 0 {
		t.Fatalf("%s 未解析出任何 mux 路由（注册写法可能已变，需同步更新解析规则）", routerPath)
	}
	out := make([]muxRoute, 0, len(all))
	for _, r := range all {
		if _, skipped := nonAPIRoutes[r.Path]; skipped {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ============ 可测纯函数：三方比对判定核心（S1 负向用例的落点） ============
//
// 抽成纯函数的原因：原先比对逻辑全部内联在 TestContractRouterNoDrift 内，
// 想验证「判定真有约束力」只能去改生产源码（router.go / openapi.yaml），
// 而生产源码是只读基准，不能被负向用例污染。
// 现在正向用例与负向用例共用**同一份**判定实现：正向用真实文件，负向喂构造样本。
// 若有人把判定改成「恒返回无差异」，正向用例未必立刻变红，但负向用例必定变红。
//
// 约定：所有 diff 函数「空切片 = 无漂移」，非空即以升序返回差异项，调用方负责报错。

// sortedKeys 把字符串集合转成升序切片（diff 结果稳定，便于断言与日志比对）。
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// diffStrings 返回 a 中不属于 b 的元素（去重升序）。
// 「属于」按值精确比较，不做前缀/通配模糊匹配——模糊匹配会让新增路径静默滑过。
func diffStrings(a, b []string) []string {
	bSet := make(map[string]struct{}, len(b))
	for _, p := range b {
		bSet[p] = struct{}{}
	}
	seen := make(map[string]struct{}, len(a))
	for _, p := range a {
		if _, ok := bSet[p]; ok {
			continue
		}
		seen[p] = struct{}{}
	}
	return sortedKeys(seen)
}

// buildRouterMethodIndex 把 router 注册项按路径聚合成「方法集合」。
// 只看路径会漏掉方法维度漂移（历史实测：同一路径 router=POST、openapi=GET → 线上 405）。
func buildRouterMethodIndex(routes []muxRoute) map[string]map[string]struct{} {
	out := make(map[string]map[string]struct{}, len(routes))
	for _, r := range routes {
		if out[r.Path] == nil {
			out[r.Path] = map[string]struct{}{}
		}
		out[r.Path][r.Method] = struct{}{}
	}
	return out
}

// diffRouterAgainstOpenAPI 返回「router.go 已注册、但 openapi.yaml 未登记」的路径。
//
// 这是三方校验里最关键的一条边：router.go 是端点真实存在的唯一证据。
// 若新增端点只在 router 落地、契约两侧都没写，前端照契约写代码就是 404，
// 而旧测试会一直绿——本函数存在就是为了让这种情况必然报错。
func diffRouterAgainstOpenAPI(routerRoutes []muxRoute, openapiPaths []string) []string {
	routerPaths := make([]string, 0, len(routerRoutes))
	for _, r := range routerRoutes {
		routerPaths = append(routerPaths, r.Path)
	}
	return diffStrings(routerPaths, openapiPaths)
}

// diffOpenAPIAgainstRouter 返回「openapi.yaml 声明、但 router.go 未注册」的路径
// （nonAPIRoutes 白名单内的静态/SPA/探活路径除外）。
//
// 非空即幽灵端点：契约里挂着早已删除的端点，前端照契约调用会 404。
func diffOpenAPIAgainstRouter(openapiPaths []string, routerRoutes []muxRoute, whitelist map[string]string) []string {
	routerPaths := make([]string, 0, len(routerRoutes))
	for _, r := range routerRoutes {
		routerPaths = append(routerPaths, r.Path)
	}
	routerSet := make(map[string]struct{}, len(routerPaths))
	for _, p := range routerPaths {
		routerSet[p] = struct{}{}
	}
	missing := make([]string, 0, 4)
	for _, p := range openapiPaths {
		if _, ok := routerSet[p]; ok {
			continue
		}
		if _, whitelisted := whitelist[p]; whitelisted {
			continue
		}
		missing = append(missing, p)
	}
	return diffStrings(missing, nil) // 复用去重 + 升序
}

// diffMethodDrifts 返回方法维度漂移（router 某路径注册了某方法、openapi 同路径未声明）。
// known 是显式台账：登记后豁免，但台账条目一旦修好必须在 TestContractRouterNoDrift 里被清理。
func diffMethodDrifts(routerRoutes []muxRoute, openapiRoutes map[string]map[string]struct{},
	known map[string]string) []string {

	idx := buildRouterMethodIndex(routerRoutes)
	out := make([]string, 0, 4)
	for path, methods := range idx {
		declared, ok := openapiRoutes[path]
		if !ok {
			// 路径整体未登记已由 diffRouterAgainstOpenAPI 报出，这里不重复报，避免噪音。
			continue
		}
		for m := range methods {
			if _, ok := declared[m]; ok {
				continue
			}
			if _, isKnown := known[path]; isKnown {
				continue
			}
			out = append(out, fmt.Sprintf("%s %s（router 注册但 openapi 未声明该方法）", m, path))
		}
	}
	sort.Strings(out)
	return out
}

// diffOpenAPIAgainstAPITypes 返回「openapi.yaml 有、apiTypes.ts 的 API_PATHS 缺失」的路径。
// 改契约必须两侧同步：漏改前端会导致前端拿不到路径常量。
func diffOpenAPIAgainstAPITypes(openapiPaths, tsPaths []string) []string {
	return diffStrings(openapiPaths, tsPaths)
}

// diffAPITypesAgainstOpenAPI 返回「apiTypes.ts 有、openapi.yaml 缺失」的路径。
// 非空即前端自造端点：后端 404，且契约自检形同虚设。
func diffAPITypesAgainstOpenAPI(tsPaths, openapiPaths []string) []string {
	return diffStrings(tsPaths, openapiPaths)
}

// TestParseMuxRoutesSelfCheck 是解析器自身的单元测试。
//
// 必要性：router.go 当前恰好没有注释掉的注册行，也没有变量拼接路径，
// 因此「跳过注释」「未识别写法必须报错」这两条防线无法靠修改 router.go 来验证
// （router.go 是只读生产源码）。用内联样本把规则钉死，避免日后有人放松解析规则。
func TestParseMuxRoutesSelfCheck(t *testing.T) {
	src := strings.Join([]string{
		"func NewRouter(d Deps) http.Handler {",
		"\tmux := http.NewServeMux()",
		"\tmux.HandleFunc(\"GET /{$}\", h.root)",
		"\tmux.Handle(\"GET /healthz\", observ.Healthz(d.Version))",
		"\tmux.HandleFunc(\"POST /api/v1/sessions/{sid}/stop\", h.stop)",
		"\t// mux.HandleFunc(\"POST /api/v1/debug\", h.debug)",
		"\t//mux.HandleFunc(\"GET /api/v1/commented-no-space\", h.x)",
		"\t/*",
		"\tmux.HandleFunc(\"GET /api/v1/block-commented\", h.y)",
		"\t*/",
		"\treturn mux",
		"}",
	}, "\n")

	got, err := parseMuxRoutes(t, src, "<inline>")
	if err != nil {
		t.Fatalf("解析内联样本失败: %v", err)
	}
	want := []muxRoute{
		{Method: "POST", Path: "/api/v1/sessions/{sid}/stop"},
		{Method: "GET", Path: "/healthz"},
		{Method: "GET", Path: "/{$}"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("解析结果不符\n got=%v\nwant=%v（注释行/块注释行必须被跳过，方法必须解析出来）", got, want)
	}

	// 未识别写法（变量拼接）必须报错，而不是静默跳过。
	badSrc := "\tmux.HandleFunc(method + \" /api/v1/x\", h.x)"
	if _, err := parseMuxRoutes(t, badSrc, "<inline-bad>"); err == nil {
		t.Fatal("变量拼接写法未被识别却未报错——这会让契约盲区以另一种形式复发")
	}
}

// TestContractRouterNoDrift 以 router.go 的实际注册为权威基准做三方校验。
//
// 设计要点（为何能堵住盲区）：
//  1. 正向：router.go 注册的每条对外路径必须已登记在 openapi.yaml ——
//     这直接命中「端点实现很久、契约两侧都没写、旧测试一直绿」的事故模式；
//  2. 反向：openapi.yaml 声明的每条路径必须在 router.go 有对应注册 ——
//     防止契约里挂着早已删除的幽灵端点（前端照契约写代码会 404）；
//  3. 方法维度：同一路径的方法集必须一致 ——
//     否则前端照 openapi 发 GET、后端只注册 POST，线上就是 405（实测已抓到 1 条）；
//  4. 非 API 契约的静态/探活/SPA 路由只能通过 nonAPIRoutes 显式白名单排除，
//     且白名单条目必须仍真实存在（失效条目会报错），避免白名单腐化成新的盲区；
//  5. apiTypes.ts ↔ openapi.yaml 的原双向比对保留在 TestContractPathsNoDrift，
//     三条边合起来就是 router ↔ openapi ↔ apiTypes 的闭环。
func TestContractRouterNoDrift(t *testing.T) {
	routes := readRouterRoutes(t)
	openapi := readOpenAPIPaths(t)
	openapiRoutes := readOpenAPIRoutes(t)

	// 判定全部委托给纯函数（与 TestContractDiffPureFunctions 的负向用例共用同一份实现，
	// 避免「正向走一套、负向测另一套」的自欺欺人）。
	missingInOpenAPI := diffRouterAgainstOpenAPI(routes, openapi)
	missingInRouter := diffOpenAPIAgainstRouter(openapi, routes, nonAPIRoutes)
	methodDrifts := diffMethodDrifts(routes, openapiRoutes, knownMethodDrifts)

	routerMethods := buildRouterMethodIndex(routes)
	routeSet := make(map[string]struct{}, len(routes))
	for _, r := range routes {
		routeSet[r.Path] = struct{}{}
	}
	openapiSet := make(map[string]struct{}, len(openapi))
	for _, p := range openapi {
		openapiSet[p] = struct{}{}
	}

	// 白名单不得腐化：条目必须在 router 或 openapi 任一侧仍存在。
	rawSet := rawRouterPaths(t)
	var staleWhitelist []string
	for p := range nonAPIRoutes {
		_, inRouter := routeSet[p]
		_, inOpenAPI := openapiSet[p]
		_, inRawRouter := rawSet[p]
		if !inRouter && !inOpenAPI && !inRawRouter {
			staleWhitelist = append(staleWhitelist, p)
		}
	}
	sort.Strings(staleWhitelist)

	// 方法漂移台账不得腐化：修好后必须删除条目，否则「已知漂移」会变成永久豁免。
	var fixedDrifts []string
	for p, reason := range knownMethodDrifts {
		rm, okRouter := routerMethods[p]
		om, okOpenAPI := openapiRoutes[p]
		if !okRouter || !okOpenAPI {
			fixedDrifts = append(fixedDrifts, fmt.Sprintf("%s（路径已不存在于某一侧，台账应删除：%s）", p, reason))
			continue
		}
		identical := len(rm) == len(om)
		if identical {
			for m := range rm {
				if _, ok := om[m]; !ok {
					identical = false
					break
				}
			}
		}
		if identical {
			fixedDrifts = append(fixedDrifts, fmt.Sprintf("%s（两侧方法已一致，台账应删除）", p))
		}
	}
	sort.Strings(fixedDrifts)

	sort.Strings(missingInOpenAPI)
	if len(missingInOpenAPI) > 0 {
		t.Errorf("router.go 注册但 openapi.yaml 未登记: %v（新增端点必须同步登记 openapi，否则契约自检形同虚设）", missingInOpenAPI)
	}
	if len(missingInRouter) > 0 {
		t.Errorf("openapi.yaml 声明但 router.go 未注册: %v（幽灵端点，前端照契约调用会 404；确属非 API 请加入 nonAPIRoutes 并写明理由）", missingInRouter)
	}
	if len(methodDrifts) > 0 {
		t.Errorf("方法维度漂移: %v（前端照 openapi 发请求会 405；确认后修契约，或登记到 knownMethodDrifts 并写明理由）", methodDrifts)
	}
	if len(staleWhitelist) > 0 {
		t.Errorf("nonAPIRoutes 存在失效条目: %v（router 与 openapi 两侧都不存在该路径，请删除以免白名单腐化）", staleWhitelist)
	}
	if len(fixedDrifts) > 0 {
		t.Errorf("knownMethodDrifts 存在已失效条目: %v（漂移已修复或路径已删除，请从台账移除）", fixedDrifts)
	}
	if len(missingInOpenAPI) > 0 || len(missingInRouter) > 0 || len(methodDrifts) > 0 ||
		len(staleWhitelist) > 0 || len(fixedDrifts) > 0 {
		t.FailNow()
	}
	t.Logf("router ↔ openapi 一致：router 对外「方法+路径」%d 条 / 去重路径 %d 条，openapi 路径 %d 条，白名单排除 %d 条，已知方法漂移台账 %d 条",
		len(routes), len(routeSet), len(openapi), len(nonAPIRoutes), len(knownMethodDrifts))
}

// rawRouterPaths 解析 router.go 注册的**全部**路径（不过白名单），供白名单失效检测使用。
// 复用 parseMuxRoutes，保证「跳过注释 / 未识别写法报错」两条规则只有一处实现。
func rawRouterPaths(t *testing.T) map[string]struct{} {
	t.Helper()
	raw, err := os.ReadFile(routerPath)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", routerPath, err)
	}
	all, err := parseMuxRoutes(t, string(raw), routerPath)
	if err != nil {
		t.Fatalf("%v", err)
	}
	out := make(map[string]struct{}, len(all))
	for _, r := range all {
		out[r.Path] = struct{}{}
	}
	return out
}

// TestContractPathsNoDrift 校验 HTTP 契约零漂移。
func TestContractPathsNoDrift(t *testing.T) {
	openapi := readOpenAPIPaths(t)
	ts := readAPITypesPaths(t)

	// 判定委托给纯函数（与 TestContractDiffPureFunctions 的负向用例共用同一份实现）。
	missingInTS := diffOpenAPIAgainstAPITypes(openapi, ts)
	missingInYAML := diffAPITypesAgainstOpenAPI(ts, openapi)

	if len(missingInTS) > 0 {
		t.Fatalf("openapi.yaml 有但 apiTypes.ts 缺失: %v（改契约必须两侧同步）", missingInTS)
	}
	if len(missingInYAML) > 0 {
		t.Fatalf("apiTypes.ts 有但 openapi.yaml 缺失: %v（前端不得自造端点）", missingInYAML)
	}
	t.Logf("契约一致：%d 条 path", len(openapi))
}

// ============ S1：契约负向用例（证明校验真有约束力） ============
//
// 正向用例只能证明「当前三方恰好一致」，无法证明「不一致时会变红」——
// 若有人把比对改成恒返回空（或把断言放宽成 len>0 才报），正向用例依旧全绿。
// 下面四个用例喂构造样本，逐个钉死「必须报出差异」。

// TestContractDiffRouterAgainstOpenAPIRejectsUnregistered 是 S1 最核心的负向用例：
// router.go 注册了、openapi.yaml 没登记 → **必须**报出该路径。
//
// 两段式：
//  1. 纯构造样本：断言返回值精确等于缺失项（钉死语义，不依赖仓库真实状态）；
//  2. 真实基线 + 注入假路由：证明「往今天这份 router 集合里掺一条漏登记的路由」也会被抓到。
//     这里只断言「包含」，因为断言精确等于会把「将来真实基线确有一条待修漂移」误判成本测试失败。
func TestContractDiffRouterAgainstOpenAPIRejectsUnregistered(t *testing.T) {
	// ① 纯构造样本
	router := []muxRoute{
		{Method: "GET", Path: "/api/v1/sessions"},
		{Method: "GET", Path: "/api/v1/__drift_probe"},
	}
	openapi := []string{"/api/v1/sessions"}
	got := diffRouterAgainstOpenAPI(router, openapi)
	want := []string{"/api/v1/__drift_probe"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("router 注册但 openapi 未登记必须报出\n got=%v\nwant=%v", got, want)
	}

	// ② 真实基线 + 假路由
	real := readRouterRoutes(t)
	if drift := diffRouterAgainstOpenAPI(real, readOpenAPIPaths(t)); len(drift) != 0 {
		t.Fatalf("前置不成立：真实基线本身已有漂移 %v，负向用例无法定位假路由", drift)
	}
	probe := muxRoute{Method: "GET", Path: "/api/v1/__drift_probe"}
	injected := append(append([]muxRoute{}, real...), probe)
	got = diffRouterAgainstOpenAPI(injected, readOpenAPIPaths(t))
	found := false
	for _, p := range got {
		if p == probe.Path {
			found = true
		}
	}
	if !found {
		t.Fatalf("往真实 router 集合注入 %s 后未被报出（got=%v）——契约校验形同虚设", probe, got)
	}

	// ③ 一致时不得报差异（防「恒返回非空」的反向腐化）
	if got := diffRouterAgainstOpenAPI(router[:1], openapi); len(got) != 0 {
		t.Fatalf("router 与 openapi 完全一致时不得报差异，实际 %v", got)
	}
}

// TestContractDiffOpenAPIAgainstRouterRejectsGhost 覆盖反向边：
// openapi 声明了但 router 未注册（幽灵端点）必须报出，且 nonAPIRoutes 白名单内的不算漂移。
func TestContractDiffOpenAPIAgainstRouterRejectsGhost(t *testing.T) {
	router := []muxRoute{{Method: "GET", Path: "/api/v1/sessions"}}
	whitelist := map[string]string{"/healthz": "探活端点，非 API 契约"}
	openapi := []string{"/api/v1/sessions", "/api/v1/ghost", "/healthz"}

	got := diffOpenAPIAgainstRouter(openapi, router, whitelist)
	want := []string{"/api/v1/ghost"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("幽灵端点必须报出、白名单必须放过\n got=%v\nwant=%v", got, want)
	}

	// 白名单不是「万能放过」：非白名单路径一律报
	if got := diffOpenAPIAgainstRouter([]string{"/api/v1/ghost2"}, router, whitelist); len(got) != 1 {
		t.Fatalf("非白名单的幽灵端点必须报出，实际 %v", got)
	}
}

// TestContractDiffMethodDriftsRejectsMethodMismatch 覆盖方法维度：
// 同一路径 router=POST、openapi 只声明 GET → 必须报 POST 漂移；登记台账后豁免。
func TestContractDiffMethodDriftsRejectsMethodMismatch(t *testing.T) {
	router := []muxRoute{{Method: "POST", Path: "/api/v1/providers/{pid}/models"}}
	openapiRoutes := map[string]map[string]struct{}{
		"/api/v1/providers/{pid}/models": {"GET": {}},
	}

	got := diffMethodDrifts(router, openapiRoutes, map[string]string{})
	if len(got) != 1 || !strings.Contains(got[0], "POST /api/v1/providers/{pid}/models") {
		t.Fatalf("方法漂移必须报出，实际 %v", got)
	}

	// 登记到显式台账后豁免（台账本身另有「修好必须删条目」的腐化检测）
	known := map[string]string{"/api/v1/providers/{pid}/models": "历史遗留，本轮不改契约"}
	if got := diffMethodDrifts(router, openapiRoutes, known); len(got) != 0 {
		t.Fatalf("台账登记项不得再报漂移，实际 %v", got)
	}

	// 方法一致时不得误报
	same := map[string]map[string]struct{}{"/api/v1/providers/{pid}/models": {"POST": {}}}
	if got := diffMethodDrifts(router, same, map[string]string{}); len(got) != 0 {
		t.Fatalf("方法一致时不得报漂移，实际 %v", got)
	}
}

// TestContractDiffOpenAPIAgainstAPITypesRejectsDrift 覆盖 openapi ↔ apiTypes 双向：
// openapi 有而 apiTypes 没有必须报出；apiTypes 有而 openapi 没有同样必须报出。
func TestContractDiffOpenAPIAgainstAPITypesRejectsDrift(t *testing.T) {
	openapi := []string{"/api/v1/sessions", "/api/v1/auth/login"}
	ts := []string{"/api/v1/auth/login", "/api/v1/frontend-only"}

	if got := diffOpenAPIAgainstAPITypes(openapi, ts); fmt.Sprint(got) != fmt.Sprint([]string{"/api/v1/sessions"}) {
		t.Fatalf("openapi 有但 apiTypes 缺失必须报出，实际 %v", got)
	}
	if got := diffAPITypesAgainstOpenAPI(ts, openapi); fmt.Sprint(got) != fmt.Sprint([]string{"/api/v1/frontend-only"}) {
		t.Fatalf("apiTypes 有但 openapi 缺失必须报出，实际 %v", got)
	}
	// 两侧一致时双向都不得报差异
	if a, b := diffOpenAPIAgainstAPITypes(openapi, openapi), diffAPITypesAgainstOpenAPI(openapi, openapi); len(a) != 0 || len(b) != 0 {
		t.Fatalf("两侧一致时不得报差异，实际 %v / %v", a, b)
	}
}

// TestPluginLayering 校验插件与契约层的分层依赖（depguard 兜底）。
func TestPluginLayering(t *testing.T) {
	denied := []string{
		modulePrefix + "internal/store",
		modulePrefix + "internal/llm",
		modulePrefix + "internal/agent",
		modulePrefix + "internal/api",
		modulePrefix + "internal/service",
		modulePrefix + "internal/plugins",
	}

	scanned := 0
	for _, dir := range []string{toolapiDir, pluginsDir} {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			scanned++
			f, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if parseErr != nil {
				return parseErr
			}
			for _, imp := range f.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				for _, d := range denied {
					if p == d || strings.HasPrefix(p, d+"/") {
						t.Errorf("%s 越界 import %q（插件只能依赖 pkg/toolapi，能力经 Host 获取）", path, p)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("扫描 %s 失败: %v", dir, err)
		}
	}
	if scanned == 0 {
		t.Fatal("未扫描到任何插件/契约文件（目录结构可能已变）")
	}
	t.Logf("分层合规：扫描 %d 个文件，零越界", scanned)
}

// webviewTicketSrcPath / authStoreSrcPath / apiClientSrcPath：WebView 免登链路的三个前端落点。
//
// 为什么用 Go 断言 TS 源码：`harness_go/web` 未安装 vitest，也没有 test 脚本，
// 本任务明令禁止新增 npm 依赖，因此「前端契约被人删掉」只能靠仓库级的源码断言守住。
const (
	webviewTicketSrcPath = "../../web/src/lib/webviewTicket.ts"
	authStoreSrcPath     = "../../web/src/stores/authStore.ts"
	apiClientSrcPath     = "../../web/src/lib/api.ts"
)

// readWebSrc 读取前端源码原文，失败即 Fatal（文件被改名/删除本身就是漂移）。
func readWebSrc(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v（前端契约文件缺失或被改名）", path, err)
	}
	return string(raw)
}

// TestWebExchangesWebviewTicket 守门「契约两边都有、但没人真调」的漂移复发。
//
// 事故背景（P0）：移动端 `prepareAuth` 会签出一次性 ticket 并拼进入口 URL 的 fragment，
// 服务端 `/api/v1/auth/webview-exchange` 也早已齐备，`apiTypes.ts` 里连路径常量与 DTO 都有，
// **但 web/src 全仓没有任何调用点、也没有一处读 `location.hash` 的 ticket**。
// 结果：ticket 被签出后无人消费 → `/auth/me` 无 hg_sess → 首屏渲染登录表单 = 移动端「两轮登录」。
//
// 本测试把那条断链的三个必要条件钉死（三边缺一即失败）：
//  1. `lib/webviewTicket.ts` 存在并导出解析/抹除/组合三个纯函数；
//  2. `stores/authStore.ts` 的 boot 里**同时**出现 `takeTicketFromLocation` 与 `exchangeWebviewTicket`
//     —— 只断言契约文件存在是不够的，必须证明启动路径真的接了线；
//  3. `lib/api.ts` 里出现 `API_PATHS.authWebviewExchange` —— 证明 realApi 真的发出了兑换请求。
func TestWebExchangesWebviewTicket(t *testing.T) {
	// ① 纯函数模块：三个导出缺一不可（`export function xxx` 精确匹配，避免注释误命中）
	ticketSrc := readWebSrc(t, webviewTicketSrcPath)
	for _, fn := range []string{
		"export function readTicketFromHash",
		"export function stripTicketFromFragment",
		"export function takeTicketFromLocation",
	} {
		if !strings.Contains(ticketSrc, fn) {
			t.Errorf("%s 缺少 %q —— fragment 里的 ticket 将再次无人消费（移动端两轮登录的直接成因）",
				webviewTicketSrcPath, fn)
		}
	}

	// ② boot 必须真的调用：读 ticket + 发兑换，二者同时出现才算接线完成
	authSrc := readWebSrc(t, authStoreSrcPath)
	start := strings.Index(authSrc, "boot: async () => {")
	if start < 0 {
		t.Fatalf("%s 中未找到 `boot: async () => {`（boot 实现已改名或被删，守门点失效）", authStoreSrcPath)
	}
	// 截取 boot 函数体：到下一个顶层 `  },\n` 结束（boot 之后的兄弟方法以 2 空格缩进声明）
	bootBody := authSrc[start:]
	if end := strings.Index(bootBody[1:], "\n  },\n"); end > 0 {
		bootBody = bootBody[:end+1]
	}
	// 防止「截取失效 → bootBody 变成整个文件 → 断言退化为形式主义」：
	// 若截取成功，bootBody 一定不含紧邻的兄弟方法声明，也必然短于全文。
	if strings.Contains(bootBody, "ssoLogin: async") || len(bootBody) >= len(authSrc) {
		t.Fatalf("%s 的 boot() 函数体截取失败（守门断言会退化为全文匹配，形同虚设）", authStoreSrcPath)
	}
	for _, call := range []string{"takeTicketFromLocation", "exchangeWebviewTicket"} {
		if !strings.Contains(bootBody, call) {
			t.Errorf("%s 的 boot() 内未调用 %q —— 契约文件存在但启动路径没接线，"+
				"移动端 WebView 免登会静默失效（首屏回落 Web 登录表单）", authStoreSrcPath, call)
		}
	}

	// ③ api.ts 必须真的打兑换端点
	apiSrc := readWebSrc(t, apiClientSrcPath)
	if !strings.Contains(apiSrc, "API_PATHS.authWebviewExchange") {
		t.Errorf("%s 中未找到 API_PATHS.authWebviewExchange —— realApi 未实现兑换请求", apiClientSrcPath)
	}

	t.Logf("WebView ticket 兑换链路三边齐备：webviewTicket.ts（3 个纯函数）/ authStore.boot（取票+兑换）/ api.ts（authWebviewExchange）")
}
