package service

import (
	"context"
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

// Test 连通测试：用该厂商配置发一次极小 chat 请求，返回延迟与错误。
//
// 刻意不返回上游响应体（防泄漏配额/账号信息），只回错误码 + 简短原因。
func (s *ProviderService) Test(ctx context.Context, id, modelRef string) (ProviderTestResult, error) {
	p, err := s.main.Providers().Get(ctx, id)
	if err != nil {
		return ProviderTestResult{}, wrapStoreErr(err, apperr.CodeNotFound, "厂商不存在")
	}
	key := ""
	if len(p.APIKeyEnc) > 0 {
		if s.cipher == nil {
			return ProviderTestResult{}, apperr.New(apperr.CodeUnavailable, "服务未启用密钥，无法读取 api_key")
		}
		raw, dErr := s.cipher.Decrypt(p.APIKeyEnc)
		if dErr != nil {
			return ProviderTestResult{}, apperr.Wrap(dErr, apperr.CodeInvalidArgument, "api_key 解密失败")
		}
		key = string(raw)
	}
	name := modelRef
	if name == "" && len(p.Models) > 0 {
		name = p.Models[0]
	}
	if name == "" {
		return ProviderTestResult{Model: "", Code: "missing_model", Error: "厂商未配置可用模型"}, nil
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
