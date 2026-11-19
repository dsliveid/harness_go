/**
 * sessionStore 实时合并逻辑单测。
 *
 * 被测对象：`applyServerSnapshot` / `applyServerPatch` / `applyServerRemoval`
 * ——会话列表级 SSE（`/api/v1/sessions/events`）推来的服务端视图如何落位到本地列表。
 *
 * 为什么这三段值得单独测：它们是全链路里最容易**悄悄写错**的地方，且错法都很隐蔽：
 *   - 排序不稳定 → 列表每帧莫名跳动（用户以为是卡了）；
 *   - 当前会话丢失 → 右侧对话区指向幽灵会话；
 *   - 乐观壳被快照抹掉 → 点了「新对话」但列表里没有它；
 *   - 归属未补 → 项目会话掉进「纯对话」（历史缺陷复发）。
 * 这些错都不会抛异常、不会 500，只会「看起来怪」，靠手工点很难稳定复现。
 *
 * 不测 SSE 传输层（`lib/sessionEvents.ts` 的 EventSource 封装）：真实帧契约由
 * 服务端测试 `internal/api/session_events_test.go` 覆盖，那才是权威口径。
 */
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useSessions } from './sessionStore'
import type { Project, Session } from '../types'

/** 造 UI 会话（补全必填字段，避免每个用例都写一遍噪音）。 */
const mkSession = (over: Partial<Session> & { id: string }): Session =>
  ({
    title: 't',
    accessMode: 'confirm',
    updatedAt: 1000,
    ...over,
  }) as Session

/** 造项目（defaultAccessMode 是必填字段）。 */
const mkProject = (over: Partial<Project> & { id: string; path: string }): Project => ({
  name: over.id,
  defaultAccessMode: 'confirm',
  ...over,
}) as Project

const reset = (sessions: Session[], currentId = '', projects: Project[] = []) => {
  useSessions.setState({ sessions, currentId, projects })
}

const ids = () => useSessions.getState().sessions.map((s) => s.id)

describe('sessionStore.applyServerSnapshot', () => {
  beforeEach(() => reset([]))

  it('以服务端列表为准覆盖本地，并按 updatedAt 降序排列', () => {
    reset([mkSession({ id: 's-stale', updatedAt: 1 })])
    useSessions.getState().applyServerSnapshot([
      mkSession({ id: 's-a', updatedAt: 300 }),
      mkSession({ id: 's-b', updatedAt: 200 }),
    ])
    // 本地陈旧条目不在快照里且不是乐观壳 → 必须消失
    expect(ids()).toEqual(['s-a', 's-b'])
  })

  it('保留本地乐观壳（服务端尚不知道的「新对话」不能被抹掉）', () => {
    // create() 造的临时 id 形态：s- + 13 位毫秒时间戳
    const optimistic = mkSession({ id: 's-1731000000000', updatedAt: 999_999 })
    reset([optimistic])
    useSessions.getState().applyServerSnapshot([mkSession({ id: 's-real', updatedAt: 1 })])

    expect(ids()).toContain('s-1731000000000')
    expect(ids()).toContain('s-real')
    // 乐观壳 updatedAt 更新 → 排在最前（刚新建的对话就该在最上）
    expect(ids()[0]).toBe('s-1731000000000')
  })

  it('不把「服务端 id 形态」的旧条目当乐观壳保留（防幽灵会话复活）', () => {
    // 别端已删除的会话：不在快照里，且 id 不符合乐观壳形态 → 必须移除。
    // 若误判成乐观壳，这个幽灵会永久留在列表里，点进去必 404。
    reset([mkSession({ id: 'srv-abc', updatedAt: 100 })])
    useSessions.getState().applyServerSnapshot([mkSession({ id: 's-real', updatedAt: 1 })])
    expect(ids()).toEqual(['s-real'])
  })

  it('稳界：updatedAt 相同时保持原有相对顺序（列表不乱跳）', () => {
    reset([])
    useSessions.getState().applyServerSnapshot([
      mkSession({ id: 's-1', updatedAt: 500 }),
      mkSession({ id: 's-2', updatedAt: 500 }),
      mkSession({ id: 's-3', updatedAt: 500 }),
    ])
    expect(ids()).toEqual(['s-1', 's-2', 's-3'])
  })

  it('当前会话在快照中消失时回落到首条', () => {
    reset([mkSession({ id: 'gone', updatedAt: 100 })], 'gone')
    useSessions.getState().applyServerSnapshot([
      mkSession({ id: 'keep-1', updatedAt: 50 }),
      mkSession({ id: 'keep-2', updatedAt: 40 }),
    ])
    expect(useSessions.getState().currentId).toBe('keep-1')
  })

  it('当前会话仍在快照中时保持不变（不抢焦点）', () => {
    reset([mkSession({ id: 'keep-1', updatedAt: 100 })], 'keep-1')
    useSessions.getState().applyServerSnapshot([
      mkSession({ id: 'keep-1', updatedAt: 100 }),
      mkSession({ id: 'newer', updatedAt: 200 }),
    ])
    expect(useSessions.getState().currentId).toBe('keep-1')
  })

  it('忽略空快照（断线重连时不得把列表清空）', () => {
    reset([mkSession({ id: 's-1', updatedAt: 200 })], 's-1')
    useSessions.getState().applyServerSnapshot([])
    expect(ids()).toEqual(['s-1'])
    expect(useSessions.getState().currentId).toBe('s-1')
  })

  it('按 project_dir 反查补回项目归属（不补就会掉进「纯对话」）', () => {
    const project = mkProject({ id: 'p-1', path: '/repo/a', workspaceId: 'ws-1' })
    reset([], '', [project])
    useSessions.getState().applyServerSnapshot([
      mkSession({ id: 's-1', projectDir: '/repo/a', updatedAt: 10 }),
    ])
    const s = useSessions.getState().sessions[0]
    expect(s.projectId).toBe('p-1')
    expect(s.workspaceId).toBe('ws-1')
  })
})

