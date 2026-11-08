package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/config"
	"harness-go/internal/store"
)

// settingsKeyDefaultModel 全局默认对话模型（agent.default_model）。
//
// 前端「设置 → 能力模型槽位 chat」落库为 `<providerID>/<modelID>`，
// 运行时由 ProviderResolver 在 model_ref 为空/"default" 时消费（见 provider_resolver.go）。
const settingsKeyDefaultModel = "agent.default_model"

// 报文抓取日志的三个可写配置键（设置页「日志与诊断」面板）。
// 与 config.Log.SnapshotMode / config.Log.RetentionDays / config.Log.Level 同义，
// 仅作为 settings 表的 key 名。
const (
	settingsKeyLogSnapshotMode  = "log.snapshot_mode"
	settingsKeyLogRetentionDays = "log.retention_days"
	// settingsKeyLogLevel 运行期日志级别（debug|info|warn|error）。
	// 写入后经 slog.LevelVar 立即生效，控制台与 data/app.log 同步变化。
	settingsKeyLogLevel = "log.level"
)

// settingsWhitelist 允许经 API 读写的配置键（白名单，防止任意键污染）。
//
// 说明：`tools.tiers` 为工具分组三层覆盖的全局层（见 D11 修订）。
var settingsWhitelist = map[string]bool{
	"ui.theme":              true,
	"ui.locale":             true,
	"ui.compact":            true,
	settingsKeyDefaultModel: true,
	"agent.access_mode":     true,
	"agent.max_steps":       true,
	"tools.tiers":           true,
	"storage.quota_warn":    true,
	// 报文抓取档位与保留天数：白名单不放开的话 Patch 会直接拒写，
	// 界面上就会表现为「改了档位但保存无反应」。
	settingsKeyLogSnapshotMode:  true,
	settingsKeyLogRetentionDays: true,
	settingsKeyLogLevel:         true,
}

// settingsValidators 值域校验表（仅校验业务取值，JSON 合法性由 Patch 统一把关）。
//
// 为什么需要额外一层：白名单只回答「这个键能不能写」，不回答「能写成什么」。
// 抓取档位一旦被写成 "preview " / "FULL" 之外的脏值，写入路径会静默回退 preview，
// 用户以为开了 full 却永远拿不到正文——这种静默失效极难排查，故在入口直接 400。
var settingsValidators = map[string]func(json.RawMessage) error{
	settingsKeyLogSnapshotMode: func(v json.RawMessage) error {
		var mode string
		if err := json.Unmarshal(v, &mode); err != nil {
			return invalidArg("%s 必须是字符串（off|preview|full）", settingsKeyLogSnapshotMode)
		}
		if !config.ValidSnapshotMode(mode) {
			return invalidArg("%s 取值非法: %q（合法值: off|preview|full）", settingsKeyLogSnapshotMode, mode)
		}
		return nil
	},
	settingsKeyLogLevel: func(v json.RawMessage) error {
		var level string
		if err := json.Unmarshal(v, &level); err != nil {
			return invalidArg("%s 必须是字符串（debug|info|warn|error）", settingsKeyLogLevel)
		}
		if !config.ValidLogLevel(level) {
			return invalidArg("%s 取值非法: %q（合法值: debug|info|warn|error）", settingsKeyLogLevel, level)
		}
		return nil
	},
	settingsKeyLogRetentionDays: func(v json.RawMessage) error {
		// 用 float64 先接，避免 30.5 这类值被 json 静默取整；非整数一律拒绝。
		var num float64
		if err := json.Unmarshal(v, &num); err != nil {
			return invalidArg("%s 必须是整数天数", settingsKeyLogRetentionDays)
		}
		days := int(num)
		if float64(days) != num {
			return invalidArg("%s 必须是整数天数", settingsKeyLogRetentionDays)
		}
		if days < 1 || days > 365 {
			return invalidArg("%s 超出合法区间 1~365 天: %d", settingsKeyLogRetentionDays, days)
		}
		return nil
	},
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
		if verify, ok := settingsValidators[k]; ok {
			if err := verify(v); err != nil {
				return nil, err
			}
		}
	}
	for k, v := range patch {
		if err := s.main.Settings().Set(ctx, k, v); err != nil {
			return nil, err
		}
	}
	return s.Get(ctx)
}

// 日志倒序读取的分块大小。64KB 是「单次 read 的 IO 成本」与「跨块残片内存」的平衡点：
// 太小则 syscall 次数暴涨，太大则内存浪费且首屏延迟变高。
const logReadChunk = 64 * 1024

// logMaxRotation 轮转文件扫描上限，与 observ.RotationWriter 默认保留 5 份
// （app.log.1 .. app.log.5，序号越大越旧）对齐。
const logMaxRotation = 5

