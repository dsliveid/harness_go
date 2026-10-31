// Package device 设备域（多端协同 HALO M5）。
//
// 分层铁律（docs/架构设计文档.md + 阶段 C 方案）：
//   - 本包**禁止 import** internal/{store,llm,agent,api,service}，只依赖 model 与标准库
//   - 需要的仓储能力一律在**使用方包内定义窄接口**，由 service 层适配注入
//   - 端侧永不直写 DB（不变量 1）：本包只做协议、状态与校验
package device

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"

	"harness-go/internal/model"
)

// DefaultPairTTL 配对码默认有效期（铁律：120s，见 protocols.md §6）。
const DefaultPairTTL = 120 * time.Second

// PairCodeLength 配对码位数（纯数字，便于用户口述/手输）。
const PairCodeLength = 6

// pairCodeMaxAttempts 生成码时的冲突重试次数（6 位数字空间 100 万，冲突概率极低）。
const pairCodeMaxAttempts = 5

// ErrPairCodeInvalid 配对码不可用。
//
// **防枚举铁律**：码不存在 / 已消费 / 已过期 / 格式非法，一律返回本错误，
// 上层必须翻译成同一个对外错误码（apperr.CodeDevicePairCodeInvalid），
// 不得向调用方泄露究竟是哪一种情况。
var ErrPairCodeInvalid = errors.New("device: 配对码无效")

// PairCodeStore 配对码仓储窄接口（在 device 包内定义，store 实现其方法集）。
type PairCodeStore interface {
	// Create 写入一条配对码（code 冲突返回错误，由本包重试）。
	Create(ctx context.Context, pc *model.DevicePairCode) error
	// Get 取配对码明细（不存在返回 store.ErrNotFound）。
	Get(ctx context.Context, code string) (*model.DevicePairCode, error)
	// Consume 原子消费：仅当码存在且未消费未过期时返回 true。
	Consume(ctx context.Context, code string, now int64) (bool, error)
	// PurgeExpired 清理过期码，返回删除行数。
	PurgeExpired(ctx context.Context, now int64) (int64, error)
}

// PairCodeIssuer 配对码签发与兑换（一次性 + TTL 120s）。
//
// 不持有任何内存态：码本体落主库，服务重启不打断仍在有效期内的配对窗口。
type PairCodeIssuer struct {
	store PairCodeStore
	ttl   time.Duration
	now   func() time.Time
	rand  io.Reader
}

// NewPairCodeIssuer 创建签发器；ttl<=0 时回退 DefaultPairTTL（120s）。
func NewPairCodeIssuer(store PairCodeStore, ttl time.Duration) *PairCodeIssuer {
	if ttl <= 0 {
		ttl = DefaultPairTTL
	}
	return &PairCodeIssuer{store: store, ttl: ttl, now: time.Now, rand: rand.Reader}
}

// TTL 返回当前配对码有效期（供端点回显与断言）。
func (p *PairCodeIssuer) TTL() time.Duration { return p.ttl }

// Issue 生成一条 6 位配对码。
//
// deviceName / deviceKind 是**预置提示**：端侧配对时若未自带名称则沿用（kind 非法时回退 desktop）。
func (p *PairCodeIssuer) Issue(ctx context.Context, deviceName string, kind model.DeviceKind) (*model.DevicePairCode, error) {
	if !kind.Valid() {
		kind = model.DeviceKindDesktop
	}
	now := p.now()
	expires := now.Add(p.ttl).UnixMilli()

	var lastErr error
	for attempt := 0; attempt < pairCodeMaxAttempts; attempt++ {
		code, err := p.newCode()
		if err != nil {
			return nil, err
		}
		pc := &model.DevicePairCode{
			Code:       code,
			DeviceName: strings.TrimSpace(deviceName),
			DeviceKind: kind,
			ExpiresAt:  expires,
			CreatedAt:  now.UnixMilli(),
		}
		if err := p.store.Create(ctx, pc); err != nil {
			// 主键冲突（极小概率）换一个码重试；也覆盖其他写入故障的最后兜底
			lastErr = err
			continue
		}
		return pc, nil
	}
	if lastErr == nil {
		lastErr = errors.New("device: 生成配对码失败")
	}
	return nil, fmt.Errorf("device: 生成配对码失败: %w", lastErr)
}