describe('sessionStore.applyServerPatch', () => {
  beforeEach(() => reset([]))

  it('对已存在会话做覆盖（upsert，服务端视图为准）', () => {
    reset([mkSession({ id: 's-1', title: '旧标题', running: false })])
    useSessions.getState().applyServerPatch(
      mkSession({ id: 's-1', title: '新标题', running: true }),
    )
    const s = useSessions.getState().sessions.find((x) => x.id === 's-1')
    expect(s?.title).toBe('新标题')
    expect(s?.running).toBe(true)
    // 不得变成两条（覆盖而非追加）
    expect(useSessions.getState().sessions).toHaveLength(1)
  })

  it('对不存在的会话做插入（别端新建的会话实时出现）', () => {
    reset([mkSession({ id: 's-1', updatedAt: 100 })])
    useSessions.getState().applyServerPatch(mkSession({ id: 's-2', updatedAt: 200 }))
    expect(ids()).toEqual(['s-2', 's-1'])
  })

  it('重复同一帧幂等（SSE 重连补发/重复帧安全）', () => {
    reset([mkSession({ id: 's-1' })])
    const frame = mkSession({ id: 's-1', running: true })
    useSessions.getState().applyServerPatch(frame)
    useSessions.getState().applyServerPatch(frame)
    expect(useSessions.getState().sessions).toHaveLength(1)
  })

  it('乱序到达同一会话时以后到的为准（幂等覆盖，无累积副作用）', () => {
    reset([])
    useSessions.getState().applyServerPatch(mkSession({ id: 's-1', title: 'A', updatedAt: 300 }))
    useSessions.getState().applyServerPatch(mkSession({ id: 's-1', title: 'B', updatedAt: 200 }))
    expect(useSessions.getState().sessions).toHaveLength(1)
    expect(useSessions.getState().sessions[0].title).toBe('B')
  })
})

