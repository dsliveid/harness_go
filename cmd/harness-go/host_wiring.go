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
	"harness-go/internal/sop"
	"harness-go/internal/service"
	"harness-go/pkg/toolapi"
)

// ============ SOP 动态注入（阶段 G） ============

// sopBodies 读取项目级 SOP 条目正文列表（<ws>/.harness/sop/*.md）。
//
// 架构变更（重要）：改造前本函数经 host.Workspace() 读目录，而该 root 在
// hostimpl.NewHost 时一次性固化，导致「切到别的项目会话仍读到旧项目的 SOP」
// 的串味问题。现改为接受**显式会话工作区路径**，由 internal/sop 直接解析，
// 内核不再依赖插件包，也不再受 host root 固化影响。
//
// 返回值进 prompt.Input.SOP（系统提示「## 项目规范（SOP）」段，全量注入）；
// 未绑定工作区或项目未配置 SOP 时返回空切片（不注入该段）。
func sopBodies(wsPath string) []string {
	entries := sop.LoadEntries(wsPath)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Body)
	}
	return out
}

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

// ============ 记忆文件 → 检索索引同步 ============

// memoryKindByLayer 记忆层目录 → retrieval.Kind。
//
// ⚠️ 必须与 memory.Layer 的四个层一一对应（L0/L1/L2/L3）。
// 改造前这里只有 core/notes/archive 三项，**缺 L1（项目层）**，
// 于是 memory.Store.Recall 里那段 L1 三行摘要逻辑永远空跑 ——
// 设计文档宣称的"四层记忆"实际只有三层在跑，且没有任何报错。
//
// 现在补齐 project → memory_l1。记忆插件侧的层目录名仍是 core/notes/archive
// （它的 LayerCore/LayerNotes/LayerArchive 是写侧三分类），
// 因此 L1 的来源是**目录可选的 project 子目录**：存在即同步，不存在即跳过
// （不建空目录、不伪造内容 —— 宁可没有，也不能让模型读到假的项目记忆）。
var memoryKindByLayer = map[string]string{
	"core":    "memory_l0",
	"project": "memory_l1",
	"notes":   "memory_l2",
	"archive": "memory_l3",
}

// MemorySyncer 把工作区记忆文件同步进检索索引（文件是真源，索引可重建）。
//
// 抽成独立类型而不是散在装配点，是为了让**写后同步**（memory 插件写入成功后
// 立刻同步这一条）与**启动期全量同步**共用同一段逻辑：
// 两条路径若各写一份，解析规则（标题/正文/重要性）迟早漂移。
type MemorySyncer struct {
	// WorkDir 工作区根（进程级默认根）。
	WorkDir string
	// Store 主库记忆 upsert 入口。
	Store interface {
		UpsertMemoryDoc(ctx context.Context, kind, key, title, body string, importance float64) (bool, error)
	}
	// Log 日志（可为 nil）。
	Log func(msg string, args ...any)
}

func (s *MemorySyncer) log(msg string, args ...any) {
	if s.Log != nil {
		s.Log(msg, args...)
	}
}

// SyncAll 全量同步全部层（启动期调用一次）。
func (s *MemorySyncer) SyncAll(ctx context.Context) {
	for layer := range memoryKindByLayer {
		n := s.SyncLayer(ctx, layer)
		if n > 0 {
			s.log("记忆文件已同步进检索索引", "layer", layer, "changed", n)
		}
	}
}

// SyncLayer 同步单个层目录，返回变更条数。
func (s *MemorySyncer) SyncLayer(ctx context.Context, layer string) int {
	if _, ok := memoryKindByLayer[layer]; !ok {
		return 0
	}
	dir := filepath.Join(s.WorkDir, ".harness", "memory", layer)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0 // 层目录不存在是正常态
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if s.SyncFile(ctx, layer, e.Name()) {
			n++
		}
	}
	return n
}

// SyncFile 同步单个记忆文件（写后同步走这里）。
//
// 只影响**索引可见性**，不触发任何注入 —— 注入快照由 Run 边界冻结，
// 两者是彼此独立的轴（详见 record_memory 的回执文案）。
func (s *MemorySyncer) SyncFile(ctx context.Context, layer, fileName string) bool {
	kind, ok := memoryKindByLayer[layer]
	if !ok {
		return false
	}
	path := filepath.Join(s.WorkDir, ".harness", "memory", layer, fileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	title, body, importance := parseMemoryDoc(string(data))
	key := layer + "/" + fileName
	changed, uerr := s.Store.UpsertMemoryDoc(ctx, kind, key, title, body, importance)
	if uerr != nil {
		s.log("记忆文件同步失败", "file", key, "err", uerr)
		return false
	}
	return changed
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
