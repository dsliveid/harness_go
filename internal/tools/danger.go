package tools

import (
	"regexp"
	"strings"
)

// 高危命令特征检测（对齐桌面端 tools/helpers.rs::is_high_danger）。
//
// 命中即把 run_command 的风险级从 command 升级为 danger——审批引擎对 danger 的
// 处理是「full（完全访问）模式下仍逐次审批、不可按会话放行」（approval/gate.go），
// 这是唯一防住 `rm -rf /` 在完全访问下被静默执行的兜底，属内核安全边界而非工具实现。
//
// 维护口径：与桌面端特征表保持同构（新增特征两端口进）；模式一律 (?i) 大小写不敏感。
var dangerPatterns = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`\brm\s+(-[a-z]+)*\s*-[a-z]*r[a-z]*\b`, // rm -r / -rf（递归删除）
	`\bdel\b.*?(?:[/\\]s\b|[/\\]s:)`,       // Windows del /s
	`\b(?:rd|rmdir)\b.*?(?:[/\\]s\b|[/\\]s:)`,
	`\bformat\s+[a-z]:`,
	`\bmkfs`,
	`\bdd\s+if=`,
	`\bshutdown\b`,
	`\breboot\b`,
	`\breg\s+delete\b`,
	`\bgit\s+push\s+.*--force\b`,
	`\bgit\s+reset\s+--hard\b`,
}, "|"))

// IsDangerCommand 判定整条命令行是否命中高危特征。
func IsDangerCommand(cmdline string) bool {
	return dangerPatterns.MatchString(cmdline)
}
