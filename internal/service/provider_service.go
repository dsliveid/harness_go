package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/secret"
	"harness-go/internal/store"
)

// ProviderServiceOptions ProviderService 依赖。
type ProviderServiceOptions struct {
	Main   *store.MainStore
	Cipher *secret.Cipher
}

// ProviderService 模型厂商用例（B-4）：CRUD + 连通测试。
//
// 铁律：api_key 明文**永不出栈**（List/Get 一律清空），落库前用 secret.Cipher 加密。
type ProviderService struct {
	main   *store.MainStore
	cipher *secret.Cipher
}

// NewProviderService 创建厂商用例；cipher 为 nil 时拒绝写入密钥（返回 unavailable）。
func NewProviderService(opts ProviderServiceOptions) *ProviderService {
	return &ProviderService{main: opts.Main, cipher: opts.Cipher}
}

// ProviderTestResult 连通测试结果。
type ProviderTestResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Model     string `json:"model"`
	Error     string `json:"error,omitempty"`
	Code      string `json:"code,omitempty"`
}

// ProviderModelsResult 上游模型清单（「获取模型列表」）。
type ProviderModelsResult struct {
	Items []string `json:"items"`
	Count int      `json:"count"`
}

// ProviderSecretResult 厂商密钥明文（「显示密钥 / 复制密钥」）。
type ProviderSecretResult struct {
	APIKey string `json:"api_key"`
	HasKey bool   `json:"has_key"`
}

// List 列出厂商（密钥脱敏）。
func (s *ProviderService) List(ctx context.Context) ([]model.Provider, error) {
	list, err := s.main.Providers().List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].APIKeyEnc = nil
		list[i].APIKey = ""
	}
	return list, nil
}

// Get 取单个厂商（密钥脱敏）。
func (s *ProviderService) Get(ctx context.Context, id string) (*model.Provider, error) {
	p, err := s.main.Providers().Get(ctx, id)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeNotFound, "厂商不存在")
	}
	p.APIKeyEnc = nil
	p.APIKey = ""
	return p, nil
}

// Create 新增厂商。
func (s *ProviderService) Create(ctx context.Context, in model.Provider) (*model.Provider, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, invalidArg("厂商名称不能为空")
	}
	if strings.TrimSpace(in.BaseURL) == "" {
		return nil, invalidArg("Base URL 不能为空")
	}
	enc, err := s.encrypt(in.APIKey)
	if err != nil {
		return nil, err
	}
	p := &model.Provider{
		ID:        newID(),
		Name:      strings.TrimSpace(in.Name),
		BaseURL:   strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"),
		APIKeyEnc: enc,
		Models:    in.Models,
		Enabled:   in.Enabled,
	}
	if p.Models == nil {
		p.Models = []string{}
	}
	if err := s.main.Providers().Create(ctx, p); err != nil {
		return nil, err
	}
	p.APIKeyEnc = nil
	p.HasAPIKey = len(enc) > 0
	return p, nil
}

// Update 更新厂商；in.APIKey 为空表示保留原密钥。
func (s *ProviderService) Update(ctx context.Context, id string, in model.Provider) (*model.Provider, error) {
	cur, err := s.main.Providers().Get(ctx, id)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeNotFound, "厂商不存在")
	}
	if strings.TrimSpace(in.Name) != "" {
		cur.Name = strings.TrimSpace(in.Name)
	}
	if strings.TrimSpace(in.BaseURL) != "" {
		cur.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	}
	cur.Enabled = in.Enabled
	if in.Models != nil {
		cur.Models = in.Models
	}
	if in.APIKey != "" {
		enc, eErr := s.encrypt(in.APIKey)
		if eErr != nil {
			return nil, eErr
		}
		cur.APIKeyEnc = enc
	}
	if err := s.main.Providers().Update(ctx, cur); err != nil {
		return nil, err
	}
	out := *cur
	out.APIKeyEnc = nil
	out.APIKey = ""
	out.HasAPIKey = len(cur.APIKeyEnc) > 0
	return &out, nil
}

// Delete 删除厂商。
func (s *ProviderService) Delete(ctx context.Context, id string) error {
	return wrapStoreErr(s.main.Providers().Delete(ctx, id), apperr.CodeNotFound, "厂商不存在")
}

