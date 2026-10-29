package service

import (
	"context"

	"harness-go/internal/observ"
	"harness-go/internal/store"
)

// MaintenanceStats 维护接口返回的统计（不暴露数据目录绝对路径以外的敏感信息）。
type MaintenanceStats struct {
	ProtocolVersion  string        `json:"protocol_version"`
	Sessions         int64         `json:"sessions"`
	OpenSessionDBs   int           `json:"open_session_dbs"`
	RunningRuns      int           `json:"running_runs"`
	PendingApprovals int           `json:"pending_approvals"`
	Main             store.DBStats `json:"main_db"`
}

// MaintenanceService 运维统计（双库视图）。
type MaintenanceService struct {
	main     *store.MainStore
	sessions *store.SessionStore
	runs     *RunService
	pending  func() int
}

// NewMaintenanceService 创建运维用例。
func NewMaintenanceService(main *store.MainStore, sessions *store.SessionStore, runs *RunService, pending func() int) *MaintenanceService {
	if pending == nil {
		pending = func() int { return 0 }
	}
	return &MaintenanceService{main: main, sessions: sessions, runs: runs, pending: pending}
}

// Stats 采集统计（只读主库 + 连接池计数，不扫会话库）。
func (s *MaintenanceService) Stats(ctx context.Context) (MaintenanceStats, error) {
	mainStats, err := s.main.Stats(ctx)
	if err != nil {
		return MaintenanceStats{}, err
	}
	count, err := s.main.Sessions().Count(ctx)
	if err != nil {
		return MaintenanceStats{}, err
	}
	running := 0
	if s.runs != nil {
		running = s.runs.RunningCount()
	}
	return MaintenanceStats{
		ProtocolVersion:  observ.ProtocolVersion,
		Sessions:         count,
		OpenSessionDBs:   s.sessions.OpenCount(),
		RunningRuns:      running,
		PendingApprovals: s.pending(),
		Main:             mainStats,
	}, nil
}
