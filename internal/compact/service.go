package compact

// 压缩服务：把纯逻辑（Evaluate / PlanCompaction / BuildSummaryPrompt）
// 与存储、模型调用编排起来。
//
// # 分层意图
//
// 本文件是唯一允许产生 IO 的地方。这样可以做到：
//   - 水位与分区规则用纯函数单测覆盖，不需要数据库与模型桩；
//   - 存储失败、模型失败的处理策略集中在一处，不会散落。
//
// # 失败策略（关键设计）
//
// 压缩**永远不应让 Run 失败**。它是一项优化：成功则省 token，
// 失败则退回原样继续跑。因此这里所有错误只做日志与事件，不向上抛。
//
// 唯一例外是「压缩后反而更糟」——若摘要比原区间还长，必须拒绝写入，
// 否则等于用一次额外的模型调用买来更差的上下文。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"harness-go/internal/llm"
	"harness-go/internal/model"
)

// ErrSummaryTooLong 摘要不短于原区间，压缩无收益。
//
// 这不算失败，而是"这次不值得压"：保留原样是更优选择。
// 单独定义错误是为了让调用方区分"压了但没收益"与"压失败了"。
var ErrSummaryTooLong = errors.New("compact: 摘要未短于原区间，已放弃本次压缩")

// 压缩跳过原因码（SkipReason 的取值，全部为稳定字符串，可进事件与日志）。
//
// 为什么要枚举而不是自由文本：这些值会被前端/排障脚本按等值匹配消费
// （「为什么这个长会话从没压缩过？」是最常被问的问题）。
const (
	// SkipReasonWindowUnknown 模型上下文窗口未知（limit<=0），按设计不压缩。
	SkipReasonWindowUnknown = "window_unknown"
	// SkipReasonBelowWatermark 未达压缩水位（**正常高频路径**，非异常）。
	SkipReasonBelowWatermark = "below_watermark"
	// SkipReasonStoreReadFailed 读不到既有的压缩边界（存储异常）。
	SkipReasonStoreReadFailed = "store_read_failed"
	// SkipReasonPlanEmpty 分区后没有可折叠内容（例如未压缩区间只剩 1 条）。
	SkipReasonPlanEmpty = "plan_empty"
	// SkipReasonSummaryFailed 摘要模型调用失败。
	SkipReasonSummaryFailed = "summary_failed"
	// SkipReasonSummaryEmpty 摘要模型返回空内容。
	SkipReasonSummaryEmpty = "summary_empty"
	// SkipReasonTooLong 摘要不比原区间短，压缩无收益（拒绝写入更差的上下文）。
	SkipReasonTooLong = "summary_too_long"
	// SkipReasonCreateFailed 压缩记录落库失败。
	SkipReasonCreateFailed = "create_failed"
	// SkipReasonNotConfigured 未装配压缩能力（引擎侧判定）。
	SkipReasonNotConfigured = "not_configured"
)

// Store 压缩所需的存储端口（由 store.CompactionRepo 实现）。
//
// 只声明两个方法：压缩不需要更宽的接口。窄接口让测试桩极简，
// 也让"谁在用什么"一目了然。
type Store interface {
	Create(ctx context.Context, c *model.Compaction) error
	List(ctx context.Context) ([]model.Compaction, error)
	LatestEndSeq(ctx context.Context) (int64, error)
}

// Options 服务参数。
type Options struct {
	// Store 压缩记录存储（必需）。
	Store Store
	// Summarizer 摘要生成器（必需）。可为 SummarizerFunc。
	Summarizer Summarizer
	// Now 时钟注入（测试可替换）；nil 时用 time.Now。
	Now func() int64
	// Logger 可选日志（nil 时静默）。
	Logger *slog.Logger
	// OnCompacted 压缩成功后的回调（用于发事件、更新 UI 水位）。
	OnCompacted func(viewer Viewer, c model.Compaction)
}

// Service 压缩服务。
type Service struct {
	opts Options
}

// New 创建压缩服务。
func New(opts Options) (*Service, error) {
	if opts.Store == nil {
		return nil, errors.New("compact: Store 不能为空")
	}
	if opts.Summarizer == nil {
		return nil, errors.New("compact: Summarizer 不能为空")
	}
	if opts.Now == nil {
		opts.Now = func() int64 { return time.Now().UnixMilli() }
	}
	return &Service{opts: opts}, nil
}

// Result 一次压缩尝试的结果。
type Result struct {
	// Compacted 是否真的写入了一条压缩记录。
	Compacted bool
	// Skipped 是否因为无可压缩内容而跳过。
	Skipped bool
	// SkipReason 跳过/放弃的**明确原因**（Skipped 或 ErrSummaryTooLong 时有效）。
	//
	// 🚨 为什么必须显式记录（2026-11 教训）：
	// 「压缩没生效」此前完全不可观测 —— 会话 122559a3 烧到 839 万 token 时，
	// 日志里一条压缩相关的痕迹都没有，只能靠人肉读 SQL 才推断出「压缩真空」。
	// 现在每个放弃分支都必须留下原因码，供 run.status / error 事件透出：
	//   no_candidates / window_unknown / below_watermark / plan_empty /
	//   summary_failed / summary_empty / summary_too_long / create_failed /
	//   store_read_failed / not_configured
	SkipReason string
	// Viewer 压缩后的水位视图（供调用方发事件）。
	Viewer Viewer
	// Record 本次写入的压缩记录（Compacted 时有效）。
	Record model.Compaction
}