// 查询分页默认值与硬上限。
const (
	logQueryDefaultLimit = 200
	logQueryMaxLimit     = 2000
	// logQueryMaxKeyword 关键字长度上限：防止把整段报文当关键字做全量子串比对。
	logQueryMaxKeyword = 128
)

// LogQuery 文本运维日志查询条件（对齐桌面端 logs 面板的可筛选维度）。
type LogQuery struct {
	Limit   int    // 分页大小，默认 200，上限 2000（且不超过服务构造时的 maxLines）
	Offset  int    // 跳过多少条「命中行」，默认 0
	Level   string // error|warn|info|debug，空 = 不过滤
	TraceID string // 按 trace_id 子串匹配（中间件把 trace_id 写在同一行）
	Keyword string // 整行关键字（大小写不敏感子串，超 128 字符截断）
	From    int64  // Unix 毫秒，0 = 不限
	To      int64  // Unix 毫秒，0 = 不限
}

// LogQueryResult 一次日志查询结果。
type LogQueryResult struct {
	Items []LogLine `json:"items"`
	// Total 扫描过程中命中的行数。注意：命中的行数达到 offset+limit 后我们会
	// 提前停止读取（这正是倒序早停的收益），此时 Total 是**下界**而非全库精确总数。
	// 前端要展示「共 N 条」时应以 HasMore 判断是否「更多」，不要依赖 Total 做分页总数。
	Total   int  `json:"total"`
	HasMore bool `json:"has_more"`
}

// LogService 应用日志读取（B-4 统计与日志面板）。
//
// 铁律：只返回尾部 N 行（或分页后的命中行），永不返回全文
// （避免把整个 app.log 塞进响应，也避免一次请求吃掉几百 MB 内存）。
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
//
// 保留此签名是为了兼容既有调用方（provider_handler.go 的旧 /logs）。
// 内部改为走 Query 的倒序早停实现：不再从头扫到 EOF，读够即停。
func (s *LogService) Tail(limit int, level string) ([]LogLine, error) {
	res, err := s.Query(context.Background(), LogQuery{Limit: limit, Level: level})
	if err != nil {
		return nil, err
	}
	return res.Items, nil
}

// Query 倒序扫描日志文件族，返回符合条件的最新一页（从新到旧）。
//
// 为什么倒序：运维面板 99% 的诉求是「看最近 N 条」。若像旧 Tail 那样从头扫到 EOF，
// 成本是 O(文件大小)——32MB 轮转文件 + 5 份备份，单次请求就是 190MB 的顺序读，
// 且每来一次筛选条件变化就重读一遍。倒序读取把成本降到 O(实际命中所需字节数)：
// 命中行数达到 offset+limit 立即停止，最坏情况才退化到 O(文件大小)。
//
// 文件范围：app.log（当前，最新）→ app.log.1 → ... → app.log.5（最旧）。
// 因为日志会按大小轮转，只读 app.log 会在轮转后「丢掉」最近的历史，必须整族倒序。
func (s *LogService) Query(ctx context.Context, q LogQuery) (LogQueryResult, error) {
	// 空路径 = 日志未接入（测试或未启用文件日志），返回空结果而非报错。
	if s.path == "" {
		return LogQueryResult{Items: []LogLine{}}, nil
	}

	limit := q.Limit
	if limit <= 0 {
		limit = logQueryDefaultLimit
	}
	if limit > logQueryMaxLimit {
		limit = logQueryMaxLimit
	}
	// 服务构造时的 maxLines 是运维配置的硬顶，取两者较小值。
	if s.maxLines > 0 && limit > s.maxLines {
		limit = s.maxLines
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	keyword := q.Keyword
	if len(keyword) > logQueryMaxKeyword {
		keyword = keyword[:logQueryMaxKeyword]
	}

	need := limit + offset // 命中这么多行就够翻这一页了
	collected := make([]LogLine, 0, min(need, 256))
	scanned := 0 // 已处理的行数（含未命中）
	// 关键字比较逐行做 strings.ToLower，提前把条件小写化，避免每行重复转换。
	kwLower := strings.ToLower(keyword)
	files := s.logFilesNewestFirst()
	hasMore := false

	for idx, path := range files {
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // 轮转备份可能不存在，跳过继续往旧文件走
			}
			return LogQueryResult{}, apperr.Wrap(err, apperr.CodeInternal, "读取日志文件失败")
		}
		stopped := false // readLinesReverse 是否因凑够本页而中途停止
		consumedAll, err := readLinesReverse(ctx, f, func(text string) bool {
			scanned++
			if !logLineMatches(text, q, kwLower) {
				return true // 本行不命中，继续往前读
			}
			collected = append(collected, LogLine{Level: parseLevel(text), Text: text})
			if len(collected) >= need {
				stopped = true
				return false // 凑够即停，不再往前读
			}
			return true
		})
		_ = f.Close()
		if err != nil {
			return LogQueryResult{}, err
		}
		if len(collected) >= need {
			if stopped {
				// 本文件未读尽 → 一定还有更早的命中行。
				hasMore = !consumedAll
			}
			if !hasMore {
				// 本文件已读尽：下一页存不存在取决于更旧的轮转文件是否还有内容。
				for _, older := range files[idx+1:] {
					if st, statErr := os.Stat(older); statErr == nil && st.Size() > 0 {
						hasMore = true
						break
					}
				}
			}
			break // 已够本页，绝不继续打开更旧的文件
		}
	}

	// collected 是从新到旧的顺序，与本服务对外语义一致（第一页 = 最新）。
	items := collected
	if offset > 0 {
		if offset >= len(items) {
			items = []LogLine{}
		} else {
			items = items[offset:]
		}
	}
	return LogQueryResult{
		Items: items,
		Total: scanned,
		// HasMore 表示「还有比本页更早的命中行」，用于前端判断能否继续翻页。
		HasMore: hasMore,
	}, nil
}