// ProviderTestInput 连通测试入参。
//
// 除 Model 外还接受 BaseURL/APIKey/Models 草稿覆盖：前端把「尚未保存」的当前值
// 一并传来，实现所见即所测——
//   - 厂商未落库（草稿 id / 已删除）：只要有 base_url 就按请求配置直接测；
//   - 已落库：非空覆盖字段优先于库内值（api_key 仅本次测试使用，不落库、不进日志）。
type ProviderTestInput struct {
	Model   string   `json:"model"`
	BaseURL string   `json:"base_url"`
	APIKey  string   `json:"api_key"`
	Models  []string `json:"models"`
}

// resolveDraft 解析「本次操作用的厂商配置」：已落库取库值 + 草稿覆盖，未落库凭草稿临时构造。
//
// 这是 Test / ListModels 共用的覆盖语义（所见即所测）：前端把尚未保存的 base_url / api_key
// 一并传来即可测试与拉取模型清单，全程不落库、密钥不进日志。
// 未落库且草稿未给 base_url 时返回 CodeNotFound（此时确实无从发起请求）。
func (s *ProviderService) resolveDraft(ctx context.Context, id, baseURL, apiKey string, models []string, modelsSet bool) (*model.Provider, string, error) {
	p, err := s.main.Providers().Get(ctx, id)
	switch {
	case err == nil:
		// 已落库：草稿里改过但未保存的字段以请求覆盖为准
		if strings.TrimSpace(baseURL) != "" {
			p.BaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
		}
		if modelsSet {
			p.Models = models
		}
	case errors.Is(err, store.ErrNotFound) && strings.TrimSpace(baseURL) != "":
		// 未落库厂商：凭草稿直接请求（所见即所测），不落库
		p = &model.Provider{
			ID:      id,
			Name:    "未保存厂商",
			BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
			Models:  models,
		}
	default:
		return nil, "", wrapStoreErr(err, apperr.CodeNotFound, "厂商不存在")
	}

	key := strings.TrimSpace(apiKey)
	if key == "" {
		if key, err = s.decryptKey(p); err != nil {
			return nil, "", err
		}
	}
	return p, key, nil
}

// Test 连通测试：用该厂商配置发一次极小 chat 请求，返回延迟与错误。
//
// 刻意不返回上游响应体（防泄漏配额/账号信息），只回错误码 + 简短原因。
// 覆盖语义见 ProviderTestInput；测试本身不产生任何落库副作用。
func (s *ProviderService) Test(ctx context.Context, id string, in ProviderTestInput) (ProviderTestResult, error) {
	p, key, err := s.resolveDraft(ctx, id, in.BaseURL, in.APIKey, in.Models, in.Models != nil)
	if err != nil {
		return ProviderTestResult{}, err
	}

	// 模型候选优先级：显式指定 > 草稿/库内首个模型。全空时给可操作提示（前端据此引导先取模型）。
	name := strings.TrimSpace(in.Model)
	if name == "" && len(p.Models) > 0 {
		name = p.Models[0]
	}
	if name == "" {
		return ProviderTestResult{
			Model: "",
			Code:  "missing_model",
			Error: "尚未选择模型：请先点「获取模型列表」或手动添加模型 ID，再测试连接",
		}, nil
	}

	prov, err := llm.NewOpenAICompat(llm.OpenAICompatConfig{
		Name:       p.Name,
		BaseURL:    p.BaseURL,
		APIKey:     key,
		MaxRetries: 0,
	})
	if err != nil {
		return ProviderTestResult{}, apperr.Wrap(err, apperr.CodeInvalidArgument, "厂商配置无效")
	}

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	stream, err := prov.ChatStream(cctx, llm.ChatRequest{
		Model:     name,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "ping"}},
		MaxTokens: 1,
		Stream:    true,
	})
	if err != nil {
		return ProviderTestResult{
			Model:     name,
			Code:      string(llm.AsError(err).Kind),
			Error:     llm.AsError(err).Message,
			LatencyMS: time.Since(start).Milliseconds(),
		}, nil
	}
	defer func() { _ = stream.Close() }()
	// 读到首个分片即算连通（不等待完整响应，避免消耗额度）
	if _, rErr := stream.Recv(); rErr != nil && rErr != llm.EOF {
		return ProviderTestResult{
			Model:     name,
			Code:      string(llm.AsError(rErr).Kind),
			Error:     llm.AsError(rErr).Message,
			LatencyMS: time.Since(start).Milliseconds(),
		}, nil
	}
	return ProviderTestResult{
		OK:        true,
		Model:     name,
		LatencyMS: time.Since(start).Milliseconds(),
	}, nil
}

