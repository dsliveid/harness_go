package service

import (
	"context"
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

// Resolve 实现 agent.Resolver：返回 (provider, 命中的适配器名, error)。
func (r *ProviderResolver) Resolve(ctx context.Context, modelRef string) (llm.Provider, string, error) {
	ref := strings.TrimSpace(modelRef)
	if ref == "" || ref == "default" {
		return r.useFallback("")
	}
	if r.main == nil {
		return r.useFallback("main store 未初始化")
	}

	list, err := r.main.Providers().List(ctx)
	if err != nil {
		// 读库失败不应打断对话：记录后回退
		if r.logger != nil {
			r.logger.Warn("读取 providers 表失败，回退默认 provider", "err", err)
		}
		return r.useFallback("providers 读取失败")
	}

	enabled := make([]model.Provider, 0, len(list))
	for _, p := range list {
		if p.Enabled {
			enabled = append(enabled, p)
		}
	}
	if len(enabled) == 0 {
		return r.useFallback("未配置任何启用的厂商")
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

// build 构造（或复用缓存的）OpenAI 兼容 provider。
func (r *ProviderResolver) build(p model.Provider, modelID string) (llm.Provider, string, error) {
	key := fmt.Sprintf("%s@%d@%s", p.ID, p.UpdatedAt, modelID)
	r.mu.Lock()
	cached, ok := r.cache[key]
	r.mu.Unlock()
	if ok && cached != nil {
		return cached, p.Name + "/" + modelID, nil
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
	return prov, p.Name + "/" + modelID, nil
}

// useFallback 回退到环境变量/fake provider。
func (r *ProviderResolver) useFallback(reason string) (llm.Provider, string, error) {
	if r.fallback == nil {
		if reason == "" {
			return nil, "", errors.New("未配置模型 provider")
		}
		return nil, "", errors.New("未配置模型 provider：" + reason)
	}
	if reason != "" && r.logger != nil {
		r.logger.Debug("模型解析回退默认 provider", "reason", reason)
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