// logFilesNewestFirst 按「新 → 旧」返回参与查询的日志文件绝对路径。
func (s *LogService) logFilesNewestFirst() []string {
	out := make([]string, 0, logMaxRotation+1)
	out = append(out, s.path)
	for i := 1; i <= logMaxRotation; i++ {
		out = append(out, fmt.Sprintf("%s.%d", s.path, i))
	}
	return out
}

// logLineMatches 判断一行是否命中全部条件（各条件逻辑与，空条件视为不过滤）。
//
// keywordLower 由调用方预先小写化（逐行重复 ToLower 是纯浪费）。
func logLineMatches(line string, q LogQuery, keywordLower string) bool {
	if keywordLower != "" && !strings.Contains(strings.ToLower(line), keywordLower) {
		return false
	}
	if q.TraceID != "" && !strings.Contains(line, q.TraceID) {
		return false
	}
	if q.Level != "" && !strings.EqualFold(parseLevel(line), q.Level) {
		return false
	}
	if q.From > 0 || q.To > 0 {
		ts, ok := parseLogTime(line)
		// 取舍：设置时间范围时，解析不出时间的行一律排除（宁可漏，不可错）。
		// 理由：把无法定位时间戳的行算进区间，比漏掉它们更容易误导排障；
		// 且非标准格式行（如 panic 多行栈）无法被可靠归位。
		if !ok {
			return false
		}
		if q.From > 0 && ts < q.From {
			return false
		}
		if q.To > 0 && ts > q.To {
			return false
		}
	}
	return true
}

// readLinesReverse 从文件末尾往前按块读取，逐行回调 visit（由新到旧）。
//
// 实现要点：块读会在行中间切开，切点处的前半段是「残片」，必须留到下一块
// （更靠前的那块）拼上前缀后才是一整行；因此每轮处理的都是「残片 + 本块内容」
// 中第一个 \n 之后的部分，最前端的部分继续当残片。
//
// visit 返回 false 时立即早停，不再往前读。第二个返回值 consumedAll 表示
// **文件内容是否已被完整消费**——调用方靠它区分两种 earlyStop：
//   - consumedAll=false：文件里还有更早的行没读，说明一定有下一页；
//   - consumedAll=true：恰好读到文件第一行就凑够了，下一页的有无取决于更旧的轮转文件。
//
// 缺了这个标志，HasMore 只能在「凑够」时一律报 true，导致最后一页仍显示「下一页」。
func readLinesReverse(ctx context.Context, f *os.File, visit func(line string) bool) (consumedAll bool, err error) {
	st, err := f.Stat()
	if err != nil {
		return false, apperr.Wrap(err, apperr.CodeInternal, "读取日志文件状态失败")
	}
	size := st.Size()
	if size == 0 {
		return true, nil
	}

	var (
		pos   = size
		carry []byte // 尚未拼全的行尾残片
	)
	buf := make([]byte, logReadChunk)

	for pos > 0 {
		if err := ctx.Err(); err != nil {
			return false, apperr.Wrap(err, apperr.CodeInternal, "日志查询被取消")
		}
		readSize := int64(logReadChunk)
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		chunk := buf[:readSize]
		if _, err := f.ReadAt(chunk, pos); err != nil && err != io.EOF {
			return false, apperr.Wrap(err, apperr.CodeInternal, "读取日志块失败")
		}

		// data = 本块 + 上一轮遗留残片，一起按 \n 切分。
		if len(carry) > 0 {
			merged := make([]byte, 0, len(chunk)+len(carry))
			merged = append(merged, chunk...)
			merged = append(merged, carry...)
			chunk = merged
		}

		start := len(chunk) // 下一轮残片的起点（chunk 内部最早的那个未闭合行首偏移）
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] != '\n' {
				continue
			}
			line := strings.TrimRight(string(chunk[i+1:start]), "\r")
			start = i
			if line == "" {
				continue
			}
			if !visit(line) {
				// 早停。chunk[:i] 之前的内容（含尚未读的 pos 字节）都还没被消费，
				// 于是「读尽」的充要条件是 i==0 且 pos==0。
				return pos == 0 && i == 0, nil
			}
		}
		// chunk[:start] 是本轮最前面那一行（可能还没拼全）的尾部。
		carry = append([]byte(nil), chunk[:start]...)
	}

	// 文件第一行前面没有 \n，carry 即文件首行（也就是本次读取的最后一行）。
	if len(carry) > 0 {
		if line := strings.TrimRight(string(carry), "\r"); strings.TrimSpace(line) != "" {
			visit(line) // 返回值无需处理：这已是文件的最后一行
		}
	}
	return true, nil
}

