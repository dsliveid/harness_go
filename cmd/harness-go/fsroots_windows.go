//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// kernel32 GetLogicalDrives：标准库 syscall 包未导出该函数，且本仓库的
// golang.org/x/sys 只是 indirect 间接依赖 —— 为避免把它提升为直接依赖
// （会引发 go.mod 重排），这里用 LazyDLL 就地取过程地址，零新增依赖。
var procGetLogicalDrives = syscall.NewLazyDLL("kernel32.dll").NewProc("GetLogicalDrives")

// filesystemRoots 返回整机文件系统的根列表（Windows：各逻辑盘符）。
//
// 语义（2026-11 口径变更）：**默认不限制**目录选择范围 —— 未配置
// workspace.fs_roots 时用户应能浏览整台服务器，而不是只能浏览启动目录。
//
// 实现要点：
//   - GetLogicalDrives 返回盘符位图（位 0 = A:，位 25 = Z:），返回 0 表示失败；
//   - 逐个 os.Stat 过滤「不存在 / 未就绪」的盘（空光驱、已卸载的网络盘会 Stat 失败），
//     否则目录选择器里会出现一个点进去就报错的假根；
//   - 顺序天然按 A→Z，无需额外排序。
//
// 返回 nil 表示枚举失败（极少数受限环境），调用方会降级为数据目录父级并告警。
func filesystemRoots() []string {
	r, _, _ := procGetLogicalDrives.Call()
	if r == 0 {
		return nil
	}
	mask := uint32(r)
	out := make([]string, 0, 8)
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		st, sErr := os.Stat(root)
		if sErr != nil || !st.IsDir() {
			// 盘符存在但未就绪（空光驱 / 断连的网络盘）：不放进根列表。
			continue
		}
		out = append(out, filepath.Clean(root))
	}
	return out
}