// CompactIfNeeded 在需要时执行一次压缩。
//
// 参数：
//   - used / limit：当前水位与窗口上限（来自 prompt 构建结果与模型元数据）；
//   - msgs：会话全量消息（按 seq 升序）；
//   - running：本 Run 是否正在执行（决定是否保护当前轮次）。
//
// 返回的 error 恒为 nil 语义上的"不致命"：调用方无需处理。
// 之所以仍返回 error，是为了让调用方能选择记录；但**绝不能**因它中断 Run。
func (s *Service) CompactIfNeeded(
	ctx context.Context,
	used, limit int,
	msgs []model.Message,
	running bool,
) (Result, error) {
	viewer := Evaluate(used, limit)
	if !viewer.NeedsCompaction {
		// 未达水位是**最常见的正常路径**（每一步都会走一遍），原因码用于
		// 排障时区分「没到水位」与「到了水位但压不动」，两者处置完全不同。
		reason := SkipReasonBelowWatermark
		if limit <= 0 {
			reason = SkipReasonWindowUnknown
		}
		return Result{Skipped: true, SkipReason: reason, Viewer: viewer}, nil
	}

	// 注意：这里用 WithoutCancel 保护压缩过程。
	// 压缩是"为后续轮次做准备"的工作，即使当前请求被取消，已投入的
	// 摘要工作也不应白费；更重要的是避免"取消后 is_running 判定错乱"。
	// 但仍加超时兜底，防止模型调用挂死。
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	latest, err := s.opts.Store.LatestEndSeq(cctx)
	if err != nil {
		s.logWarn("读取压缩边界失败，跳过本次压缩", "err", err)
		return Result{Skipped: true, SkipReason: SkipReasonStoreReadFailed, Viewer: viewer}, nil
	}

	plan := PlanCompaction(msgs, latest, running)
	if plan.Empty() {
		// 无可压缩内容：这是正常情况（例如单轮任务运行中），不是错误。
		return Result{Skipped: true, SkipReason: SkipReasonPlanEmpty, Viewer: viewer}, nil
	}

	prompt := BuildSummaryPrompt(plan.Messages)
	summary, err := s.opts.Summarizer.Summarize(cctx, prompt)
	if err != nil {
		s.logWarn("生成摘要失败，跳过本次压缩", "err", err)
		return Result{Skipped: true, SkipReason: SkipReasonSummaryFailed, Viewer: viewer}, nil
	}
	summary = trimSummary(summary)
	if summary == "" {
		s.logWarn("摘要为空，跳过本次压缩")
		return Result{Skipped: true, SkipReason: SkipReasonSummaryEmpty, Viewer: viewer}, nil
	}
	// 收益校验：摘要必须有实质性缩短，否则保留原样更优。
	if estimateText(summary) >= plan.EstimatedTokens {
		return Result{Skipped: true, SkipReason: SkipReasonTooLong, Viewer: viewer}, ErrSummaryTooLong
	}

	rec := model.Compaction{
		ID:               fmt.Sprintf("cmp-%d-%d", plan.EndSeq, s.opts.Now()),
		StartSeq:         plan.StartSeq,
		EndSeq:           plan.EndSeq,
		SummaryMarkdown:  summary,
		ReplacedMessages: len(plan.Messages),
		ReplacedTokens:   plan.EstimatedTokens,
		CreatedAt:        s.opts.Now(),
	}
	if err := s.opts.Store.Create(cctx, &rec); err != nil {
		s.logWarn("写入压缩记录失败，跳过本次压缩", "err", err)
		return Result{Skipped: true, SkipReason: SkipReasonCreateFailed, Viewer: viewer}, nil
	}

	// 压缩后重新计算水位：用摘要 token 替代被折叠区间。
	after := viewer.Used - plan.EstimatedTokens + estimateText(summary)
	afterViewer := Evaluate(after, limit)
	if s.opts.OnCompacted != nil {
		s.opts.OnCompacted(afterViewer, rec)
	}
	return Result{Compacted: true, Viewer: afterViewer, Record: rec}, nil
}

// trimSummary 清理模型输出中的常见包装（代码块围栏、多余空行）。
//
// 模型经常把摘要包在 ```markdown ... ``` 里。保留围栏会让摘要看起来像
// 代码而非对话内容，且浪费 token。这里只做最小清理，不改动正文。
func trimSummary(s string) string {
	s = trimSpace(s)
	if len(s) < 12 {
		return s
	}
	// 去掉整体包裹的代码块围栏。
	for _, fence := range []string{"```markdown", "```md", "```"} {
		if hasPrefixFold(s, fence) && hasSuffixFold(s, "```") {
			inner := s[len(fence) : len(s)-3]
			return trimSpace(inner)
		}
	}
	return s
}

// LoadHistory 读取消息与压缩记录，返回模型应看到的完整历史。
//
// 这是读取侧的唯一入口：所有需要"把历史喂给模型"的地方都应走它，
// 以保证已压缩区间一定被摘要替代（见 History 的说明）。
func (s *Service) LoadHistory(ctx context.Context, msgs []model.Message) ([]llm.Message, error) {
	comps, err := s.opts.Store.List(ctx)
	if err != nil {
		// 读不到压缩记录时退回原始历史：宁可多花 token，也不能让会话无法继续。
		s.logWarn("读取压缩记录失败，退回未压缩历史", "err", err)
		return History(msgs, nil), nil
	}
	return History(msgs, comps), nil
}

func (s *Service) logWarn(msg string, args ...any) {
	if s.opts.Logger != nil {
		s.opts.Logger.Warn(msg, args...)
	}
}

// trimSpace 去掉首尾空白（含 Windows 换行与不换行空格）。
func trimSpace(s string) string {
	return strings.TrimSpace(s)
}

// hasPrefixFold 大小写不敏感的前缀判断（ASCII 足够：这里是代码围栏）。
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// hasSuffixFold 大小写不敏感的后缀判断。
func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
}