// parseLogTime 尽力从一行日志开头解析时间戳，返回 Unix 毫秒。
//
// 诚实边界（重要）：这里只认 slog 的两种标准编码——
//  1. text：行首 `time=2026-11-08T10:00:00.000+08:00`
//  2. json：`{"time":"2026-11-08T10:00:00.000+08:00",...}`
//
// 其它形态（如自定义 handler、panic 栈、第三方库裸打印）一律解析不出。
// 调用方（logLineMatches）在设置了 from/to 时会把解析失败的行排除，
// 这是刻意取舍：宁可漏一条，也不要把无时间归属的行算进用户指定的区间。
func parseLogTime(line string) (int64, bool) {
	var raw string
	switch {
	case strings.HasPrefix(line, "time="):
		rest := line[len("time="):]
		if i := strings.IndexAny(rest, " \t"); i >= 0 {
			raw = rest[:i]
		} else {
			raw = rest
		}
	default:
		// json 格式：time 字段通常在最前，为稳妥起见在行首 128 字节内查找。
		head := line
		if len(head) > 128 {
			head = head[:128]
		}
		const marker = `"time":"`
		i := strings.Index(head, marker)
		if i < 0 {
			return 0, false
		}
		rest := head[i+len(marker):]
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			return 0, false
		}
		raw = rest[:j]
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z0700"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// parseLevel 抽取日志级别（小写）。按「结构化字段优先」的优先级判定：
//
//	1. json：`"level":"ERROR"` —— slog 的 JSONHandler 输出；
//	2. text：`level=ERROR`    —— slog 的 TextHandler 输出；
//	3. 都取不到才回退到旧 detectLevel 的裸子串匹配（向后兼容非 slog 产出的行）。
//
// 为什么必须优先走结构化字段：旧实现用 strings.Contains(line, "ERROR")，
// 只要工具参数/错误消息正文里出现 "ERROR" 一词，整行就被误判成 error 级，
// 导致面板按级别筛选时假阳性严重；JSON 格式下更是完全没有 `level=`，
// 旧条件直接失效、全部行都被归为 info。
func parseLevel(line string) string {
	const jsonMarker = `"level":"`
	if i := strings.Index(line, jsonMarker); i >= 0 {
		rest := line[i+len(jsonMarker):]
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			if lv := strings.ToLower(strings.TrimSpace(rest[:j])); lv != "" {
				return lv
			}
		}
	}
	const textMarker = "level="
	if i := strings.Index(line, textMarker); i >= 0 {
		rest := line[i+len(textMarker):]
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			end = len(rest)
		}
		if lv := strings.ToLower(strings.TrimSpace(rest[:end])); lv != "" {
			// 只接受已知级别，避免 `level=custom` 之类的脏值污染筛选。
			if isKnownLevel(lv) {
				return lv
			}
		}
	}
	return detectLevel(line)
}

// isKnownLevel 判断是否为 slog 标准级别名。
func isKnownLevel(lv string) bool {
	switch lv {
	case "error", "warn", "warning", "info", "debug":
		return true
	}
	return false
}

// detectLevel 旧版级别识别（裸子串匹配），仅作 parseLevel 的兜底。
//
// 保留它而不是直接删掉，是为了兼容历史/非标准格式日志行：
// 这些行没有 `level=` 或 `"level":"` 字段，宁可粗判也不要一律归 info。
func detectLevel(line string) string {
	for _, lv := range []string{"ERROR", "WARN", "INFO", "DEBUG"} {
		if strings.Contains(line, "level="+lv) || strings.Contains(line, lv) {
			return strings.ToLower(lv)
		}
	}
	return "info"
}
