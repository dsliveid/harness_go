/**
 * 实时进度可视化（对齐老版本 harness_mini 的 MessageItem/formatDuration 体感）。
 *
 * 存在意义（2026-11）：首次发送消息时，前端在「POST /runs 同步阻塞 + LLM 首字延迟(TTFT)」
 * 这段静默期里**没有任何进度反馈** —— 消息虽然已乐观上屏，但下方只有一个静止的三点，
 * 用户无法判断「到底在跑还是卡死」。老版本的做法是：
 *   ① `animate-bounce` 三点错峰弹跳（明确「正在加载」）；
 *   ② `liveTurnDuration` 每轮持续计时，配 `Clock animate-spin` 转圈图标，
 *      让用户**看得见时间在走**（时间在动 = 进程活着）。
 *
 * 本文件把这两点抽成独立单元，供「消息气泡下方」与「输入框上方常驻条」两处复用，
 * 避免两份计时器各写一套（老版本踩过的重复状态机问题）。
 */

import { useEffect, useState } from 'react'
import { Clock, Loader2 } from 'lucide-react'
import { cn } from '../../components/ui'

/**
 * 时长格式化（逐字对齐老版本 MessageItem.tsx:25-41 的口径）。
 *
 * 为什么不用 ProcessDrawer 的 formatStepMs：那个是「步骤耗时」，<1s 显示 `ms`、
 * 空值返回空串；这里要的是「运行中持续跳动的秒表」，空值不应出现，
 * 且 <1s 也要显示 0.1s（老版本同为 `formatDuration(liveTurnDuration) || "0.1s"`）。
 */
export function formatDuration(ms?: number | null): string {
  if (ms == null || isNaN(ms)) return ''
  if (ms < 0) ms = 0
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  const mins = Math.floor(ms / 60_000)
  const secs = Math.floor((ms % 60_000) / 1000)
  return `${mins}分${secs}秒`
}

/**
 * 实时秒表：从 startedAt 起按 tick 间隔刷新已过毫秒数。
 *
 * - `running=false` 或 `startedAt<=0` 时**停止计时并归零**，不保留上一轮的残留值
 *   （否则切会话/收口后会显示上一个 Run 的时长，属于假数据）。
 * - 只在运行中挂 interval，非运行态不产生任何定时器（避免每轮空转）。
 * - 返回 0 表示「尚未开始」，调用方可据此决定是否渲染整条进度。
 */
export function useLiveElapsed(startedAt: number, running: boolean, tickMs = 100): number {
  const [elapsed, setElapsed] = useState(0)

  useEffect(() => {
    if (!running || !startedAt || startedAt <= 0) {
      setElapsed(0)
      return
    }
    // 立即算一次：避免首帧显示 0.0s 造成「刚点下去」的错觉
    setElapsed(Math.max(0, Date.now() - startedAt))
    const timer = setInterval(() => {
      setElapsed(Math.max(0, Date.now() - startedAt))
    }, tickMs)
    return () => clearInterval(timer)
  }, [startedAt, running, tickMs])

  return elapsed
}

/**
 * 三点错峰弹跳加载动画（对齐老版本 MessageItem.tsx:244-256 的 animate-bounce 错峰）。
 *
 * 为什么不复用在用的 `.typing-dot`：那是 opacity+scale 呼吸效果，视觉上「静止感」更强；
 * 老版本用的是纵向位移弹跳，用户更容易识别为「加载中」。
 */
export function LoadingDots({ className }: { className?: string }) {
  return (
    <span className={cn('inline-flex items-center gap-1', className)} aria-hidden>
      {[0, 120, 240].map((delay) => (
        <span
          key={delay}
          className="w-1.5 h-1.5 rounded-full bg-accent animate-bounce"
          style={{ animationDelay: `${delay}ms` }}
        />
      ))}
    </span>
  )
}

/** 运行阶段：决定文案。时间轴顺序即枚举顺序。 */
export type RunPhase = 'idle' | 'posting' | 'connecting' | 'waiting'

