package main

// 仓库级契约自检（无需 golangci-lint 与 Node 工具链即可运行）：
//
//   - TestContractPathsNoDrift：api/openapi.yaml 的 paths 与 web/src/lib/apiTypes.ts 的 API_PATHS
//     必须一一对应（阶段 A 冻结后只允许向后兼容新增，禁止单边漂移）；
//   - TestPluginLayering：pkg/toolapi 与 internal/plugins/** 禁止 import
//     internal/{store,llm,agent,api,service}，插件之间亦禁止互相 import
//     （等价于 .golangci.yml 的 depguard 规则，本机无 golangci-lint 时的兜底门禁）。

import (
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

// TestContractPathsNoDrift 校验 HTTP 契约零漂移。
func TestContractPathsNoDrift(t *testing.T) {
	openapi := readOpenAPIPaths(t)
	ts := readAPITypesPaths(t)

	openapiSet := make(map[string]struct{}, len(openapi))
	for _, p := range openapi {
		openapiSet[p] = struct{}{}
	}
	tsSet := make(map[string]struct{}, len(ts))
	for _, p := range ts {
		tsSet[p] = struct{}{}
	}

	var missingInTS, missingInYAML []string
	for _, p := range openapi {
		if _, ok := tsSet[p]; !ok {
			missingInTS = append(missingInTS, p)
		}
	}
	for _, p := range ts {
		if _, ok := openapiSet[p]; !ok {
			missingInYAML = append(missingInYAML, p)
		}
	}
	if len(missingInTS) > 0 {
		t.Fatalf("openapi.yaml 有但 apiTypes.ts 缺失: %v（改契约必须两侧同步）", missingInTS)
	}
	if len(missingInYAML) > 0 {
		t.Fatalf("apiTypes.ts 有但 openapi.yaml 缺失: %v（前端不得自造端点）", missingInYAML)
	}
	t.Logf("契约一致：%d 条 path", len(openapi))
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
