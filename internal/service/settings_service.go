package service

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"

	"harness-go/internal/apperr"
	"harness-go/internal/store"
)

// settingsWhitelist 允许经 API 读写的配置键（白名单，防止任意键污染）。
//
// 说明：`tools.tiers` 为工具分组三层覆盖的全局层（见 D11 修订）。
var settingsWhitelist = map[string]bool{
	"ui.theme":            true,
	"ui.locale":           true,
	"ui.compact":          true,
	"agent.default_model": true,
	"agent.access_mode":   true,
	"agent.max_steps":     true,
	"tools.tiers":         true,
	"storage.quota_warn":  true,
}

// SettingsService 通用键值配置用例（B-4）。
type SettingsService struct {
	main *store.MainStore
}

// NewSettingsService 创建设置用例。
func NewSettingsService(main *store.MainStore) *SettingsService {
	return &SettingsService{main: main}
}

// Get 读取全部白名单配置（未设置的键不返回，由前端用默认值兜底）。
func (s *SettingsService) Get(ctx context.Context) (map[string]json.RawMessage, error) {
	all, err := s.main.Settings().All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(all))
	for k, v := range all {
		if settingsWhitelist[k] {
			out[k] = v
		}
	}
	return out, nil
}

// Patch 批量写入配置（键必须命中白名单，值必须为合法 JSON 片段）。
func (s *SettingsService) Patch(ctx context.Context, patch map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if len(patch) == 0 {
		return nil, invalidArg("patch 不能为空")
	}
	for k, v := range patch {
		if !settingsWhitelist[k] {
			return nil, invalidArg("不支持的配置键: %s", k)
		}
		if len(v) == 0 || !json.Valid(v) {
			return nil, invalidArg("配置键 %s 的值不是合法 JSON", k)
		}
	}
	for k, v := range patch {
		if err := s.main.Settings().Set(ctx, k, v); err != nil {
			return nil, err
		}
	}
	return s.Get(ctx)
}

// LogService 应用日志尾行读取（B-4 统计与日志面板）。
//
// 铁律：只返回尾部 N 行，永不返回全文（避免把整个 app.log 塞进响应）。
type LogService struct {
	path     string
	maxLines int
}

// NewLogService 创建日志用例；path 为空时所有查询返回空列表。
func NewLogService(path string, maxLines int) *LogService {
	if maxLines <= 0 || maxLines > 2000 {
		maxLines = 500
	}
	return &LogService{path: path, maxLines: maxLines}
}

// LogLine 一条日志（原样文本 + 简单级别提取）。
type LogLine struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// Tail 读取日志尾部 limit 行，level 非空时按级别过滤（大小写不敏感）。
func (s *LogService) Tail(limit int, level string) ([]LogLine, error) {
	if s.path == "" {
		return []LogLine{}, nil
	}
	if limit <= 0 || limit > s.maxLines {
		limit = 200
	}
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []LogLine{}, nil
		}
		return nil, apperr.Wrap(err, apperr.CodeInternal, "读取日志文件失败")
	}
	defer func() { _ = f.Close() }()

	// 环形缓冲：常量内存保留最后 limit 行（不把全文读进内存）
	buf := make([]string, limit)
	count, idx := 0, 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		buf[idx] = sc.Text()
		idx = (idx + 1) % limit
		if count < limit {
			count++
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return nil, apperr.Wrap(err, apperr.CodeInternal, "读取日志失败")
	}

	out := make([]LogLine, 0, count)
	for i := 0; i < count; i++ {
		text := buf[(idx+limit-count+i)%limit]
		lv := detectLevel(text)
		if level != "" && !strings.EqualFold(lv, level) {
			continue
		}
		out = append(out, LogLine{Level: lv, Text: text})
	}
	return out, nil
}

func detectLevel(line string) string {
	for _, lv := range []string{"ERROR", "WARN", "INFO", "DEBUG"} {
		if strings.Contains(line, "level="+lv) || strings.Contains(line, lv) {
			return strings.ToLower(lv)
		}
	}
	return "info"
}
