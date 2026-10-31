package main

// Host 受控能力的内核接线（阶段 F）：把 llm.Provider / events.Bus /
// 命令执行器 / 嵌套 Run 适配为 toolapi 的端口形态。
//
// 这些适配器是「装配点胶水」——插件只认 toolapi 接口，内核实现经此转换注入。

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/service"
	"harness-go/pkg/toolapi"
)

// ============ LLM 适配：llm.Provider(ChatStream) → toolapi.LLM(Chat) ============

// llmAdapter 把流式 provider 包装为插件用的非流式 Chat。
// 模型解析复用 ProviderResolver（providers 表优先，回退默认 provider）。
type llmAdapter struct {
	resolver *service.ProviderResolver
}

// Chat 实现 toolapi.LLM。
func (a *llmAdapter) Chat(ctx context.Context, req toolapi.LLMRequest) (toolapi.LLMResponse, error) {
	provider, modelName, err := a.resolver.Resolve(ctx, req.ModelRef)
	if err != nil {
		return toolapi.LLMResponse{}, err
	}
	msgs := make([]llm.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, llm.Message{Role: llm.Role(m.Role), Content: m.Content})
	}
	stream, err := provider.ChatStream(ctx, llm.ChatRequest{
		Model:       modelName,
		System:      req.System,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      true,
	})
	if err != nil {
		return toolapi.LLMResponse{}, err
	}
	defer func() { _ = stream.Close() }()
	var content strings.Builder
	var usage toolapi.TokenUsage
	for {
		ch, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return toolapi.LLMResponse{}, rerr
		}
		content.WriteString(ch.Delta)
		if ch.Usage != nil {
			usage.PromptTokens += ch.Usage.PromptTokens
			usage.CompletionTokens += ch.Usage.CompletionTokens
			usage.TotalTokens += ch.Usage.TotalTokens
		}
	}
	return toolapi.LLMResponse{Content: content.String(), Usage: usage}, nil
}

// ============ Events 适配：events.Bus → toolapi.Events（类型白名单） ============

// eventsAdapter 插件可发布的事件类型白名单（docs/protocols.md §2 之外的类型拒绝——
// 插件不能伪造 run.status / approval 等内核事件）。
var pluginEventTypes = map[string]events.Type{
	"tool.progress": events.TypeToolProgress,
}

type eventsAdapter struct{ bus *events.Bus }

// Publish 实现 toolapi.Events。
func (a *eventsAdapter) Publish(_ context.Context, runID, typ string, payload any) error {
	t, ok := pluginEventTypes[typ]
	if !ok {
		return errors.New("host: 插件不允许发布事件类型 " + typ)
	}
	_, err := a.bus.Publish(runID, t, payload)
	return err
}

// ============ 命令执行器环境白名单 ============

// commandEnvWhitelist 子进程继承的宿主环境变量（最小集；请求 Env 可再叠加）。
var commandEnvWhitelist = []string{
	"PATH", "PATHEXT", "HOME", "USERPROFILE", "TEMP", "TMP",
	"SYSTEMROOT", "COMSPEC", "WINDIR", "APPDATA", "LOCALAPPDATA", "PROGRAMFILES",
}

// ============ 记忆文件 → 检索索引同步（启动期） ============

// memoryKindByLayer 记忆层目录 → retrieval.Kind。
var memoryKindByLayer = map[string]string{
	"core":    "memory_l0",
	"notes":   "memory_l2",
	"archive": "memory_l3",
}

// syncMemoryFiles 把工作区记忆文件（memory 插件写侧产物）同步进主库检索索引。
//
// 文件是唯一真源、索引可重建（工程避坑清单同款口径）；UpsertDoc 幂等，
// 同内容只刷新 last_ref_at，不重写 FTS5。
func syncMemoryFiles(ctx context.Context, workDir string, mainStore interface {
	UpsertMemoryDoc(ctx context.Context, kind, key, title, body string, importance float64) (bool, error)
}, log func(msg string, args ...any)) {
	base := filepath.Join(workDir, ".harness", "memory")
	for layer, kind := range memoryKindByLayer {
		dir := filepath.Join(base, layer)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // 层目录不存在是正常态
		}
		n := 0
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				continue
			}
			title, body, importance := parseMemoryDoc(string(data))
			key := layer + "/" + e.Name()
			changed, uerr := mainStore.UpsertMemoryDoc(ctx, kind, key, title, body, importance)
			if uerr != nil {
				log("记忆文件同步失败", "file", key, "err", uerr)
				continue
			}
			if changed {
				n++
			}
		}
		if n > 0 {
			log("记忆文件已同步进检索索引", "layer", layer, "changed", n)
		}
	}
}

// parseMemoryDoc 拆标题/正文/重要性（memory 插件的落盘格式）。
func parseMemoryDoc(doc string) (title, body string, importance float64) {
	importance = 0.5
	lines := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "<!-- importance:") && strings.HasSuffix(t, "-->") {
			if v, perr := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(t, "<!-- importance:"), "-->"), 64); perr == nil {
				importance = v
			}
			continue
		}
		if i == 0 && strings.HasPrefix(t, "# ") {
			title = strings.TrimSpace(t[2:])
			continue
		}
		if t == "" && body == "" && i <= 2 {
			continue
		}
		body += ln + "\n"
	}
	if title == "" {
		title = firstNonEmptyLine(doc)
	}
	return title, strings.TrimSpace(body), importance
}

func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return s
}
