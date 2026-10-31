package retrieval

import (
	"math"
	"testing"
)

func TestEncodeDecodeVectorRoundTrip(t *testing.T) {
	src := []float32{0, 1, -1, 3.5, -0.25, 1e-6}
	blob, err := EncodeVector(src)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if len(blob) != 4*len(src) {
		t.Fatalf("BLOB 长度期望 %d，得到 %d", 4*len(src), len(blob))
	}
	if Dim(blob) != len(src) {
		t.Fatalf("Dim 期望 %d，得到 %d", len(src), Dim(blob))
	}
	got, err := DecodeVector(blob)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if len(got) != len(src) {
		t.Fatalf("解码长度期望 %d，得到 %d", len(src), len(got))
	}
	for i := range src {
		if got[i] != src[i] {
			t.Fatalf("第 %d 位不一致：期望 %v，得到 %v", i, src[i], got[i])
		}
	}
}

func TestEncodeVectorRejectsEmpty(t *testing.T) {
	if _, err := EncodeVector(nil); err != ErrEmptyVector {
		t.Fatalf("空向量应返回 ErrEmptyVector，得到 %v", err)
	}
	if _, err := DecodeVector(nil); err != ErrEmptyVector {
		t.Fatalf("空 BLOB 应返回 ErrEmptyVector，得到 %v", err)
	}
}

func TestDecodeVectorRejectsBadLength(t *testing.T) {
	if _, err := DecodeVector([]byte{1, 2, 3}); err == nil {
		t.Fatal("长度非 4 倍数应报错")
	}
}

func TestCosineBasics(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"同向", []float32{1, 0, 0}, []float32{2, 0, 0}, 1},
		{"正交", []float32{1, 0, 0}, []float32{0, 1, 0}, 0},
		{"反向", []float32{1, 0, 0}, []float32{-3, 0, 0}, -1},
		{"斜向", []float32{1, 1, 0}, []float32{1, 0, 0}, 1 / math.Sqrt2},
	}
	for _, c := range cases {
		got := Cosine(c.a, c.b)
		if math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("%s：期望 %v，得到 %v", c.name, c.want, got)
		}
	}
}

func TestCosineEdgeCasesReturnZero(t *testing.T) {
	// 边界必须返回 0 而不是 NaN：NaN 会污染 sort 比较，导致排序不确定。
	if v := Cosine(nil, nil); v != 0 {
		t.Fatalf("空向量应返回 0，得到 %v", v)
	}
	if v := Cosine([]float32{1, 2}, []float32{1, 2, 3}); v != 0 {
		t.Fatalf("维度不一致应返回 0，得到 %v", v)
	}
	if v := Cosine([]float32{0, 0}, []float32{1, 2}); v != 0 {
		t.Fatalf("零向量应返回 0，得到 %v", v)
	}
	if v := Cosine([]float32{0, 0}, []float32{0, 0}); math.IsNaN(v) {
		t.Fatal("零向量不得返回 NaN")
	}
}

func TestRRFFusion(t *testing.T) {
	// 手算用例（k=60）：
	//   doc1 关键词第 1 名 + 向量第 1 名 → 1/61 + 1/61 = 2/61
	//   doc2 仅关键词第 2 名            → 1/62
	//   doc3 仅向量第 2 名              → 1/62
	// doc2 与 doc3 同分 → 按 DocID 升序（2 < 3），故顺序为 doc1, doc2, doc3。
	kw := []KeywordHit{{DocID: 1, Key: "a"}, {DocID: 2, Key: "b"}}
	vec := []VectorHit{{DocID: 1, Key: "a", Score: 0.9}, {DocID: 3, Key: "c", Score: 0.8}}

	got := RRF(kw, vec, 60)
	if len(got) != 3 {
		t.Fatalf("期望 3 条融合结果，得到 %d", len(got))
	}
	if got[0].DocID != 1 || got[1].DocID != 2 || got[2].DocID != 3 {
		t.Fatalf("顺序错误：期望 [1 2 3]，得到 [%d %d %d]", got[0].DocID, got[1].DocID, got[2].DocID)
	}
	wantTop := 2.0 / 61.0
	if math.Abs(got[0].Score-wantTop) > 1e-12 {
		t.Fatalf("doc1 融合分期望 %v，得到 %v", wantTop, got[0].Score)
	}
	if got[0].KeywordRank != 1 || got[0].VectorRank != 1 {
		t.Fatalf("doc1 双通道名次错误：kw=%d vec=%d", got[0].KeywordRank, got[0].VectorRank)
	}
	if got[1].VectorRank != 0 {
		t.Fatalf("doc2 未命中向量通道，VectorRank 应为 0，得到 %d", got[1].VectorRank)
	}
	if got[0].Cosine != 0.9 {
		t.Fatalf("doc1 应携带原始余弦 0.9，得到 %v", got[0].Cosine)
	}
}

