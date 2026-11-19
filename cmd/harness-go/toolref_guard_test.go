package main

import (
	"sort"
	"strings"
	"testing"

	"harness-go/internal/sop"
)

// 提示词工具名一致性门禁（阶段 0）。
//
// # 这份测试守的是什么
//
// 工具名是**机器契约**：文本里教模型调用什么名字，注册表里就必须有同名可执行体。
// 但提示词是自然语言，编译器看不见、类型系统管不着，于是漂移可以长期潜伏：
// 文本教 `create_plan`，注册表里却只有 `plan_set` → 模型拿到「工具未注册」，
// 然后自行改试别名，或干脆跳过立项门禁。表现为「功能时灵时不灵」，极难排查。
//
// 本测试把那条**隐形的跨域契约**变成红灯。
//
// # 为什么放在 cmd/harness-go
//
// depguard 铁律：internal/sop 只依赖标准库，严禁 import 插件（否则内核反向
// 依赖插件）。而本测试必须**同时**看到「提示词文本」与「真实注册的工具名」，
// 唯一合法的交集就是装配点 cmd/harness-go（它本就同时 import 两者）。
//
// # 扫描范围
//
//   - SOP 全部变体正文（模型真实看到的措辞）；
//   - 核心 SOP agent-core.md（人格与基础口径）；
//   - defaultSystemBase（未装配 SOP 服务时的兜底文本，与 agent-core.md 同源）。
func TestPromptToolRefsExist(t *testing.T) {
	registered := registeredToolNames()
	if len(registered) == 0 {
		t.Fatal("未取到任何已注册工具名：门禁失去比对基准，请检查 builtinFactories")
	}

	texts := append(sop.CatalogTexts(), sop.BuiltinCoreText(), defaultSystemBase)
	refs := sop.ExtractToolRefs(texts...)

	var missing []string
	for _, ref := range refs {
		if _, ok := registered[ref]; !ok {
			missing = append(missing, ref)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("提示词引用了不存在的工具（模型会拿到「工具未注册」的软失败）：\n  %s\n\n"+
			"已注册工具共 %d 个：%s\n"+
			"修法：二选一 —— ① 把提示词改成真实工具名；② 让工具真的以该名注册。\n"+
			"禁止在本测试里加白名单绕过（护栏一旦被绕，漂移就会复发）。",
			strings.Join(missing, "\n  "), len(registered), strings.Join(toolNamesSorted(registered), ", "))
	}
}

// registeredToolNames 收集内置插件清单声明的全部工具名。
//
// 刻意读 **Manifest 声明** 而非运行期 registry：本门禁要校验的是「声明的契约」，
// 不依赖 host 装配是否成功（host 未就绪不应让契约校验失真）。
// Manifest ↔ 实际注册 的一一对应另有各插件单测守护（如 browser_test.go）。
func registeredToolNames() map[string]struct{} {
	out := make(map[string]struct{})
	for _, f := range builtinFactories {
		for _, spec := range f.Manifest().Tools {
			if name := strings.TrimSpace(spec.Name); name != "" {
				out[name] = struct{}{}
			}
		}
	}
	return out
}

// toolNamesSorted 返回有序键列表（测试失败信息需要确定性输出，便于比对）。
func toolNamesSorted(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
