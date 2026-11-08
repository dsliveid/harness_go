// Package logvault 实现「天分片审计日志库族」——与业务库（main.db / session.db）物理隔离。
//
// 为什么独立成库族（而不是塞进会话库）：
//   - 业务库必须保持纯净：会话库要参与导出/备份/删除，塞入每日数 GB 的报文会撑爆全部三条路径；
//   - 跨会话按时间查询在「一会话一库」架构下需要遍历 N 个库，每次 Acquire 都跑
//     PRAGMA integrity_check + 出池 wal_checkpoint(TRUNCATE)，实测 10~50s；
//   - 天分片把「时间维度」下沉到文件名，查 1 小时只需开 1 个库（<100ms）；
//   - 过期清理退化为删文件（原子操作，<100ms），无需 DELETE/VACUUM。
//
// 铁律：
//  1. 本包不参与任何业务事务。天库是可丢弃数据（有保留期），丢只影响查询不影响业务。
//  2. 写入一律经 Writer 的有界队列 + 批量事务，绝不进入 Run 关键路径（对齐
//     docs/基础服务设计.md:301「快照写入不阻塞 TTFT」）。
//  3. 天库 schema 允许 PRAGMA user_version + 幂等补列（与业务库的版本化迁移不同，
//     因为历史天库 schema 落后不影响业务正确性）。业务库仍走 //go:embed migrations。
package logvault

import (
	"strings"
	"sync/atomic"

	"harness-go/internal/config"
)

// SnapshotMode 报文抓取档位（off / preview / full）。
type SnapshotMode string

const (
	// ModeOff 只记元数据，不记任何报文内容。
	ModeOff SnapshotMode = SnapshotMode(config.SnapshotOff)
	// ModePreview 记脱敏预览 + sha256 + 字节数，正文不落盘（默认）。
	ModePreview SnapshotMode = SnapshotMode(config.SnapshotPreview)
	// ModeFull 正文落 data/logs/blob/<day>/<xx>/<sha256>。
	ModeFull SnapshotMode = SnapshotMode(config.SnapshotFull)
)

// Normalize 把任意字符串归一为合法档位（非法值回退 preview，绝不 panic）。
func Normalize(s string) SnapshotMode {
	switch SnapshotMode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeOff:
		return ModeOff
	case ModeFull:
		return ModeFull
	default:
		return ModePreview
	}
}

// ModeProvider 提供**可热读**的报文抓取档位。
//
// 为什么需要它：抓取档位是界面上的运行时开关（settings 表 log.snapshot_mode），
// 而写入路径每天要跑数十万次。若每次都 SELECT settings，就是数十万次额外 DB 查询，
// 直接抬高 TTFT。故这里用 atomic.Value 缓存，由 settings 变更回调 Refresh。
//
// 零值（未构造）也可安全使用：SnapshotMode() 返回 ModePreview。
type ModeProvider struct {
	v atomic.Value // 存 SnapshotMode
}

// NewModeProvider 以初始档位创建 Provider（空/非法值按 preview）。
func NewModeProvider(initial string) *ModeProvider {
	p := &ModeProvider{}
	p.Refresh(string(Normalize(initial)))
	return p
}

// SnapshotMode 读取当前档位（O(1)，无锁、无 IO，可在写入热路径调用）。
func (p *ModeProvider) SnapshotMode() SnapshotMode {
	if p == nil {
		return ModePreview
	}
	if v, ok := p.v.Load().(SnapshotMode); ok {
		return v
	}
	return ModePreview
}

// Refresh 更新档位（settings 变更成功后由用例层调用）。
//
// 语义（必须在界面上明说，否则必然误解）：
//   - 切到 full：只对之后的新调用生效，**不补录历史**；
//   - 切到 off：只停新写入，**不删除已有数据**；
//   - 切回 preview：已有 full 的正文 blob 不删不改。
func (p *ModeProvider) Refresh(mode string) {
	if p == nil {
		return
	}
	p.v.Store(Normalize(mode))
}

// WantsPayload 是否需要写入报文内容（preview 与 full 都需要）。
func (m SnapshotMode) WantsPayload() bool { return m != ModeOff }

// WantsBlob 是否需要把正文外置为 blob（仅 full）。
func (m SnapshotMode) WantsBlob() bool { return m == ModeFull }