// Redeem 兑换配对码（一次性消费）。
//
// 返回码明细（含预置 name/kind）供上层建设备；任何不可用情形一律返回 ErrPairCodeInvalid。
//
// 顺序刻意为「先原子消费，再读明细」：消费是单条 UPDATE 的条件命中判定，
// 不存在 / 已消费 / 已过期三种情况**在 device 包内无法区分、也不需要区分**，
// 天然满足防枚举要求，且避免多一次查询与"先查后改"竞态。
func (p *PairCodeIssuer) Redeem(ctx context.Context, code string) (*model.DevicePairCode, error) {
	code = strings.TrimSpace(code)
	if !ValidPairCode(code) {
		return nil, ErrPairCodeInvalid
	}
	now := p.now().UnixMilli()
	ok, err := p.store.Consume(ctx, code, now)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrPairCodeInvalid
	}
	pc, err := p.store.Get(ctx, code)
	if err != nil {
		// 消费成功后读明细失败属基础设施故障（不是"码无效"），交由上层按 internal 处理
		return nil, err
	}
	return pc, nil
}

// Purge 清理已过期配对码（可由维护任务周期调用）。
func (p *PairCodeIssuer) Purge(ctx context.Context) (int64, error) {
	return p.store.PurgeExpired(ctx, p.now().UnixMilli())
}

// SetClock 注入时钟（测试用）。
func (p *PairCodeIssuer) SetClock(fn func() time.Time) {
	if fn != nil {
		p.now = fn
	}
}

// newCode 生成 6 位纯数字码（含前导零，均匀分布）。
func (p *PairCodeIssuer) newCode() (string, error) {
	max := big.NewInt(1)
	for i := 0; i < PairCodeLength; i++ {
		max.Mul(max, big.NewInt(10))
	}
	n, err := rand.Int(p.rand, max)
	if err != nil {
		return "", fmt.Errorf("device: 生成随机配对码失败: %w", err)
	}
	return fmt.Sprintf("%0*d", PairCodeLength, n.Int64()), nil
}

// ValidPairCode 校验码形状（必须是 6 位纯数字）。
func ValidPairCode(code string) bool {
	if len(code) != PairCodeLength {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

// ============ 设备令牌 ============

// DeviceTokenPrefix 设备令牌前缀（便于日志/审计识别，与用户令牌 hgt_ 区分）。
const DeviceTokenPrefix = "hgd_"

// NewDeviceToken 生成设备令牌明文（仅在配对成功时返回一次，库内只存 sha256）。
func NewDeviceToken() (string, error) {
	b := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("device: 生成设备令牌失败: %w", err)
	}
	return DeviceTokenPrefix + hex.EncodeToString(b), nil
}

// HashDeviceToken 设备令牌摘要（sha256 hex），用于 devices.token_hash 与 WS 升级前鉴权。
func HashDeviceToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// CapabilitiesHash 计算能力集合摘要（按 name 排序后拼接 name@version）。
//
// 端侧 ping 上报同一摘要时服务端无需重建能力表；摘要变化即触发能力刷新。
func CapabilitiesHash(caps []model.Capability) string {
	names := make([]string, 0, len(caps))
	index := make(map[string]string, len(caps))
	for _, c := range caps {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		names = append(names, name)
		index[name] = name + "@" + strings.TrimSpace(c.Version)
	}
	// 插入排序（能力项数量很小，避免引入 sort 依赖影响可读性）
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(index[n]))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
