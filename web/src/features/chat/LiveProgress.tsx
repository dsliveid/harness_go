/**
 * 实时进度可视化（对齐老版本 harness_mini 的 MessageItem 体感）。
 *
 * 存在意义（2026-11）：首次发送消息时，前端在「POST /runs 同步阻塞 + LLM 首字延迟(TTFT)」
 * 这段静默期里**没有任何进度反馈** —— 用户无法判断「到底在跑还是卡死」。
 * 老版本的做法是：
 *   ① `animate-bounce` 三点错峰弹跳（明确「正在加载」）；
 *   ② 整轮持续计时 `正在执行... 21分5秒`，配 `Clock animate-spin` 转圈图标，
 *      让用户**看得见时间在走**（时间在动 = 进程活着）。
 *
 * 🚨 计时基准铁律（2026-11 修正，上一版踩过的坑）：
 *   基准必须来自**消息自身的时间戳**（本轮 user 消息的 createdAt），
 *   而不是 store 里的全局「运行开始时刻」。老版本 types.ts:888-890 正是这么做的
 *   （`turnStartTime = currentTurnUser.createdAt`）。
 *
 *   为什么全局字段会出错：停止某个 Run 后再切换对话，全局字段会被 resumeRun /
 *   切会话分支重置为「此刻」→ 秒表从 0 重新开始，用户看到时长莫名倒退。
 *   而消息 createdAt 是服务端落库的不可变事实：切走、切回、刷新都读到同一个值，
 *   秒表自然接着走，无需任何额外的状态同步。
 */

import { useEffect, useState } from 'react'
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
 * - `running=false` 或 `startedAt<=0` 时**停止计时并归零**，不保留上一轮的残留值。
 * - 只在运行中挂 interval，非运行态不产生任何定时器（避免每轮空转）。
 * - 返回 0 表示「尚未开始」，调用方可据此决定是否渲染计时。
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
 * 为什么不复用 `.typing-dot`：那是 opacity+scale 呼吸效果，视觉上「静止感」更强；
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

/** 运行阶段：决定「加载中」文案。 */
export type RunPhase = 'idle' | 'posting' | 'connecting' | 'waiting'

/**
 * 阶段文案。
 *
 * 为什么分三段而不是一句「思考中」：首次发送的静默期由两段构成——
 * POST 在途（服务端同步做建库/落库/记忆召回/SOP读盘/工具检索）与
 * 模型首字延迟。用户看到「正在创建运行」→「正在建立连接」→「模型思考中」
 * 的推进，才能确信程序没有卡死。
 *
 * `hasTimer=false`（startedAt 未知）时**不显示时长**：
 * 显示一个永不变化的「0.1s」比不显示更像卡死，宁可只留动画。
 */
export function phaseText(phase: RunPhase, elapsed: number, hasTimer: boolean): string {
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
  return hasTimer ? `${base} ${formatDuration(elapsed) || '0.1s'}` : base
}

interface RunProgressInlineProps {
  /** 是否处于运行中 */
  running: boolean
  /**
   * 本轮开始时刻（毫秒）。
   *
   * 🚨 必须传**本轮 user 消息的 createdAt**，绝不能传 store 里的全局运行时刻：
   * 后者在停止/切换对话时会被重置，导致秒表从头开始（上一版的实际缺陷）。
   */
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
