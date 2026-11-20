package api

import (
	"context"
	"sync"
	"time"

	"harness-go/internal/model"
)

// ============ 设备能力协商的小型 TTL 缓存 ============
//
// ## 为什么需要
//
// fsDispatchDevice 每次列举目录都要先问一遍「这台设备申报了 workspace.list 吗」，
// 而 DeviceCapabilities 走的是 service → store 的查库路径。目录浏览是高频轻操作
// （用户连点层级能在几百毫秒内打出十几个请求），每次都查库纯属浪费，
// 还会把无谓的 DB 压力叠加到一条本来就很热的路径上。
//
// ## ⚠️ 安全边界（必须在改动时反复确认）
//
// **缓存只加速能力对齐，不承担任何安全判定。** 能否真正列出某个目录，最终由端侧
// Policy 白名单决定（端侧每次都现查 policy，服务端缓存不到也不缓存它）。
// 因此这里最坏情况只是「用 30 秒前的申报去判断该不该派发」，绝不会让越界路径
// 因为缓存而变得可读 —— 越界仍由端侧逐次拒绝并映射成 fs.path_denied。
//
// ## 为什么是这个形状
//
//   - 端口签名**不变**：本缓存包装在 internal/api 内部，handler.go 的 Deps 语义零改动，
//     因此其他调用点无需任何迁移即可受益（也避免了为拿 capabilities_hash 去侵入 gateway）；
//   - 键只含 deviceID：capabilities_hash 需要额外的端侧握手字段，端口不便取得，
//     而申报变更必然伴随重连 —— 30s TTL 已远小于重连尺度，纯 TTL 足够；
//   - 容量上限 + 惰性过期（与 device.PathQuery 的失败快照同风格）：不启 GC 协程，
//     只在写入点顺手清理，避免长期运行内存增长。

// capabilityCacheTTL 缓存有效期。
//
// 取 30 秒：远小于「用户发现能力不生效」的忍耐尺度（要重试点十几次才会察觉），
// 又远大于目录浏览的连点间隔（毫秒级），命中率足够高。
const capabilityCacheTTL = 30 * time.Second

// capabilityCacheMaxEntries 缓存容量上限（防御性：设备很多时不无限增长）。
const capabilityCacheMaxEntries = 1024

// capabilityCacheEntry 一条缓存。
type capabilityCacheEntry struct {
	caps      []model.Capability
	expiresAt time.Time
}

// capabilityCache 以 deviceID 为键的能力缓存（零值即可用，无需构造函数）。
type capabilityCache struct {
	mu      sync.Mutex
	entries map[string]capabilityCacheEntry
	// ttl 有效期覆盖（0 表示用默认 capabilityCacheTTL；测试注入短 TTL 用）。
	ttl time.Duration
	// hits / misses 命中统计（观测与测试断言用）。
	hits   int
	misses int
}

// effectiveTTL 实际生效的 TTL。
func (c *capabilityCache) effectiveTTL() time.Duration {
	if c.ttl > 0 {
		return c.ttl
	}
	return capabilityCacheTTL
}

// get 读取缓存（过期条目视为 miss 并顺手清除）。
func (c *capabilityCache) get(deviceID string, now time.Time) ([]model.Capability, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[deviceID]
	if !ok {
		c.misses++
		return nil, false
	}
	if !now.Before(e.expiresAt) {
		// 惰性过期：读到过期项就地清理，避免需要独立 GC 协程。
		delete(c.entries, deviceID)
		c.misses++
		return nil, false
	}
	c.hits++
	return e.caps, true
}

// put 写入缓存（含容量上限与惰性清理）。
func (c *capabilityCache) put(deviceID string, caps []model.Capability, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]capabilityCacheEntry)
	}
	c.pruneLocked(now)
	if len(c.entries) >= capabilityCacheMaxEntries {
		c.evictOldestLocked()
	}
	c.entries[deviceID] = capabilityCacheEntry{caps: caps, expiresAt: now.Add(c.effectiveTTL())}
}

// pruneLocked 清理过期条目（调用方已持锁）。
func (c *capabilityCache) pruneLocked(now time.Time) {
	for id, e := range c.entries {
		if !now.Before(e.expiresAt) {
			delete(c.entries, id)
		}
	}
}

// evictOldestLocked 容量吃紧时丢弃最早过期者（调用方已持锁）。
func (c *capabilityCache) evictOldestLocked() {
	var oldestID string
	var oldest time.Time
	first := true
	for id, e := range c.entries {
		if first || e.expiresAt.Before(oldest) {
			oldestID, oldest, first = id, e.expiresAt, false
		}
	}
	if oldestID != "" {
		delete(c.entries, oldestID)
	}
}

// reset 清空缓存与统计（测试用）。
func (c *capabilityCache) reset() {
	c.mu.Lock()
	c.entries = nil
	c.hits, c.misses = 0, 0
	c.mu.Unlock()
}

// stats 读取（命中, 未命中）计数。
func (c *capabilityCache) stats() (hits, misses int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// deviceCapabilitiesCached 带 TTL 缓存的能力查询。
//
// 语义与直接调用 Deps.DeviceCapabilities **完全一致**（含错误透出）：
// 缓存只在成功返回后写入，端口报错不缓存，从而不会把一次瞬时故障冻结 30 秒。
func (h *handlers) deviceCapabilitiesCached(ctx context.Context, deviceID string) ([]model.Capability, error) {
	port := h.d.DeviceCapabilities
	if port == nil {
		return nil, nil
	}
	now := time.Now()
	if caps, ok := h.capsCache.get(deviceID, now); ok {
		return caps, nil
	}
	caps, err := port(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	h.capsCache.put(deviceID, caps, now)
	return caps, nil
}
