package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"harness-go/internal/store"
)

// ============ 会话执行根解析（2026-11 目录选择放开） ============
//
// 背景：改造前工具执行根是**进程级固定**的（main.go 的 os.Getwd()），而用户在
// 目录选择器里选的路径只写进了 session.ProjectDir —— 结果就是"选了也没用"，
// 工具仍然只在启动目录里读写，甚至提示词还宣称"相对路径以 project_dir 为根"，
// 形成静默的行为偏差。
//
// 本文件把 project_dir 变成**真实执行根**，同时保持三道防线：
//  1. 绝对路径 + 存在性 + 是目录：不合法一律不注入（回落进程根）；
//  2. 白名单归属：必须落在服务端已开放的根内（FSRoots）；
//  3. 软链防护：解析真实路径后再校验一次，防「白名单内的软链指向白名单外」。
//
// 🚨 缺任何一道，放开执行根就等价于把整盘读写交给任何登录用户。

// ExecutionRootResolver 会话执行根解析器（实现 tools.ExecutionRootResolver）。
//
// 依赖面刻意收窄为「主库 + 白名单 + 日志」三者，仓库不存在时返回空根而非报错，
// 便于测试与「设备域/存储未装配」的部分部署形态。
type ExecutionRootResolver struct {
	main  *store.MainStore
	roots []string
	// enforce 是否强制白名单。false 时跳过归属校验（仅保留绝对/存在/是目录校验），
	// 供「单机自用、项目目录可任意选」的场景显式关闭。
	enforce bool
	log     *slog.Logger
}

// NewExecutionRootResolver 创建解析器。
//
// roots 为空表示「未开放任何执行根」：此时所有会话一律回落进程根，
// 与改造前行为一致（失败即关闭）。
//
// enforce=false 会跳过白名单归属校验，属**显式降级**：调用方（main）必须已在
// 启动日志里打印该状态，让运维知道主机文件读写面已放开。
func NewExecutionRootResolver(main *store.MainStore, roots []string, enforce bool, log *slog.Logger) *ExecutionRootResolver {
	return &ExecutionRootResolver{
		main:    main,
		roots:   normalizeRoots(roots),
		enforce: enforce,
		log:     log,
	}
}

// normalizeRoots 归一化白名单根（Abs + Clean + 去重）。
func normalizeRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		key := rootKey(abs)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, abs)
	}
	return out
}

// rootKey 路径比较键（Windows 大小写不敏感）。
func rootKey(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// ResolveExecutionRoot 实现 tools.ExecutionRootResolver。
//
// 返回 ("", nil)：该会话没有绑定项目目录（纯对话）或 main 未装配 —— 正常状态，
// 调用方回落进程根，**不是错误**。
//
// 返回 error：目录越界/不存在/非目录 —— 调用方必须回落进程根并告警。
func (r *ExecutionRootResolver) ResolveExecutionRoot(ctx context.Context, sessionID string) (string, error) {
	if r == nil || r.main == nil || strings.TrimSpace(sessionID) == "" {
		return "", nil
	}
	sess, err := r.main.Sessions().Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		// 主库读取失败：不阻断开工具调用，回落进程根（记录告警便于排查）。
		r.warn("读取会话失败，工具回落进程根", sessionID, err)
		return "", nil
	}
	dir := strings.TrimSpace(sess.ProjectDir)
	if dir == "" {
		return "", nil
	}

	// ① 必须是绝对路径（相对路径语义依赖进程 CWD，不可控）
	if !filepath.IsAbs(dir) {
		r.warn("会话项目目录不是绝对路径，拒绝作为执行根", sessionID, errors.New(dir))
		return "", errors.New("项目目录必须是绝对路径")
	}
	abs := filepath.Clean(dir)

	// ② 必须是真实存在的目录
	st, statErr := os.Stat(abs)
	if statErr != nil || !st.IsDir() {
		r.warn("会话项目目录不存在或不是目录，拒绝作为执行根", sessionID, statErr)
		return "", errors.New("项目目录不存在或不是目录")
	}

	// ③ 白名单归属（真实路径再校验一次，防软链逃逸）
	if !r.enforce {
		// 显式关闭白名单（单机自用形态）：只保留绝对/存在/是目录三项校验。
		return abs, nil
	}
	if len(r.roots) == 0 {
		// 未开放任何根：等价于「不启用会话执行根」，静默回落（非错误）。
		return "", nil
	}
	if !r.within(abs) {
		r.warn("会话项目目录不在已开放范围内，拒绝作为执行根", sessionID, errors.New(abs))
		return "", errors.New("项目目录不在已开放的目录范围内")
	}
	if real, realErr := filepath.EvalSymlinks(abs); realErr == nil {
		if !r.within(real) {
			r.warn("会话项目目录的真实路径越界（软链），拒绝作为执行根", sessionID, errors.New(real))
			return "", errors.New("项目目录真实路径越界")
		}
	}

	return abs, nil
}

// within 判断 target 是否落在任一白名单根内（或恰为根本身）。
func (r *ExecutionRootResolver) within(target string) bool {
	target = filepath.Clean(target)
	for _, root := range r.roots {
		rel, err := filepath.Rel(root, target)
		if err != nil {
			continue
		}
		if rel == "." {
			return true
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return true
	}
	return false
}

// warn 记录一条告警（logger 为 nil 时静默，便于测试）。
func (r *ExecutionRootResolver) warn(msg, sessionID string, err error) {
	if r.log == nil {
		return
	}
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	r.log.Warn(msg, "session_id", sessionID, "detail", detail)
}

// Roots 返回归一化后的白名单（只读副本，供启动期日志/调试）。
func (r *ExecutionRootResolver) Roots() []string {
	if r == nil {
		return nil
	}
	out := make([]string, len(r.roots))
	copy(out, r.roots)
	return out
}
