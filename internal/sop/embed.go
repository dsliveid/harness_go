package sop

import _ "embed"

// builtinCore 出厂核心 SOP（agent-core）。
//
// 这是「Agent 内置行为规范」的唯一出厂真源：编进二进制，**永不落盘**。
// 设计铁律（务必遵守）：
//  1. 绝不在启动或读取时把本内容播种（seed）到 data/sop/ —— 一旦播种，
//     二进制升级后旧副本会永久遮蔽新的出厂内容，这正是「改了不生效」的经典陷阱。
//  2. 生产环境只读；运行时可编辑的覆盖层见 data/sop/agent-core.md。
//  3. 用户在界面调教满意后，手动把正文贴回本文件并重新编译，即成为新的永久出厂。
//
// 内容来源：原 cmd/harness-go/main.go 的 const defaultSystemBase（原样迁移，行为零变化）。
//
//go:embed builtin/agent-core.md
var builtinCore string

// BuiltinCoreText 返回出厂核心 SOP 正文（只读副本）。
//
// # 为什么必须导出
//
// 改造前这段文本在 cmd/harness-go/main.go 里还有一份逐字重复的
// `defaultSystemBase` 常量，用于「SOP 服务未装配」的兜底路径。两份文本
// 靠人工保持一致 —— 改一处漏一处就是行为分叉，且**任何测试都看不出来**。
//
// 导出处唯一真源后，兜底路径改为引用本函数，重复真源被物理消除（阶段 5）。
// 调用方只读：返回字符串值，修改它不会影响本包的 embed 内容。
func BuiltinCoreText() string { return builtinCore }