describe('sessionStore.applyServerRemoval', () => {
  beforeEach(() => reset([]))

  it('移除指定会话，不动当前会话', () => {
    reset([mkSession({ id: 's-1', updatedAt: 200 }), mkSession({ id: 's-2', updatedAt: 100 })], 's-1')
    useSessions.getState().applyServerRemoval('s-2')
    expect(ids()).toEqual(['s-1'])
    expect(useSessions.getState().currentId).toBe('s-1')
  })

  it('移除当前会话时回落到首条（不留在幽灵会话上）', () => {
    reset([mkSession({ id: 's-1', updatedAt: 200 }), mkSession({ id: 's-2', updatedAt: 100 })], 's-1')
    useSessions.getState().applyServerRemoval('s-1')
    expect(useSessions.getState().currentId).toBe('s-2')
  })

  it('移除最后一条时 currentId 置空', () => {
    reset([mkSession({ id: 's-1', updatedAt: 200 })], 's-1')
    useSessions.getState().applyServerRemoval('s-1')
    expect(useSessions.getState().sessions).toHaveLength(0)
    expect(useSessions.getState().currentId).toBe('')
  })

  it('移除不存在的 id 是无操作（不产生新数组引用，避免无谓重渲染）', () => {
    reset([mkSession({ id: 's-1', updatedAt: 200 })], 's-1')
    const before = useSessions.getState().sessions
    useSessions.getState().applyServerRemoval('nope')
    expect(useSessions.getState().sessions).toBe(before)
  })
})

/**
 * 2026-11 用户诉求回归：
 *   ① 会话列表必须按「最新时间」倒序（刚聊过的排最上）；
 *   ② 删除当前会话后回落到「当前空间内最新一条」，而不是机械取数组首条。
 */
describe('sessionStore.bumpSession（发消息即置顶）', () => {
  beforeEach(() => reset([]))

  it('把目标会话的 updatedAt 推到现在并排到最前', () => {
    reset([
      mkSession({ id: 's-old', updatedAt: 100 }),
      mkSession({ id: 's-newest', updatedAt: 300 }),
    ])
    // 给最旧的会话发消息 → 它必须立刻置顶（服务端 TouchMessage 是异步回写，
    // 在此之前列表纹丝不动，用户会以为「刚聊过的对话不往上走」）
    useSessions.getState().bumpSession('s-old', 500)
    expect(ids()).toEqual(['s-old', 's-newest'])
    expect(useSessions.getState().sessions[0].updatedAt).toBe(500)
  })

  it('不存在的会话是无操作', () => {
    reset([mkSession({ id: 's-1', updatedAt: 100 })])
    const before = useSessions.getState().sessions
    useSessions.getState().bumpSession('ghost', 999)
    expect(useSessions.getState().sessions).toBe(before)
  })

  it('不影响其它会话的 updatedAt（只动目标一条）', () => {
    reset([
      mkSession({ id: 's-1', updatedAt: 100 }),
      mkSession({ id: 's-2', updatedAt: 200 }),
    ])
    useSessions.getState().bumpSession('s-1', 300)
    const other = useSessions.getState().sessions.find((s) => s.id === 's-2')
    expect(other?.updatedAt).toBe(200)
  })
})