/**
 * 阶段文案。
 *
 * 为什么分三段而不是一句「思考中」：首次发送的静默期由两段构成——
 * POST 在途（服务端同步做建库/落库/记忆召回/SOP 读盘/工具检索）与
 * 模型首字延迟。用户看到「正在创建运行」→「正在建立连接」→「模型思考中」
 * 的推进，才能确信程序没有卡死。
 */
function phaseText(phase: RunPhase, elapsed: number, hasTimer = true): string {
  const base = (() => {
    switch (phase) {
      case 'posting':
        return '正在提交并创建运行…'
      case 'connecting':
        return '正在建立事件连接…'
      case 'waiting':
        return '模型思考中…'
      default:
        return ''
    }
  })()
  if (!base) return ''
  // hasTimer=false（startedAt 未知，如刷新恢复失败的兜底）时**不显示时长**：
  // 显示一个永不变化的「0.1s」比不显示更像卡死，宁可只留动画。
  return hasTimer ? `${base} ${formatDuration(elapsed) || '0.1s'}` : base
}

interface RunProgressInlineProps {
  /** 是否处于运行中 */
  running: boolean
  /** 运行开始时间戳（毫秒），0/空表示未知 */
  startedAt: number
  phase?: RunPhase
  className?: string
}

/**
 * 行内进度（挂在「用户消息 + 空 assistant 气泡」下方）。
 *
 * 这是「消息已上屏 → 下方显示加载中动画」的直接落点：
 * 用户消息一旦发出立刻可见，紧跟着的这一行就是「我确实在干活」的证据。
 */
export function RunProgressInline({ running, startedAt, phase = 'waiting', className }: RunProgressInlineProps) {
  const elapsed = useLiveElapsed(startedAt, running)
  if (!running) return null
  return (
    <div className={cn('flex items-center gap-2 text-inkfaint text-[12px] py-1', className)}>
      <LoadingDots />
      <span className="text-[12px] font-medium text-inkdim">
        {phaseText(phase, elapsed, startedAt > 0)}
      </span>
    </div>
  )
}

interface LiveProgressBarProps {
  running: boolean
  startedAt: number
  phase?: RunPhase
  /** 本轮已完成步数（可选，用于展示执行进度） */
  stepCount?: number
}

/**
 * 输入框上方的常驻执行时长条。
 *
 * 为什么常驻在输入框上方而不是消息流末尾：消息流是可滚动的，用户往上翻看历史时
 * 末尾的耗时会被滚出视野 —— 「始终显示」的要求就落空了。输入框区域是固定可见区，
 * 放这里才能真正做到「对话过程中下方始终显示执行时长」。
 */
export function LiveProgressBar({ running, startedAt, phase = 'waiting', stepCount = 0 }: LiveProgressBarProps) {
  const elapsed = useLiveElapsed(startedAt, running)
  if (!running) return null
  const hasTimer = startedAt > 0
  return (
    <div className="px-3 sm:px-5 pt-1.5 select-none">
      <div className="max-w-[860px] mx-auto flex items-center gap-2 rounded-lg border border-accent/25 bg-accent/[0.06] px-3 py-1.5 slide-up">
        {/* 转圈图标：与老版本 `Clock animate-spin` 同语义 —— 图标在转 = 进程活着 */}
        <Loader2 size={13} className="text-accent animate-spin shrink-0" />
        <span className="text-[12px] font-medium text-accent">{phaseText(phase, elapsed, hasTimer) || '运行中…'}</span>
        {stepCount > 1 && (
          <span className="text-[11px] text-inkfaint font-mono shrink-0">第 {stepCount} 步</span>
        )}
        {/* 时钟+时长常驻右侧：即使阶段文案很长被截断，秒表也始终可见 */}
        {hasTimer && (
          <span className="ml-auto inline-flex items-center gap-1 text-[11px] text-inkfaint font-mono shrink-0">
            <Clock size={11} className="opacity-70" />
            <span>{formatDuration(elapsed) || '0.1s'}</span>
          </span>
        )}
      </div>
    </div>
  )
}
