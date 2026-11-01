package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/secret"
	"harness-go/internal/store"
)

// ProviderResolver 按 model_ref 解析运行时模型厂商（B 阶段收口，替代 A 阶段的 staticResolver）。
//
// 背景：B-4 已支持在「设置 → 模型」里配置厂商（主库 providers 表，api_key 密文列）。
// 若运行时不查这张表，配置就只是"入库 + 连通测试"，真实对话仍走环境变量/fake provider，
// 属于典型的"界面能用、功能不生效"。此解析器把 providers 表接到 agent 的 Resolver 端口上。
//
// 解析顺序（宽进严出，永不因解析失败直接崩掉对话）：
//  0. `model_ref` 为空或 `"default"` → 「默认模型」语义（对齐桌面端 resolve_active_model）：
//     先读 `agent.default_model`（设置 → 能力模型槽位 chat），须命中启用厂商且模型在列表内；
//     失效/未设置 → 回落第一个启用厂商的第一个模型；仍无 → 回退 fallback（env / fake）。
//  1. `model_ref` 形如 `<providerID>/<modelID>` → 精确匹配该厂商（enabled 才可用）；
//  2. 否则把 `model_ref` 当作模型名，在全部 enabled 厂商的 models 里找第一个命中；
//  3. 仍未命中但**只有一家** enabled 厂商 → 用该厂商 + `model_ref` 作为模型名（只配 base_url 的最小场景）；
//  4. 以上都不成立（或未配置任何厂商）→ 回退 fallback（env 配置 / fake provider），保证开箱可用。
//
// 缓存：以 `providerID@updatedAt` 为键，避免每轮都解密 api_key 与重建 HTTP 客户端；
// 厂商被改（updatedAt 变化）即自动失效，不需要显式失效通知。
type ProviderResolver struct {
	main         *store.MainStore
	cipher       *secret.Cipher
	logger       *slog.Logger
	fallback     llm.Provider
	fallbackName string

	mu    sync.Mutex
	cache map[string]llm.Provider
}

// NewProviderResolver 构造解析器。
//
// cipher 为 nil 表示主库未启用加密（理论上不会，启动即校验）；fallback 为 nil 时
// 未命中会返回错误（调用方会以明确的 5xx/错误码暴露，而不是静默失败）。
func NewProviderResolver(
	main *store.MainStore,
	cipher *secret.Cipher,
	logger *slog.Logger,
	fallback llm.Provider,
	fallbackName string,
) *ProviderResolver {
	return &ProviderResolver{
		main:         main,
		cipher:       cipher,
		logger:       logger,
		fallback:     fallback,
		fallbackName: fallbackName,
		cache:        make(map[string]llm.Provider),
	}
}

// Resolve 实现 agent.Resolver：返回 (provider, 解析出的模型名, error)。
//
// 第二个返回值是**裸模型名**（OpenAI 兼容协议里 model 字段的原样取值），不是
// `厂商名/模型名` 展示串——引擎会把它直接发上游（engine.go Request.Model）；
// 发错形态（如 opencode/deepseek-v4.1-flash）会被上游按未知模型拒绝。
func (r *ProviderResolver) Resolve(ctx context.Context, modelRef string) (llm.Provider, string, error) {
	ref := strings.TrimSpace(modelRef)
	// 空 / "default" = 默认模型语义：agent.default_model → 首个启用厂商的首个模型。
	// 不再把 "default" 直接短路到 fallback——否则配置了厂商也不会被对话用到（B-4 断链）。
	if ref == "" || ref == "default" {
		if p, mid, ok := r.resolveDefault(ctx); ok {
			return r.build(p, mid)
		}
		return r.useFallback("默认模型不可用：未设置 agent.default_model 且没有启用厂商")
	}
	if r.main == nil {
		return r.useFallback("main store 未初始化")
	}

	enabled, failReason := r.enabledProviders(ctx)
	if len(enabled) == 0 {
		return r.useFallback(failReason)
	}

	// ① `<providerID>/<modelID>`
	if pid, mid, ok := splitModelRef(ref); ok {
		for _, p := range enabled {
			if p.ID == pid {
				return r.build(p, mid)
			}
		}
	}

	// ② 以模型名在全部启用厂商里找
	for _, p := range enabled {
		for _, m := range p.Models {
			if m == ref {
				return r.build(p, m)
			}
		}
	}

	// ③ 只有一家启用厂商：把它当作默认端点，ref 直接当模型名
	if len(enabled) == 1 {
		return r.build(enabled[0], ref)
	}

	return r.useFallback("model_ref 未命中任何厂商：" + ref)
}

// enabledProviders 拉取启用厂商列表；读库失败/为空时返回原因（供回退日志用，不打断对话）。
func (r *ProviderResolver) enabledProviders(ctx context.Context) ([]model.Provider, string) {
	list, err := r.main.Providers().List(ctx)
	if err != nil {
		// 读库失败不应打断对话：记录后回退
		if r.logger != nil {
			r.logger.Warn("读取 providers 表失败，回退默认 provider", "err", err)
		}
		return nil, "providers 读取失败"
	}
	enabled := make([]model.Provider, 0, len(list))
	for _, p := range list {
		if p.Enabled {
			enabled = append(enabled, p)
		}
	}
	if len(enabled) == 0 {
		return nil, "未配置任何启用的厂商"
	}
	return enabled, ""
}

