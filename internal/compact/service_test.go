package compact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"harness-go/internal/model"
)

// memStore 内存版压缩存储桩。
type memStore struct {
	records   []model.Compaction
	createErr error
	listErr   error
}

func (m *memStore) Create(_ context.Context, c *model.Compaction) error {
	if m.createErr != nil {
		return m.createErr
	}
	// 复刻真实仓储的单调性校验，保证测试覆盖到真实约束。
	if len(m.records) > 0 {
		last := m.records[len(m.records)-1]
		if c.EndSeq <= last.EndSeq {
			return errors.New("out of order")
		}
	}
	m.records = append(m.records, *c)
	return nil
}

func (m *memStore) List(context.Context) ([]model.Compaction, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.records, nil
}

func (m *memStore) LatestEndSeq(context.Context) (int64, error) {
	if len(m.records) == 0 {
		return 0, nil
	}
	return m.records[len(m.records)-1].EndSeq, nil
}

// newSvc 构造一个使用桩存储与固定摘要器的服务。
func newSvc(t *testing.T, summary string) (*Service, *memStore) {
	t.Helper()
	st := &memStore{}
	svc, err := New(Options{
		Store:      st,
		Summarizer: SummarizerFunc(func(context.Context, string) (string, error) { return summary, nil }),
		Now:        func() int64 { return 1000 },
	})
	if err != nil {
		t.Fatalf("创建服务失败: %v", err)
	}
	return svc, st
}

// TestCompactSkippedBelowThreshold 未达水位不压缩（也不调模型）。
func TestCompactSkippedBelowThreshold(t *testing.T) {
	svc, st := newSvc(t, "摘要")
	msgs := []model.Message{
		msg(1, model.RoleUser, "a"), msg(2, model.RoleAssistant, "b"),
	}
	res, err := svc.CompactIfNeeded(context.Background(), 100, 1000, msgs, false)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if res.Compacted || !res.Skipped {
		t.Error("未达水位应跳过")
	}
	if len(st.records) != 0 {
		t.Error("不应写入任何压缩记录")
	}
}

// TestCompactWritesRecord 达水位且可压缩时写入记录。
func TestCompactWritesRecord(t *testing.T) {
	svc, st := newSvc(t, strings.Repeat("摘要", 20))
	msgs := []model.Message{
		msg(1, model.RoleUser, strings.Repeat("内容", 200)),
		msg(2, model.RoleAssistant, strings.Repeat("回复", 200)),
	}
	res, err := svc.CompactIfNeeded(context.Background(), 900, 1000, msgs, false)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("应完成压缩，得到 %+v", res)
	}
	if len(st.records) != 1 {
		t.Fatalf("应写入 1 条记录，得到 %d", len(st.records))
	}
	rec := st.records[0]
	if rec.StartSeq != 1 || rec.EndSeq != 2 {
		t.Errorf("区间应为 [1,2]，得到 [%d,%d]", rec.StartSeq, rec.EndSeq)
	}
	if rec.ReplacedMessages != 2 {
		t.Errorf("替换条数应为 2，得到 %d", rec.ReplacedMessages)
	}
	// 压缩后水位应下降。
	if res.Viewer.Used >= 900 {
		t.Errorf("压缩后水位应下降，得到 %d", res.Viewer.Used)
	}
}

// TestCompactRejectsUnprofitableSummary 摘要不短于原区间时必须放弃。
//
// 这条保护很关键：否则「为了省 token 而多调一次模型」反而让上下文更长，
// 是纯粹的负收益。
func TestCompactRejectsUnprofitableSummary(t *testing.T) {
	long := strings.Repeat("很长的摘要内容", 500)
	svc, st := newSvc(t, long)
	msgs := []model.Message{
		msg(1, model.RoleUser, "短"),
		msg(2, model.RoleAssistant, "短"),
	}
	res, err := svc.CompactIfNeeded(context.Background(), 900, 1000, msgs, false)
	if !errors.Is(err, ErrSummaryTooLong) {
		t.Fatalf("应返回 ErrSummaryTooLong，得到 %v", err)
	}
	if res.Compacted {
		t.Error("无收益时不应写入记录")
	}
	if len(st.records) != 0 {
		t.Error("无收益时不得落库")
	}
}

// TestCompactSingleTurnRunningStillCompacts 单轮长任务运行中也要压缩（事故会话形态）。
//
// 与旧断言相反（见 compact_test.go TestPlanRunningSingleTurnStillCompacts 的说明）：
// 「运行中不压缩」会让单轮超长任务彻底失去容量治理 —— 事故会话 122559a3 正是
// 1 条 user + 217 条工具步骤，session_compactions 恒为 0，账单烧到 839 万 token。
func TestCompactSingleTurnRunningStillCompacts(t *testing.T) {
	svc, st := newSvc(t, "摘要")
	// 复刻真实形态：1 条 user + 多组 assistant(tool_calls)/tool。
	// （只有 2 条消息是退化形态：折叠任一条都会留下孤儿 tool 或丢失全部现场，
	//   此时不压缩才是对的，因此这里必须用有代表性的历史。）
	msgs := []model.Message{msg(1, model.RoleUser, strings.Repeat("任务描述", 200))}
	for i := int64(2); i <= 20; i += 2 {
		msgs = append(msgs, toolCallMsg(i))
		msgs = append(msgs, msg(i+1, model.RoleTool, strings.Repeat("输出", 200)))
	}
	res, _ := svc.CompactIfNeeded(context.Background(), 900, 1000, msgs, true)
	if !res.Compacted {
		t.Fatalf("单轮长任务运行中必须能压缩，实际跳过原因=%q", res.SkipReason)
	}
	if len(st.records) != 1 {
		t.Fatalf("应落库 1 条压缩记录，实际 %d", len(st.records))
	}
}

