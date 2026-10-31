package memory

import "testing"

// ============ 四分类去重（mem0 思路，>0.92 判 NOOP） ============

func TestClassifyNoSimilarCandidates(t *testing.T) {
	if got := Classify(nil, false); got != OpAdd {
		t.Fatalf("无候选应判 ADD，得到 %v", got)
	}
	if got := Classify([]Similar{}, false); got != OpAdd {
		t.Fatalf("空候选应判 ADD，得到 %v", got)
	}
}

func TestClassifyNoopAboveThreshold(t *testing.T) {
	// 边界：> 0.92 判 NOOP（恰好 0.92 不算，是严格大于）
	for _, s := range []float64{0.921, 0.95, 1.0} {
		if got := Classify([]Similar{{Key: "k", Score: s}}, false); got != OpNoop {
			t.Fatalf("相似度 %v 应判 NOOP，得到 %v", s, got)
		}
	}
	if got := Classify([]Similar{{Key: "k", Score: 0.92}}, false); got != OpUpdate {
		t.Fatalf("恰好 0.92 不属严格大于，应进入 UPDATE 带，得到 %v", got)
	}
}

func TestClassifyUpdateBand(t *testing.T) {
	// 0.80 < sim <= 0.92：同一事实的新表述 → UPDATE 覆盖
	for _, s := range []float64{0.801, 0.85, 0.9199} {
		if got := Classify([]Similar{{Key: "k", Score: s}}, false); got != OpUpdate {
			t.Fatalf("相似度 %v 应判 UPDATE，得到 %v", s, got)
		}
	}
	// 恰好 0.80 不属严格大于 → ADD
	if got := Classify([]Similar{{Key: "k", Score: 0.80}}, false); got != OpAdd {
		t.Fatalf("恰好 0.80 应判 ADD，得到 %v", got)
	}
}

func TestClassifyAddBelowThreshold(t *testing.T) {
	for _, s := range []float64{-1, 0, 0.3, 0.7999} {
		if got := Classify([]Similar{{Key: "k", Score: s}}, false); got != OpAdd {
			t.Fatalf("相似度 %v 应判 ADD，得到 %v", s, got)
		}
	}
}

func TestClassifyContradictsDeletes(t *testing.T) {
	// 矛盾判定来自上层语义方（LLM/人工）：本层只提供口径，绝不臆测 DELETE
	if got := Classify(nil, true); got != OpDelete {
		t.Fatalf("矛盾应判 DELETE（即使无相似候选），得到 %v", got)
	}
	if got := Classify([]Similar{{Key: "k", Score: 0.3}}, true); got != OpDelete {
		t.Fatalf("矛盾应优先于相似度判定，得到 %v", got)
	}
	// 无判定方时（contradicts=false）永远不出 DELETE —— 机械规则不可臆测删数据
	for _, s := range []float64{0.3, 0.85, 0.99} {
		if got := Classify([]Similar{{Key: "k", Score: s}}, false); got == OpDelete {
			t.Fatalf("无判定方时不得产出 DELETE（sim=%v）", s)
		}
	}
}

// ============ 检索打分（0.6cos + 0.25衰减 + 0.15重要性） ============

func TestDecayHalfLife(t *testing.T) {
	const day = 24 * 60 * 60 * 1000
	now := int64(1_000_000_000_000)
	if d := Decay(now, now); d != 1 {
		t.Fatalf("零衰减应为 1，得到 %v", d)
	}
	if d := Decay(now-14*day, now); d < 0.499 || d > 0.501 {
		t.Fatalf("半衰期 14 天应衰减到 0.5，得到 %v", d)
	}
	if d := Decay(now-28*day, now); d < 0.249 || d > 0.251 {
		t.Fatalf("28 天应衰减到 0.25，得到 %v", d)
	}
	if d := Decay(now+day, now); d != 1 {
		t.Fatalf("未来时间（时钟回拨）按 0 天处理，得到 %v", d)
	}
}

func TestScoreWeights(t *testing.T) {
	const day = 24 * 60 * 60 * 1000
	now := int64(1_000_000_000_000)
	// 满分：cos=1 + 新鲜 + 重要性 1 → 恰好 1.0
	if s := Score(1, now, now, 1); s < 0.999 || s > 1.001 {
		t.Fatalf("满分应≈1.0，得到 %v", s)
	}
	// 权重和 = 1（口径防漂移）
	if WeightCos+WeightDecay+WeightImportance != 1.0 {
		t.Fatalf("权重和必须为 1，实际 %v", WeightCos+WeightDecay+WeightImportance)
	}
	// 无向量（cos=0）时诚实降级：只剩衰减+重要性，上限 0.4
	if s := Score(0, now, now, 1); s > 0.4001 {
		t.Fatalf("无向量时分数不得超过 0.4，得到 %v", s)
	}
	// 越界钳制
	if s := Score(5, now, now, 5); s < 0.999 || s > 1.001 {
		t.Fatalf("越界值应钳制到 [0,1] 后计满分，得到 %v", s)
	}
}
