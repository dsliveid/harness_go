package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"harness-go/internal/config"
	"harness-go/internal/logvault"
	"harness-go/internal/observ"
)

// 保留天数合法区间。下限 1 天：0 天等价于「立刻删光」，语义上应表达为关闭抓取
// （档位切 off）而不是把保留期设成 0；上限 365 天：防止手滑输入把磁盘拖爆。
// 注：settings 键名常量定义在 settings_service.go（白名单与校验表同处一文件，
// 便于一眼确认「唯一权威取值来源」），此处不重复声明，避免包内重复定义。
const (
	logRetentionMinDays = 1
	logRetentionMaxDays = 365
)

// LogConfigOptions 构造 LogConfigService 的依赖。
type LogConfigOptions struct {
	Settings *SettingsService
	// Modes 报文档位的热读提供者；为 nil 时 SetSnapshotMode 只写库不刷新
	//（单测场景下允许缺失）。
	Modes *logvault.ModeProvider
	// Retention 保留策略执行器；可为 nil（单测）。
	Retention *logvault.Retention
	// DefaultMode config 兜底档位（settings 缺失/脏值时使用）。
	DefaultMode string
	// DefaultRetentionDays config 兜底保留天数。
	DefaultRetentionDays int
	// DefaultLevel config 兜底日志级别（settings 缺失/脏值时使用）。
	DefaultLevel string
	// LevelVar slog 的运行时级别句柄（来自 observ.NewLoggerWithLevel）。
	//
	// 为 nil 时 SetLevel 只写库不改进程级别（单测场景允许缺失）；
	// 非 nil 时一次 Set 即刻生效，且控制台与 data/app.log 同步变化。
	LevelVar *slog.LevelVar
}

// LogConfigService 日志抓取档位、保留策略与日志级别的运行时配置。
// 档位存 settings 表 log.snapshot_mode，保留天数存 log.retention_days，
// 日志级别存 log.level。
// 变更后必须让写入路径立即生效（否则要等重启）：档位走 ModeProvider.Refresh，
// 级别走 slog.LevelVar.Set。
type LogConfigService struct {
	settings      *SettingsService
	modes         *logvault.ModeProvider
	retention     *logvault.Retention // 可为 nil（测试时）
	levelVar      *slog.LevelVar      // 可为 nil（测试时）
	defaultMode   string              // config 兜底值
	retentionDays int                 // config 兜底值
	defaultLevel  string              // config 兜底值
}

// NewLogConfigService 创建日志配置用例。
func NewLogConfigService(opt LogConfigOptions) *LogConfigService {
	defMode := opt.DefaultMode
	if !config.ValidSnapshotMode(defMode) {
		// 兜底值本身非法时固定用 preview：它是「不丢正文也不落盘」的安全档位。
		defMode = config.SnapshotPreview
	}
	defDays := opt.DefaultRetentionDays
	if defDays < logRetentionMinDays || defDays > logRetentionMaxDays {
		defDays = 30
	}
	defLevel := opt.DefaultLevel
	if !config.ValidLogLevel(defLevel) {
		defLevel = config.DefaultLogLevel
	}
	return &LogConfigService{
		settings:      opt.Settings,
		modes:         opt.Modes,
		retention:     opt.Retention,
		levelVar:      opt.LevelVar,
		defaultMode:   defMode,
		retentionDays: defDays,
		defaultLevel:  defLevel,
	}
}

// SnapshotMode 读取当前生效的抓取档位。
//
// 读库失败、键缺失、值非法三种情况都回落到 defaultMode 而不报错：
// 这是一个「被界面高频轮询」的只读接口，为一个脏值就让整页设置判失败没有意义。
func (s *LogConfigService) SnapshotMode(ctx context.Context) string {
	if s == nil || s.settings == nil {
		return config.SnapshotPreview
	}
	raw, err := s.settings.main.Settings().Get(ctx, settingsKeyLogSnapshotMode)
	if err != nil {
		return s.defaultMode
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		return s.defaultMode
	}
	if !config.ValidSnapshotMode(mode) {
		return s.defaultMode
	}
	return mode
}

// SetSnapshotMode 写入抓取档位并立刻让写入路径生效。
//
// 非法值直接返回 400（invalidArg），**不静默兜底**：若在这里悄悄改成 preview，
// 用户会以为「切 full 成功了」却永远拿不到正文，属于最难排查的静默失效。
func (s *LogConfigService) SetSnapshotMode(ctx context.Context, mode string) (string, error) {
	if s == nil || s.settings == nil {
		return "", invalidArg("日志配置服务未接入")
	}
	if !config.ValidSnapshotMode(mode) {
		return "", invalidArg("抓取档位非法: %q（合法值: off|preview|full）", mode)
	}
	encoded, err := json.Marshal(mode)
	if err != nil {
		return "", invalidArg("抓取档位序列化失败: %v", err)
	}
	if err := s.settings.main.Settings().Set(ctx, settingsKeyLogSnapshotMode, encoded); err != nil {
		return "", err
	}
	// Refresh 必须在写库成功之后：先刷新后写库，一旦写库失败就会出现
	//「进程内已生效、重启后回滚」的不一致状态。
	if s.modes != nil {
		s.modes.Refresh(mode)
	}
	return mode, nil
}