func TestRRFSingleChannelStillRecalls(t *testing.T) {
	// 单路独有结果必须被召回（RRF 的核心价值，也是「宽进严出」的检索侧保证）。
	got := RRF([]KeywordHit{{DocID: 7, Key: "only-kw"}}, nil, 0) // k<=0 回落 60
	if len(got) != 1 || got[0].DocID != 7 {
		t.Fatalf("单路结果应被召回，得到 %+v", got)
	}
	if math.Abs(got[0].Score-1.0/61.0) > 1e-12 {
		t.Fatalf("k 应回落 60，分数期望 %v，得到 %v", 1.0/61.0, got[0].Score)
	}
}

func TestRRFEmpty(t *testing.T) {
	if got := RRF(nil, nil, 60); len(got) != 0 {
		t.Fatalf("空输入应返回空结果，得到 %d 条", len(got))
	}
}

func TestTopN(t *testing.T) {
	hits := []FusedHit{{DocID: 1}, {DocID: 2}, {DocID: 3}}
	if got := TopN(hits, 2); len(got) != 2 {
		t.Fatalf("TopN(2) 期望 2 条，得到 %d", len(got))
	}
	if got := TopN(hits, 0); len(got) != 3 {
		t.Fatalf("TopN(0) 应原样返回，得到 %d", len(got))
	}
	if got := TopN(hits, 99); len(got) != 3 {
		t.Fatalf("TopN 超长应原样返回，得到 %d", len(got))
	}
}

func TestHashContentStableAndFieldSensitive(t *testing.T) {
	a := HashContent(KindTool, "read_file", "读文件", "正文")
	b := HashContent(KindTool, "read_file", "读文件", "正文")
	if a != b {
		t.Fatal("相同内容哈希必须一致（幂等前提）")
	}
	// 任一字段变化都必须改变哈希（否则会漏更新索引）。
	if HashContent(KindTool, "read_file", "读文件", "正文2") == a {
		t.Fatal("正文变化必须改变哈希")
	}
	if HashContent(KindTool, "read_file2", "读文件", "正文") == a {
		t.Fatal("Key 变化必须改变哈希")
	}
	if HashContent(KindMemoryL2, "read_file", "读文件", "正文") == a {
		t.Fatal("Kind 变化必须改变哈希")
	}
	// 分隔符防碰撞：字段边界必须参与哈希，避免 ("ab","c") 与 ("a","bc") 撞车。
	if HashContent(KindTool, "ab", "c", "") == HashContent(KindTool, "a", "bc", "") {
		t.Fatal("字段拼接必须防碰撞")
	}
}

func TestDocNormalize(t *testing.T) {
	d := Doc{
		Kind:       KindTool,
		Key:        "  read_file  ",
		Title:      "读文件\n\n工具",
		Body:       "第一段\r\n\r\n\r\n\r\n第二段",
		Importance: 0,
	}
	got, ok := d.Normalize()
	if !ok {
		t.Fatal("合法文档应通过校验")
	}
	if got.Key != "read_file" {
		t.Fatalf("Key 应去首尾空白，得到 %q", got.Key)
	}
	if got.Title != "读文件 工具" {
		t.Fatalf("Title 应折叠换行为空格，得到 %q", got.Title)
	}
	if got.Body != "第一段\n\n第二段" {
		t.Fatalf("Body 应折叠连续空行，得到 %q", got.Body)
	}
	if got.Importance != 0.5 {
		t.Fatalf("零重要性应回落 0.5，得到 %v", got.Importance)
	}
	if got.ContentHash == "" {
		t.Fatal("Normalize 应自动填充 ContentHash")
	}
}

func TestDocNormalizeRejects(t *testing.T) {
	if _, ok := (Doc{Kind: KindTool, Key: "   "}).Normalize(); ok {
		t.Fatal("空 Key 应被拒绝")
	}
	if _, ok := (Doc{Kind: Kind("bogus"), Key: "k"}).Normalize(); ok {
		t.Fatal("非法 Kind 应被拒绝")
	}
}

func TestDocNormalizeClampsImportance(t *testing.T) {
	hi, _ := Doc{Kind: KindTool, Key: "k", Importance: 9}.Normalize()
	if hi.Importance != 1 {
		t.Fatalf("重要性应钳制到 1，得到 %v", hi.Importance)
	}
	lo, _ := Doc{Kind: KindTool, Key: "k", Importance: -3}.Normalize()
	if lo.Importance != 0 {
		t.Fatalf("重要性应钳制到 0，得到 %v", lo.Importance)
	}
}

func TestDocNormalizeTitleTruncatesByRune(t *testing.T) {
	long := ""
	for i := 0; i < 300; i++ {
		long += "中"
	}
	got, _ := Doc{Kind: KindTool, Key: "k", Title: long}.Normalize()
	if n := len([]rune(got.Title)); n != 200 {
		t.Fatalf("标题应按 rune 截断到 200，得到 %d", n)
	}
}
