package retrieval

import "testing"

func TestSelectToolsPrefersRetrieved(t *testing.T) {
	// 检索有命中时，必须只返回命中项，不得混入回退集（回退仅在检索为空时启用）。
	hits := []FusedHit{
		{DocID: 1, Key: "web_fetch", Score: 0.032},
		{DocID: 2, Key: "browser", Score: 0.016},
	}
	got := SelectTools(hits, SelectOptions{
		Query:       "抓取网页",
		TopK:        4,
		AlwaysNames: []string{"read_file"},
		RecentNames: []string{"grep"},
	})
	if len(got) != 2 {
		t.Fatalf("期望 2 条检索命中，得到 %d：%+v", len(got), got)
	}
	if got[0].Key != "web_fetch" || got[0].Source != "retrieved" {
		t.Fatalf("首位应为 web_fetch(retrieved)，得到 %+v", got[0])
	}
	if got[0].Score <= got[1].Score {
		t.Fatalf("应按分数降序：%v <= %v", got[0].Score, got[1].Score)
	}
}

func TestSelectToolsFallsBackWhenEmpty(t *testing.T) {
	// 检索为空必须回退：先 recent，再 core-fallback（否则 deferred 档不可发现）。
	got := SelectTools(nil, SelectOptions{
		Query:       "随便",
		TopK:        5,
		RecentNames: []string{"grep"},
		AlwaysNames: []string{"read_file", "list_dir"},
	})
	if len(got) != 3 {
		t.Fatalf("期望 3 条回退结果，得到 %d：%+v", len(got), got)
	}
	if got[0].Key != "grep" || got[0].Source != "recent" {
		t.Fatalf("首位应为 recent 的 grep，得到 %+v", got[0])
	}
	if got[1].Source != "core-fallback" || got[2].Source != "core-fallback" {
		t.Fatalf("后两位应为 core-fallback，得到 %+v", got[1:])
	}
}

func TestSelectToolsRespectsTopK(t *testing.T) {
	hits := []FusedHit{
		{DocID: 1, Key: "a", Score: 0.9},
		{DocID: 2, Key: "b", Score: 0.8},
		{DocID: 3, Key: "c", Score: 0.7},
		{DocID: 4, Key: "d", Score: 0.6},
	}
	got := SelectTools(hits, SelectOptions{TopK: 2})
	if len(got) != 2 {
		t.Fatalf("TopK=2 应截断到 2 条，得到 %d", len(got))
	}
	// TopK 截断同样作用于回退集，防「回退把注入撑成全量」。
	fb := SelectTools(nil, SelectOptions{
		TopK:        2,
		AlwaysNames: []string{"x", "y", "z"},
	})
	if len(fb) != 2 {
		t.Fatalf("回退集也应受 TopK 限制，得到 %d", len(fb))
	}
}

func TestSelectToolsDedupAndExclude(t *testing.T) {
	hits := []FusedHit{
		{DocID: 1, Key: "dup", Score: 0.9},
		{DocID: 2, Key: "dup", Score: 0.5}, // 重复键只保留首次
		{DocID: 3, Key: "core_one", Score: 0.4},
	}
	got := SelectTools(hits, SelectOptions{
		TopK:         5,
		ExcludeNames: []string{"core_one"}, // 已在常驻集，不得重复注入
	})
	if len(got) != 1 {
		t.Fatalf("去重+排除后期望 1 条，得到 %d：%+v", len(got), got)
	}
	if got[0].Key != "dup" {
		t.Fatalf("应保留 dup，得到 %s", got[0].Key)
	}
}

func TestSelectToolsEmptyNamesIgnored(t *testing.T) {
	// 空串与纯空白必须被忽略，不得产生空名工具。
	got := SelectTools(nil, SelectOptions{
		TopK:        5,
		RecentNames: []string{"", "  ", "ok"},
		AlwaysNames: []string{""},
	})
	if len(got) != 1 || got[0].Key != "ok" {
		t.Fatalf("空名应被忽略，得到 %+v", got)
	}
}

func TestClampTopK(t *testing.T) {
	opts := SelectOptions{TopK: 8, MinTopK: 2}
	if n := ClampTopK(opts, false); n != 8 {
		t.Fatalf("未降级应保持 8，得到 %d", n)
	}
	if n := ClampTopK(opts, true); n != 4 {
		t.Fatalf("降级减半应为 4，得到 %d", n)
	}
	// 减半后低于下限时取下限（不得砍到 0，否则模型完全看不见按需工具）。
	small := SelectOptions{TopK: 3, MinTopK: 2}
	if n := ClampTopK(small, true); n != 2 {
		t.Fatalf("3 减半为 1 低于下限 2，应取 2，得到 %d", n)
	}
	// TopK 缺省时用 DefaultTopK。
	if n := ClampTopK(SelectOptions{}, false); n != DefaultTopK {
		t.Fatalf("缺省 TopK 应为 %d，得到 %d", DefaultTopK, n)
	}
}

func TestSelectOptionsNormalize(t *testing.T) {
	o := SelectOptions{TopK: -1, MinTopK: 99}.normalize()
	if o.TopK != DefaultTopK {
		t.Fatalf("负 TopK 应回落默认值，得到 %d", o.TopK)
	}
	if o.MinTopK > o.TopK {
		t.Fatalf("MinTopK 不得大于 TopK，得到 %d > %d", o.MinTopK, o.TopK)
	}
}

func TestNames(t *testing.T) {
	got := Names([]SelectedTool{{Key: "a"}, {Key: "b"}})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Names 应保序提取，得到 %+v", got)
	}
	if len(Names(nil)) != 0 {
		t.Fatal("空输入应返回空切片")
	}
}
