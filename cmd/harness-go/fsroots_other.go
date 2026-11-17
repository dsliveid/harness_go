//go:build !windows

package main

import "os"

// filesystemRoots 返回整机文件系统的根列表（POSIX：唯一根 "/"）。
//
// 语义（2026-11 口径变更）：**默认不限制**目录选择范围 —— 未配置
// workspace.fs_roots 时用户应能浏览整台服务器，而不是只能浏览启动目录。
//
// "/" 不存在时返回 nil（理论上不可能，但保持与调用方的 nil=失败 契约一致）。
func filesystemRoots() []string {
	st, err := os.Stat("/")
	if err != nil || !st.IsDir() {
		return nil
	}
	return []string{"/"}
}