// Level 读取当前生效的日志级别。
//
// 优先返回**进程内真实生效值**（LevelVar），而不是读库：若有人绕过 SetLevel
// 直接改库，读库会给出与运行时不一致的答案，界面就会骗人。
// LevelVar 为 nil（未接入热调，如单测）时才回落到读库 + config 兜底。
func (s *LogConfigService) Level(ctx context.Context) string {
	if s != nil && s.levelVar != nil {
		// LevelVar.Level().String() 返回大写（INFO/WARN），统一转小写以对齐
		// config 取值与前端下拉选项，避免「设置里显示 WARN、库里存 warn」的双口径。
		return strings.ToLower(s.levelVar.Level().String())
	}
	return s.storedLevel(ctx)
}

// storedLevel 读取 settings 表里**持久化**的级别（不看进程内状态）。
//
// 与 Level 的分工：Level 回答「现在实际用什么」，storedLevel 回答「上次保存了什么」。
// 启动恢复必须用后者——此时 LevelVar 刚由 config 初始化，用 Level 比较恒相等（死代码）。
func (s *LogConfigService) storedLevel(ctx context.Context) string {
	if s == nil || s.settings == nil {
		return config.DefaultLogLevel
	}
	raw, err := s.settings.main.Settings().Get(ctx, settingsKeyLogLevel)
	if err != nil {
		// 含 store.ErrNotFound（键从未写过）：统一回落到 config 兜底值。
		return s.defaultLevel
	}
	var level string
	if err := json.Unmarshal(raw, &level); err != nil {
		return s.defaultLevel
	}
	if !config.ValidLogLevel(level) {
		return s.defaultLevel
	}
	return level
}

// ApplyStoredLevel 把 settings 里上次保存的级别应用到进程内 logger（启动时调用一次）。
//
// 为什么需要它：config 的 Log.Level 只是「首次启动默认值」，用户若在界面调过级别，
// 重启后必须恢复用户的选择，否则会「重启就变回 config 默认」——这是最容易被当成 bug 的行为。
// 只改进程内状态、**不写库**（值本来就来自库），故与 SetLevel 分开。
func (s *LogConfigService) ApplyStoredLevel(ctx context.Context) string {
	level := s.storedLevel(ctx)
	if s != nil && s.levelVar != nil {
		s.levelVar.Set(observ.ParseLevel(level))
	}
	return level
}

// SetLevel 写入日志级别并立刻让进程内 logger 生效。
//
// 非法值直接返回 400（invalidArg），**不静默兜底**：若悄悄改成 info，
// 用户会以为「已降噪」却发现控制台照样刷屏。
//
// 生效范围：因为控制台与 data/app.log 共用同一个 slog handler（同一 io.MultiWriter），
// 一次 Set 同时作用于两者——这正是「界面与控制台显示同一套」的实现基础。
func (s *LogConfigService) SetLevel(ctx context.Context, level string) (string, error) {
	if s == nil || s.settings == nil {
		return "", invalidArg("日志配置服务未接入")
	}
	if !config.ValidLogLevel(level) {
		return "", invalidArg("日志级别非法: %q（合法值: debug|info|warn|error）", level)
	}
	encoded, err := json.Marshal(level)
	if err != nil {
		return "", invalidArg("日志级别序列化失败: %v", err)
	}
	if err := s.settings.main.Settings().Set(ctx, settingsKeyLogLevel, encoded); err != nil {
		return "", err
	}
	// 与 SetSnapshotMode 同样的次序铁律：先写库成功，再改进程内状态。
	// 反过来一旦写库失败，就会出现「进程内已降噪、重启后又刷屏」的错位。
	if s.levelVar != nil {
		s.levelVar.Set(observ.ParseLevel(level))
	}
	return level, nil
}

// RetentionDays 读取当前生效的保留天数（缺失/非法返回 config 兜底值）。
func (s *LogConfigService) RetentionDays(ctx context.Context) int {
	if s == nil || s.settings == nil {
		return 30
	}
	raw, err := s.settings.main.Settings().Get(ctx, settingsKeyLogRetentionDays)
	if err != nil {
		// 含 store.ErrNotFound（键从未写过）：统一回落到 config 兜底值。
		return s.retentionDays
	}
	var days int
	if err := json.Unmarshal(raw, &days); err != nil {
		return s.retentionDays
	}
	if days < logRetentionMinDays || days > logRetentionMaxDays {
		return s.retentionDays
	}
	return days
}

// SetRetentionDays 写入保留天数（合法区间 1~365）。
//
// 生效时机（必须在界面上明说，否则必然误解）：本方法只改「下次执行清理时的判定天数」，
// 已过期的天库要等下一次 Retention.Sweep 才真正删盘——清理跑在后台协程里，
// 不会因为这里改了个数字就立刻删文件。logvault.Retention 的判定参数在构造时快照，
// 故当前实现下天数变更需重启进程才对 Sweep 生效；Web 层应在响应中提示这一点。
func (s *LogConfigService) SetRetentionDays(ctx context.Context, days int) (int, error) {
	if s == nil || s.settings == nil {
		return 0, invalidArg("日志配置服务未接入")
	}
	if days < logRetentionMinDays || days > logRetentionMaxDays {
		return 0, invalidArg("保留天数超出合法区间 %d~%d: %d", logRetentionMinDays, logRetentionMaxDays, days)
	}
	encoded, err := json.Marshal(days)
	if err != nil {
		return 0, invalidArg("保留天数序列化失败: %v", err)
	}
	if err := s.settings.main.Settings().Set(ctx, settingsKeyLogRetentionDays, encoded); err != nil {
		return 0, err
	}
	return days, nil
}
