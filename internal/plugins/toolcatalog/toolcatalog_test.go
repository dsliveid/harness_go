package toolcatalog

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

// catalogHost 给 plugintest.Host 加装工具目录能力。
type catalogHost struct {
	*plugintest.Host
	specs []toolapi.Spec
}

func (h catalogHost) ToolSpecs() []toolapi.Spec { return h.specs }

func spec(name, tier, risk, desc, schema string) toolapi.Spec {
	return toolapi.Spec{
		Name: name, Description: desc, Tier: toolapi.Tier(tier),
		Risk: toolapi.Risk(risk), SchemaJSON: []byte(schema),
	}
}

// TestLookupReturnsSchemaForOnDemand on_demand 档工具必须能通过 lookup_tool 取到 schema。
//
// 这是本插件存在的**根本理由**：on_demand 是零注入档（模型完全看不到），
// 改造前没有任何手段能取到它的参数规范 → 只能盲猜调用。
func TestLookupReturnsSchemaForOnDemand(t *testing.T) {
	h := catalogHost{Host: plugintest.New(t), specs: []toolapi.Spec{
		spec("record_memory", "on_demand", "write", "沉淀记忆",
			`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`),
	}}
	res, err := lookupTool(context.Background(), h, &toolapi.Call{Args: []byte(`{"name":"record_memory"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("不应报错: %q", res.Content)
	}
	for _, want := range []string{"record_memory", "沉淀记忆", "on_demand", "write", `"title"`} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("规范缺少 %q: %q", want, res.Content)
		}
	}
}

// TestLookupUnknownToolIsExplicit 未知工具必须显式报错，并指引查看名录。
func TestLookupUnknownToolIsExplicit(t *testing.T) {
	h := catalogHost{Host: plugintest.New(t), specs: []toolapi.Spec{
		spec("read_file", "core", "readonly", "读文件", `{}`),
	}}
	res, _ := lookupTool(context.Background(), h, &toolapi.Call{Args: []byte(`{"name":"no_such_tool"}`)})
	if !res.IsError || !strings.Contains(res.Content, "未找到工具") {
		t.Fatalf("未知工具必须显式报错: %q", res.Content)
	}
	if !strings.Contains(res.Content, "省略 name") {
		t.Fatalf("应指引省略 name 查看名录: %q", res.Content)
	}
}

// TestLookupListGroupsByTier 省略 name 时按 tier 分组列出，让模型知道哪些需要先 lookup。
func TestLookupListGroupsByTier(t *testing.T) {
	h := catalogHost{Host: plugintest.New(t), specs: []toolapi.Spec{
		spec("read_file", "core", "readonly", "读文件", `{}`),
		spec("create_plan", "deferred", "write", "建计划", `{}`),
		spec("record_memory", "on_demand", "write", "记忆", `{}`),
	}}
	res, _ := lookupTool(context.Background(), h, &toolapi.Call{Args: []byte(`{}`)})
	c := res.Content
	for _, want := range []string{"core", "deferred", "on_demand", "read_file", "create_plan", "record_memory"} {
		if !strings.Contains(c, want) {
			t.Fatalf("名录缺少 %q: %q", want, c)
		}
	}
}

// TestLookupWithoutCatalogHostIsExplicit 未实现工具目录能力时必须诚实报错。
//
// 插件层不能崩溃、也不能假装"没有工具"——那会让调用方误判为配置问题。
func TestLookupWithoutCatalogHostIsExplicit(t *testing.T) {
	res, _ := lookupTool(context.Background(), plugintest.New(t), &toolapi.Call{Args: []byte(`{}`)})
	if !res.IsError || !strings.Contains(res.Content, "未接线") {
		t.Fatalf("未接线必须显式报错: %q", res.Content)
	}
}

// TestLookupEmptyRegistryIsExplicit 注册表为空必须报错（而非返回空名录）。
func TestLookupEmptyRegistryIsExplicit(t *testing.T) {
	h := catalogHost{Host: plugintest.New(t), specs: nil}
	res, _ := lookupTool(context.Background(), h, &toolapi.Call{Args: []byte(`{}`)})
	if !res.IsError || !strings.Contains(res.Content, "注册表为空") {
		t.Fatalf("空注册表必须显式报错: %q", res.Content)
	}
}

// TestManifestIsCoreTier lookup_tool 自己必须是 core 档。
//
// 它是三档工具的自举入口：若它自己不进 core（模型看不到它），
// deferred / on_demand 就永远无法被发现——整条链在此处断掉。
func TestManifestIsCoreTier(t *testing.T) {
	tools := Manifest().Tools
	if len(tools) != 1 || tools[0].Name != "lookup_tool" {
		t.Fatalf("应只声明 lookup_tool: %+v", tools)
	}
	if tools[0].Tier != toolapi.TierCore {
		t.Fatalf("lookup_tool 必须是 core 档（自举入口），实际 %q", tools[0].Tier)
	}
	if tools[0].Risk != toolapi.RiskReadonly {
		t.Fatalf("lookup_tool 应为 readonly，实际 %q", tools[0].Risk)
	}
}
