import type { Risk } from '../types'

/**
 * 命令行拼装工具（对话流工具卡片与右侧命令行面板共用同一份实现）。
 *
 * 为什么单独抽出来：
 * - `run_command` 的真实参数形态是 `{cmd, args[], dir, timeout_s}`（见
 *   internal/plugins/command/manifest.json），命令行是 cmd + args 拼出来的，
 *   后端**不**下发 `cmdline` 字段，所以展示层必须自己拼，且拼法只能有一份，
 *   否则「工具卡片里的命令」和「命令行面板里的命令」会长得不一样。
 * - 拼出来的字符串要能直接复制粘贴到终端执行，因此含空格/特殊字符的参数
 *   必须加引号，不能简单 `join(' ')`（`git commit -m "a b"` 会退化成两个参数）。
 */

/** 需要加引号包裹的字符集合（空白与 shell 元字符） */
const NEEDS_QUOTE = /[\s"'`$\\|&;<>()*?[\]{}~#!^%]/

/** 单参数转义：命中元字符则用双引号包裹，内部双引号转义 */
function quoteArg(a: string): string {
  if (!a) return '""'
  if (!NEEDS_QUOTE.test(a)) return a
  return `"${a.replace(/"/g, '\\"')}"`
}

/**
 * 由工具参数拼出完整命令行。
 *
 * 兼容两种后端形态：
 * - `{cmd, args[]}`：内置 command 插件（正常路径）
 * - `{command}`：单一字符串形态（端侧/外部执行器可能的变体）
 * 无命令时返回空串，调用方据此决定不渲染（绝不编造占位命令）。
 */
export function buildCommandLine(args: Record<string, unknown> | undefined): string {
  if (!args) return ''
  const raw =
    typeof args.cmd === 'string'
      ? args.cmd
      : typeof args.command === 'string'
        ? args.command
        : ''
  const cmd = raw.trim()
  if (!cmd) return ''

  const rest = Array.isArray(args.args)
    ? args.args.filter((a): a is string => typeof a === 'string')
    : []
  return [cmd, ...rest.map(quoteArg)].join(' ')
}

/** 命令的工作目录（相对工作区根）；未提供时返回空串 */
export function commandDir(args: Record<string, unknown> | undefined): string {
  return typeof args?.dir === 'string' ? args.dir.trim() : ''
}

/** 命令超时（秒）；未提供或非法时返回 0（UI 不渲染） */
export function commandTimeout(args: Record<string, unknown> | undefined): number {
  const v = args?.timeout_s
  return typeof v === 'number' && Number.isFinite(v) && v > 0 ? Math.round(v) : 0
}

/**
 * 是否为命令类工具调用：risk=command，或工具名带命令语义
 * （兼容未来 `shell` / `bash` / `powershell` 等别名工具）。
 */
export function isCommandTool(name: string, risk?: Risk): boolean {
  if (risk === 'command') return true
  return /command|bash|shell|powershell|terminal/i.test(name || '')
}