// TestCompactStoreFailureDoesNotFailRun 存储失败不得让 Run 失败。
//
// 压缩是优化项：失败就退回原样继续跑，绝不中断业务。
func TestCompactStoreFailureDoesNotFailRun(t *testing.T) {
	st := &memStore{createErr: errors.New("db down")}
	svc, err := New(Options{
		Store:      st,
		Summarizer: SummarizerFunc(func(context.Context, string) (string, error) { return "摘要", nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []model.Message{
		msg(1, model.RoleUser, strings.Repeat("x", 2000)),
	}
	res, err := svc.CompactIfNeeded(context.Background(), 900, 1000, msgs, false)
	if err != nil {
		t.Fatalf("存储失败不应向上抛错: %v", err)
	}
	if res.Compacted {
		t.Error("存储失败时不应报告已压缩")
	}
}

// TestCompactSummarizerFailureDoesNotFailRun 摘要失败同样不致命。
func TestCompactSummarizerFailureDoesNotFailRun(t *testing.T) {
	st := &memStore{}
	svc, _ := New(Options{
		Store: st,
		Summarizer: SummarizerFunc(func(context.Context, string) (string, error) {
			return "", errors.New("model down")
		}),
	})
	msgs := []model.Message{msg(1, model.RoleUser, strings.Repeat("x", 2000))}
	res, err := svc.CompactIfNeeded(context.Background(), 900, 1000, msgs, false)
	if err != nil {
		t.Fatalf("模型失败不应向上抛错: %v", err)
	}
	if res.Compacted {
		t.Error("模型失败时不应报告已压缩")
	}
	if len(st.records) != 0 {
		t.Error("模型失败时不得落库")
	}
}

// TestCompactRespectsExistingBoundary 已压缩的部分不得再次压缩。
func TestCompactRespectsExistingBoundary(t *testing.T) {
	svc, st := newSvc(t, "摘要")
	st.records = append(st.records, model.Compaction{StartSeq: 1, EndSeq: 2, SummaryMarkdown: "旧"})

	msgs := []model.Message{
		msg(1, model.RoleUser, "已压缩1"),
		msg(2, model.RoleAssistant, "已压缩2"),
		msg(3, model.RoleUser, strings.Repeat("新的内容", 200)),
		msg(4, model.RoleAssistant, strings.Repeat("新的回复", 200)),
	}
	res, err := svc.CompactIfNeeded(context.Background(), 900, 1000, msgs, false)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("应压缩新区间，得到 %+v", res)
	}
	if res.Record.StartSeq != 3 {
		t.Errorf("新区间应从 seq3 开始，得到 %d", res.Record.StartSeq)
	}
}

// TestLoadHistoryFallsBackOnStoreError 读取失败时退回未压缩历史。
//
// 宁可多花 token，也不能让会话因压缩表读失败而无法继续。
func TestLoadHistoryFallsBackOnStoreError(t *testing.T) {
	st := &memStore{listErr: errors.New("db down")}
	svc, _ := New(Options{
		Store:      st,
		Summarizer: SummarizerFunc(func(context.Context, string) (string, error) { return "x", nil }),
	})
	msgs := []model.Message{msg(1, model.RoleUser, "a"), msg(2, model.RoleAssistant, "b")}
	out, err := svc.LoadHistory(context.Background(), msgs)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("应退回完整历史（2 条），得到 %d", len(out))
	}
}

// TestTrimSummaryStripsFence 摘要外层的 markdown 围栏应被清除。
func TestTrimSummaryStripsFence(t *testing.T) {
	cases := []string{
		"```markdown\n# 标题\n内容\n```",
		"```md\n# 标题\n内容\n```",
		"```\n# 标题\n内容\n```",
	}
	for _, c := range cases {
		got := trimSummary(c)
		if strings.Contains(got, "```") {
			t.Errorf("围栏未被清除: %q", got)
		}
		if !strings.Contains(got, "# 标题") {
			t.Errorf("正文被误删: %q", got)
		}
	}
}

// TestTrimSummaryLeavesPlainText 无围栏的摘要原样保留。
func TestTrimSummaryLeavesPlainText(t *testing.T) {
	s := "# 标题\n\n这是普通摘要内容"
	if got := trimSummary(s); got != s {
		t.Errorf("无围栏时不应改动，得到 %q", got)
	}
}
