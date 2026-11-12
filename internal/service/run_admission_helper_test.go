package service

import (
	"testing"
	"time"

	"harness-go/internal/agent"
	"harness-go/internal/approval"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/prompt"
	"harness-go/internal/store"
	"harness-go/internal/tokenizer"
)

// newRunAdmissionEngine 构造 Run 准入测试用引擎（fake provider，无工具）。
//
// 与 child_runner_test 的装配保持一致，便于两处测试共享同一套最小依赖口径。
func newRunAdmissionEngine(
	t *testing.T,
	main *store.MainStore,
	sessions *store.SessionStore,
	bus *events.Bus,
	provider *llm.FakeProvider,
) (*agent.Engine, error) {
	t.Helper()
	return agent.New(agent.Deps{
		Sessions: NewSessionProvider(sessions),
		Summary:  NewSummaryWriter(main),
		Events:   bus,
		Models:   childTestResolver{provider},
		Builder:  prompt.NewBuilder(tokenizer.NewHeuristic(), prompt.DefaultLimits()),
		Gate:     approval.NewGate(time.Second),
		MaxSteps: 3,
	})
}