// ProviderModelsInput 「获取模型列表」的草稿覆盖入参。
//
// 语义与 ProviderTestInput 完全一致：未保存的厂商也能拉清单，base_url/api_key 仅本次使用不落库。
type ProviderModelsInput struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// ListModels 拉取该厂商在上游的真实模型清单（「获取模型列表」）。
//
// 与 Test 一样需要密钥，且同样支持草稿覆盖（见 resolveDraft）：厂商尚未保存时
// 前端传 base_url/api_key 即可直接拉取，不落库。
// 上游全部候选失败时**原样上抛可读错误**（不返回任何本地兜底清单），
// 让前端明确告知用户「未获取到模型 + 原因」，避免假成功。
func (s *ProviderService) ListModels(ctx context.Context, id string, in ProviderModelsInput) (ProviderModelsResult, error) {
	p, key, err := s.resolveDraft(ctx, id, in.BaseURL, in.APIKey, nil, false)
	if err != nil {
		return ProviderModelsResult{}, err
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return ProviderModelsResult{}, invalidArg("请先填写 Base URL（厂商地址），再获取模型列表")
	}
	prov, nErr := llm.NewOpenAICompat(llm.OpenAICompatConfig{
		Name:       p.Name,
		BaseURL:    p.BaseURL,
		APIKey:     key,
		MaxRetries: 0,
	})
	if nErr != nil {
		return ProviderModelsResult{}, apperr.Wrap(nErr, apperr.CodeInvalidArgument, "厂商配置无效")
	}
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	list, lErr := prov.ListModels(cctx)
	if lErr != nil {
		le := llm.AsError(lErr)
		return ProviderModelsResult{}, apperr.New(apperr.CodeUnavailable, "获取模型列表失败：%s", le.Message)
	}
	return ProviderModelsResult{Items: list, Count: len(list)}, nil
}

// RevealAPIKey 读取厂商密钥明文（供「设置 → 模型」的眼睛图标显示与复制）。
//
// 安全边界（用户确认放开部署形态限制，不再区分 local/server/cloud）：
//   - 仍要求已登录（Bearer/Cookie 由全局鉴权中间件把关），且只返回**该厂商自己**的密钥；
//   - 响应头由 handler 加 `Cache-Control: no-store`，明文不落 access log；
//   - 未配置密钥返回空串 + has_key=false，不视为错误（前端据 has_key 置灰复制按钮）。
func (s *ProviderService) RevealAPIKey(ctx context.Context, id string) (ProviderSecretResult, error) {
	p, err := s.main.Providers().Get(ctx, id)
	if err != nil {
		return ProviderSecretResult{}, wrapStoreErr(err, apperr.CodeNotFound, "厂商不存在")
	}
	key, kErr := s.decryptKey(p)
	if kErr != nil {
		return ProviderSecretResult{}, kErr
	}
	return ProviderSecretResult{APIKey: key, HasKey: key != ""}, nil
}

// decryptKey 解密厂商密钥；未配置返回空串，cipher 缺失时给出明确原因。
func (s *ProviderService) decryptKey(p *model.Provider) (string, error) {
	if len(p.APIKeyEnc) == 0 {
		return "", nil
	}
	if s.cipher == nil {
		return "", apperr.New(apperr.CodeUnavailable, "服务未启用密钥，无法读取 api_key")
	}
	raw, err := s.cipher.Decrypt(p.APIKeyEnc)
	if err != nil {
		return "", apperr.Wrap(err, apperr.CodeInvalidArgument, "api_key 解密失败")
	}
	return string(raw), nil
}

func (s *ProviderService) encrypt(apiKey string) ([]byte, error) {
	if apiKey == "" {
		return nil, nil
	}
	if s.cipher == nil {
		return nil, apperr.New(apperr.CodeUnavailable, "服务未启用密钥，无法保存 api_key")
	}
	enc, err := s.cipher.Encrypt([]byte(apiKey))
	if err != nil {
		return nil, fmt.Errorf("加密 api_key 失败: %w", err)
	}
	return enc, nil
}