describe('sessionStore.applyServerRemoval 回落口径（当前空间最新）', () => {
  beforeEach(() => reset([]))

  it('回落到当前空间内的最新会话，而非别的空间更新的会话', () => {
    // 场景：当前空间 ws-1 里有 s-ws1-new（updatedAt 300）；
    // 另一个空间 ws-2 有个更新的 s-ws2（updatedAt 900）。
    // 删掉 ws-1 的当前会话后，必须落到 ws-1 自己的最新会话，
    // 否则用户会被「传送到另一个空间的对话」而看不到本空间的列表。
    reset(
      [
        mkSession({ id: 's-del', updatedAt: 900, workspaceId: 'ws-1' }),
        mkSession({ id: 's-ws1-new', updatedAt: 300, workspaceId: 'ws-1' }),
        mkSession({ id: 's-ws2', updatedAt: 950, workspaceId: 'ws-2' }),
      ],
      's-del',
    )
    useSessions.setState({ activeWorkspaceId: 'ws-1' })
    useSessions.getState().applyServerRemoval('s-del')
    expect(useSessions.getState().currentId).toBe('s-ws1-new')
  })

  it('同空间存在多条时优先选最新的那条', () => {
    reset(
      [
        mkSession({ id: 's-del', updatedAt: 900, workspaceId: 'ws-1' }),
        mkSession({ id: 's-old', updatedAt: 100, workspaceId: 'ws-1' }),
        mkSession({ id: 's-latest', updatedAt: 500, workspaceId: 'ws-1' }),
      ],
      's-del',
    )
    useSessions.setState({ activeWorkspaceId: 'ws-1' })
    useSessions.getState().applyServerRemoval('s-del')
    expect(useSessions.getState().currentId).toBe('s-latest')
  })

  it('同空间最新会话已归档时跳过它（不选归档会话）', () => {
    reset(
      [
        mkSession({ id: 's-del', updatedAt: 900, workspaceId: 'ws-1' }),
        mkSession({ id: 's-archived', updatedAt: 800, workspaceId: 'ws-1', archivedAt: 800 }),
        mkSession({ id: 's-live', updatedAt: 100, workspaceId: 'ws-1' }),
      ],
      's-del',
    )
    useSessions.setState({ activeWorkspaceId: 'ws-1' })
    useSessions.getState().applyServerRemoval('s-del')
    expect(useSessions.getState().currentId).toBe('s-live')
  })
})

describe('sessionStore.markRead（未读红点）', () => {
  beforeEach(() => reset([]))

  it('对未读会话：本地立刻清红点并上报服务端', async () => {
    const { api } = await import('../lib/api')
    const spy = vi.spyOn(api, 'markSessionRead').mockResolvedValue(undefined)

    reset([mkSession({ id: 's-1', unread: true })], 's-1')
    useSessions.getState().markRead('s-1')

    // 本地乐观清除：不等网络往返就应立即变已读（否则红点迟滞一拍才消失）
    expect(useSessions.getState().sessions[0].unread).toBe(false)
    expect(spy).toHaveBeenCalledWith('s-1')
    spy.mockRestore()
  })

  it('对已读会话：不发请求（避免切来切去产生无效 POST）', async () => {
    const { api } = await import('../lib/api')
    const spy = vi.spyOn(api, 'markSessionRead').mockResolvedValue(undefined)

    reset([mkSession({ id: 's-1', unread: false })], 's-1')
    useSessions.getState().markRead('s-1')

    expect(spy).not.toHaveBeenCalled()
    spy.mockRestore()
  })

  it('上报失败时静默（红点只清本地，不弹错误打扰对话）', async () => {
    const { api } = await import('../lib/api')
    const spy = vi.spyOn(api, 'markSessionRead').mockRejectedValue(new Error('network'))

    reset([mkSession({ id: 's-1', unread: true })], 's-1')
    expect(() => useSessions.getState().markRead('s-1')).not.toThrow()
    expect(useSessions.getState().sessions[0].unread).toBe(false)
    spy.mockRestore()
  })

  it('空 id 是无操作', () => {
    reset([mkSession({ id: 's-1', unread: true })], 's-1')
    expect(() => useSessions.getState().markRead('')).not.toThrow()
    expect(useSessions.getState().sessions[0].unread).toBe(true)
  })

  it('select() 切换会话时自动上报已读（任何入口都不漏点）', async () => {
    const { api } = await import('../lib/api')
    const spy = vi.spyOn(api, 'markSessionRead').mockResolvedValue(undefined)

    reset(
      [mkSession({ id: 's-1', unread: true }), mkSession({ id: 's-2', unread: true })],
      's-1',
    )
    useSessions.getState().select('s-2')

    expect(spy).toHaveBeenCalledWith('s-2')
    // 被选中的那条已清红点，未选中的那条保持未读
    const byId = (id: string) => useSessions.getState().sessions.find((s) => s.id === id)
    expect(byId('s-2')?.unread).toBe(false)
    expect(byId('s-1')?.unread).toBe(true)
    spy.mockRestore()
  })
})
