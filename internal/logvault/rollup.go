package logvault

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ============ 用量聚合（走 rollup_hourly，不扫明细） ============

// TokenGroup 聚合分组维度。
type TokenGroup string

const (
	GroupByModel    TokenGroup = "model"
	GroupByProvider TokenGroup = "provider"
	GroupByDay      TokenGroup = "day"
)

// TokenQuery 用量聚合条件。
type TokenQuery struct {
	From    int64      // Unix 毫秒
	To      int64      // Unix 毫秒
	GroupBy TokenGroup // 默认 model
}

// TokenStat 单组聚合结果。
type TokenStat struct {
	Group            string  `json:"group"`
	Provider         string  `json:"provider,omitempty"`
	Calls            int64   `json:"calls"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	EstimatedCalls   int64   `json:"estimated_calls"`
	CacheHitRate     float64 `json:"cache_hit_rate"` // cached / prompt
}

// TokenSummary 用量聚合汇总。
type TokenSummary struct {
	From           int64       `json:"from"`
	To             int64       `json:"to"`
	GroupBy        string      `json:"group_by"`
	Items          []TokenStat `json:"items"`
	TotalCalls     int64       `json:"total_calls"`
	TotalPrompt    int64       `json:"total_prompt_tokens"`
	TotalCompletion int64      `json:"total_completion_tokens"`
	TotalCached    int64       `json:"total_cached_tokens"`
	TotalTokens    int64       `json:"total_tokens"`
	// EstimatedCalls 显式暴露估算占比 —— 绝不隐藏估算值。
	EstimatedCalls int64 `json:"estimated_calls"`
	// AvailableRange 可查范围（保留期边界），供前端明示「更早的数据已清理」。
	EarliestDay string `json:"earliest_day"`
	LatestDay   string `json:"latest_day"`
}

// AggregateTokens 按 from~to 聚合 token 用量。
//
// 数据源是 rollup_hourly（小时 × 模型 × 厂商），**不扫明细表**：
// 30 天 = 720 小时 × 模型数，数千行聚合通常 <20ms；明细 SUM 需 1~2s。
func (q *Querier) AggregateTokens(ctx context.Context, tq TokenQuery) (TokenSummary, error) {
	if tq.To < tq.From {
		return TokenSummary{}, fmt.Errorf("logvault: to 不能早于 from")
	}
	group := tq.GroupBy
	switch group {
	case GroupByModel, GroupByProvider, GroupByDay:
	default:
		group = GroupByModel
	}
	sum := TokenSummary{From: tq.From, To: tq.To, GroupBy: string(group)}
	sum.EarliestDay, sum.LatestDay = q.shard.AvailableRange()

	// 按分组维度决定 SELECT 列
	var groupExpr string
	switch group {
	case GroupByProvider:
		groupExpr = "provider"
	case GroupByDay:
		groupExpr = "day"
	default:
		groupExpr = "model"
	}

	type agg struct {
		stat     TokenStat
		provider string
	}
	buckets := make(map[string]*agg, 16)

	for _, day := range daysInRange(tq.From, tq.To) {
		dayStart, err := DayStartMs(day)
		if err != nil {
			continue
		}
		dayEnd, err := DayEndMs(day)
		if err != nil {
			continue
		}
		if tq.From > dayEnd || tq.To < dayStart {
			continue
		}
		// 换算成本天的小时区间（rollup 用 day+hour 而不是毫秒时间戳）
		hFrom, hTo := 0, 23
		if tq.From > dayStart {
			hFrom = hourOfDay(tq.From)
		}
		if tq.To < dayEnd {
			hTo = hourOfDay(tq.To)
		}
		db, dErr := q.shard.openDayReadOnly(day)
		if dErr != nil {
			continue // 该天库不存在（无数据或已清理）
		}
		query := fmt.Sprintf(`SELECT %s, provider, calls, prompt_tokens, completion_tokens,
			cached_tokens, estimated_calls FROM rollup_hourly
			WHERE day = ? AND hour BETWEEN ? AND ?`, groupExpr)
		rows, qErr := db.QueryContext(ctx, query, day, hFrom, hTo)
		if qErr != nil {
			_ = db.Close()
			continue
		}
		for rows.Next() {
			var g, provider string
			var calls, prompt, completion, cached, estimated int64
			if sErr := rows.Scan(&g, &provider, &calls, &prompt, &completion, &cached, &estimated); sErr != nil {
				continue
			}
			a, ok := buckets[g]
			if !ok {
				a = &agg{stat: TokenStat{Group: g}}
				buckets[g] = a
			}
			if a.provider == "" && provider != "" {
				a.provider = provider
			}
			a.stat.Calls += calls
			a.stat.PromptTokens += prompt
			a.stat.CompletionTokens += completion
			a.stat.CachedTokens += cached
			a.stat.EstimatedCalls += estimated
		}
		_ = rows.Close()
		_ = db.Close() // 只读临时连接：用完即关，不占 LRU
	}

	items := make([]TokenStat, 0, len(buckets))
	for _, a := range buckets {
		st := a.stat
		st.Provider = a.provider
		st.TotalTokens = st.PromptTokens + st.CompletionTokens
		if st.PromptTokens > 0 {
			st.CacheHitRate = float64(st.CachedTokens) / float64(st.PromptTokens)
		}
		items = append(items, st)
		sum.TotalCalls += st.Calls
		sum.TotalPrompt += st.PromptTokens
		sum.TotalCompletion += st.CompletionTokens
		sum.TotalCached += st.CachedTokens
		sum.TotalTokens += st.TotalTokens
		sum.EstimatedCalls += st.EstimatedCalls
	}
	// 按总 token 倒序（最耗 token 的排前面）
	sortTokenStats(items)
	sum.Items = items
	return sum, nil
}

// hourOfDay 取某毫秒时间戳在**本地时区**的小时（0~23）。
//
// 必须与 upsertRollup 使用的 time.UnixMilli(...).Hour() 口径一致（本地时区），
// 否则跨时区偏移会让聚合查询落在错误的小时桶里，表现为「明细有数据但聚合恒为 0」。
func hourOfDay(ms int64) int {
	return time.UnixMilli(ms).Hour()
}

// sortTokenStats 按总 token 倒序（同值按分组名升序，保证稳定输出）。
func sortTokenStats(items []TokenStat) {
	if len(items) < 2 {
		return
	}
	for i := 1; i < len(items); i++ {
		cur := items[i]
		j := i - 1
		for j >= 0 && (items[j].TotalTokens < cur.TotalTokens ||
			(items[j].TotalTokens == cur.TotalTokens && strings.Compare(items[j].Group, cur.Group) > 0)) {
			items[j+1] = items[j]
			j--
		}
		items[j+1] = cur
	}
}