// resolveDefault 解析「默认模型」（model_ref 为空/"default" 时的语义，对齐桌面端
// resolve_active_model 的「激活项缺失/失效回落第一个有模型的厂商」）：
//  1. `agent.default_model`（设置 → 能力模型槽位 chat，前端已落库）——须命中启用厂商
//     且模型在列表内；厂商被删/停用、模型下架都视为未设置，防止拿失效引用打上游；
//  2. 回落第一个启用厂商的第一个模型。
//
// 返回 ok=false 表示无任何可用默认模型（调用方回退 fallback）。
func (r *ProviderResolver) resolveDefault(ctx context.Context) (model.Provider, string, bool) {
	if r.main == nil {
		return model.Provider{}, "", false
	}
	enabled, _ := r.enabledProviders(ctx)
	if len(enabled) == 0 {
		return model.Provider{}, "", false
	}
	if raw, err := r.main.Settings().Get(ctx, settingsKeyDefaultModel); err == nil && len(raw) > 0 {
		var ref string
		if json.Unmarshal(raw, &ref) == nil {
			ref = strings.TrimSpace(ref)
			if ref != "" {
				if pid, mid, ok := splitModelRef(ref); ok {
					for _, p := range enabled {
						if p.ID == pid && modelIn(p, mid) {
							return p, mid, true
						}
					}
				}
				for _, p := range enabled {
					for _, m := range p.Models {
						if m == ref {
							return p, m, true
						}
					}
				}
				if r.logger != nil {
					r.logger.Warn("agent.default_model 未命中启用厂商，改用第一个启用厂商的模型", "ref", ref)
				}
			}
		} else if r.logger != nil {
			r.logger.Warn("agent.default_model 不是合法字符串，忽略", "raw", string(raw))
		}
	}
	for _, p := range enabled {
		if len(p.Models) > 0 {
			return p, p.Models[0], true
		}
	}
	return model.Provider{}, "", false
}

// modelIn 判断模型是否在厂商模型列表内。
func modelIn(p model.Provider, mid string) bool {
	for _, m := range p.Models {
		if m == mid {
			return true
		}
	}
	return false
}

// build 构造（或复用缓存的）OpenAI 兼容 provider；第二个返回值为裸模型名（见 Resolve）。
func (r *ProviderResolver) build(p model.Provider, modelID string) (llm.Provider, string, error) {
	key := fmt.Sprintf("%s@%d@%s", p.ID, p.UpdatedAt, modelID)
	r.mu.Lock()
	cached, ok := r.cache[key]
	r.mu.Unlock()
	if ok && cached != nil {
		return cached, modelID, nil
	}

	apiKey := ""
	if len(p.APIKeyEnc) > 0 && r.cipher != nil {
		plain, err := r.cipher.Decrypt(p.APIKeyEnc)
		if err != nil {
			// 密钥解密失败（secret.key 被替换）→ 明确告警并回退，不把坏配置塞进对话
			if r.logger != nil {
				r.logger.Error("厂商 api_key 解密失败，回退默认 provider", "provider", p.ID, "err", err)
			}
			return r.useFallback("api_key 解密失败")
		}
		apiKey = string(plain)
	}

	prov, err := llm.NewOpenAICompat(llm.OpenAICompatConfig{
		Name:    p.Name,
		BaseURL: p.BaseURL,
		APIKey:  apiKey,
	})
	if err != nil {
		if r.logger != nil {
			r.logger.Error("构造厂商 provider 失败，回退默认 provider", "provider", p.ID, "err", err)
		}
		return r.useFallback("厂商端点配置无效")
	}

	r.mu.Lock()
	r.cache[key] = prov
	r.mu.Unlock()
	if r.logger != nil {
		// 只打印模型与端点，绝不打印密钥
		r.logger.Info("模型已按 providers 表解析", "provider", p.Name, "model", modelID, "base_url", p.BaseURL)
	}
	return prov, modelID, nil
}

// useFallback 回退到环境变量/fake provider。
//
// 回退到 fake（本地回显，不调用大模型）必须显眼：以 Warn 落日志，杜绝
// 「界面能用、功能不生效」的静默降级；回退到 env 真实端点则 Info 即可。
func (r *ProviderResolver) useFallback(reason string) (llm.Provider, string, error) {
	if r.fallback == nil {
		if reason == "" {
			return nil, "", errors.New("未配置模型 provider")
		}
		return nil, "", errors.New("未配置模型 provider：" + reason)
	}
	if reason != "" && r.logger != nil {
		if strings.Contains(r.fallbackName, "fake") {
			r.logger.Warn("对话将使用 fake 回显 provider（不调用大模型），请到「设置 → 模型」配置厂商并选择模型",
				"reason", reason)
		} else {
			r.logger.Info("模型解析回退默认 provider", "reason", reason, "provider", r.fallbackName)
		}
	}
	return r.fallback, r.fallbackName, nil
}

// splitModelRef 拆分 `<providerID>/<modelID>`；模型名本身可能含 `/`（如 openai/gpt-4o-mini），
// 因此只在**第一段不是已知模型前缀**时才按前缀拆分——这里采用保守策略：
// 仅当第一段形如厂商 ID（pv_ / 纯十六进制 / 无点号）时才拆分，否则整体当作模型名。
func splitModelRef(ref string) (pid, mid string, ok bool) {
	i := strings.Index(ref, "/")
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	head := ref[:i]
	if strings.HasPrefix(head, "pv_") || isHexID(head) {
		return head, ref[i+1:], true
	}
	return "", "", false
}

// isHexID 判断是否为十六进制 ID（厂商 ID 由主库生成的 32 位十六进制串）。
func isHexID(s string) bool {
	if len(s) < 16 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
