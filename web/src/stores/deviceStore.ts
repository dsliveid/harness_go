import { create } from 'zustand'
import { ApiError, api } from '../lib/api'
import type { DeviceDTO, PairCodeDTO, SessionLeaseDTO } from '../lib/apiTypes'
import {
  DeviceEventsClient,
  type DeviceEventsState,
  type DeviceLiveState,
} from '../lib/deviceEvents'

/**
 * 设备状态（阶段 C-8）：多端协同 HALO 的设备清单 / 配对码 / 会话租约。
 *
 * 设计口径（与 B 阶段 store 一致）：
 * - 数据只来自服务端：本 store 只调 `api.*`，不持有任何本地演示数据；
 * - 服务端是唯一真源：所有变更后 **重拉列表**（不做乐观本地推算），避免「改了没保存」的错觉；
 * - 设备令牌（device_token）**永不进入前端 store 或日志**，只在端侧配对流程中出现一次。
 */
interface DeviceState {
  devices: DeviceDTO[]
  /** 服务端下发的节奏参数（用于 UI 展示 TTL / 心跳 / 离线阈值） */
  pairTTLSec: number
  heartbeatSec: number
  offlineAfterSec: number
  /** 当前展示的配对码（一次性，生成后倒计时过期） */
  pairCode: PairCodeDTO | null
  /** 会话租约（按会话 id 缓存；无租约 = active=true / epoch=0 / holder=''） */
  leases: Record<string, SessionLeaseDTO>
  loading: boolean
  /** 上一次错误（UI 用于展示错误条；成功后清空） */
  error: string
  /**
   * 实时状态（`/api/v1/devices/events` SSE）：
   * device_id → 在线状态。**与 `devices` 分离**，因为：
   * - `devices` 来自 `GET /api/v1/devices`（含静态档案 + 分页/排序语义）；
   * - `live` 来自 SSE（只描述「此刻是否在线」，高频变化）。
   *
   * 合并两者展示时以 `live` 为准（它更新），`devices` 提供 name/kind 等档案。
   */
  live: Record<string, DeviceLiveState>
  /** SSE 连接状态（UI 可据此展示「实时同步中 / 重连中」）。 */
  liveState: DeviceEventsState
  /** 开始实时订阅（幂等）。 */
  startLive: () => void
  /** 停止实时订阅（登出 / 解绑 / 页面卸载时调）。 */
  stopLive: () => void
  loadDevices: () => Promise<void>
  createPairCode: (input?: { name?: string; kind?: DeviceDTO['kind'] }) => Promise<PairCodeDTO | null>
  clearPairCode: () => void
  revoke: (id: string) => Promise<boolean>
  loadLease: (sessionId: string) => Promise<SessionLeaseDTO | null>
}

/**
 * 当前活跃的 SSE 订阅实例（模块级单例）。
 *
 * 为什么放在 store 外：它是**副作用句柄**而非状态 —— 放进 state 会让
 * zustand 的每次 set 都尝试 diff 一个 EventSource 对象（无意义且易泄漏）。
 */
let liveClient: DeviceEventsClient | null = null

/** 设备域错误统一转可读文案（503 device.disabled 属正常降级，不当作故障弹错） */
function deviceErrorText(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.code === 'device.disabled') return '设备功能未启用（服务端未装配 devices 段）'
    return e.message || `请求失败（${e.code}）`
  }
  return e instanceof Error ? e.message : '未知错误'
}

export const useDevices = create<DeviceState>((set, get) => ({
  devices: [],
  pairTTLSec: 120,
  heartbeatSec: 15,
  offlineAfterSec: 45,
  pairCode: null,
  leases: {},
  loading: false,
  error: '',
  live: {},
  liveState: 'idle',

  startLive: () => {
    // 幂等：已有连接则不重复建（EventSource 不会自己防抖）。
    if (liveClient) return
    const client = new DeviceEventsClient({
      onSnapshot: (all) => {
        // 全量覆盖：设备状态是「当前态」，重连后旧数据一律作废。
        const next: Record<string, DeviceLiveState> = {}
        for (const d of all) next[d.device_id] = d
        set({ live: next })
      },
      onDeviceChange: ({ type, device }) => {
        if (type === 'device.revoked') {
          // 解绑：绑定关系终止 → 从两个视图里**一并移除**（不是标灰）。
          // 只删 live 不删 devices 的话，卡片仍会以「离线」留在列表里，
          // 正是用户报告的「取消绑定后设备没消失」。
          set((s) => {
            const live = { ...s.live }
            delete live[device.device_id]
            return {
              live,
              devices: s.devices.filter((d) => d.id !== device.device_id),
            }
          })
          return
        }
        // 按 device_id 落位（重复事件幂等：同一状态覆盖写入无副作用）。
        set((s) => ({ live: { ...s.live, [device.device_id]: device } }))
      },
      onState: (state) => set({ liveState: state }),
    })
    liveClient = client
    client.connect()
  },

  stopLive: () => {
    liveClient?.dispose()
    liveClient = null
    set({ live: {}, liveState: 'closed' })
  },

  loadDevices: async () => {
    set({ loading: true })
    try {
      // 默认不含已吊销设备：吊销 = 解除绑定，列表只展示仍处于绑定关系的设备。
      const res = await api.listDevices()
      set({
        devices: res.items ?? [],
        pairTTLSec: res.pair_ttl_s || 120,
        heartbeatSec: res.heartbeat_s || 15,
        offlineAfterSec: res.offline_after_s || 45,
        loading: false,
        error: '',
      })
    } catch (e) {
      set({ loading: false, error: deviceErrorText(e) })
    }
  },

  createPairCode: async (input) => {
    try {
      const code = await api.createDevicePairCode({ name: input?.name, kind: input?.kind })
      set({ pairCode: code, error: '', pairTTLSec: code.ttl_s || get().pairTTLSec })
      return code
    } catch (e) {
      set({ error: deviceErrorText(e) })
      return null
    }
  },

  clearPairCode: () => set({ pairCode: null }),

  revoke: async (id) => {
    try {
      await api.revokeDevice(id)
      // 服务端是唯一真源，但「解绑即消失」不应等重拉才生效：
      // 先本地剔除该设备（UI 立刻响应），再重拉校正。
      const leases = { ...get().leases }
      for (const [sid, l] of Object.entries(leases)) {
        if (l.holder === id) leases[sid] = { ...l, active: false }
      }
      set((s) => {
        const live = { ...s.live }
        delete live[id]
        return { leases, live, devices: s.devices.filter((d) => d.id !== id) }
      })
      await get().loadDevices()
      return true
    } catch (e) {
      set({ error: deviceErrorText(e) })
      return false
    }
  },

  loadLease: async (sessionId) => {
    try {
      const lease = await api.getSessionLease(sessionId)
      set((s) => ({ leases: { ...s.leases, [sessionId]: lease }, error: '' }))
      return lease
    } catch (e) {
      // 设备域关闭（503）不代表会话不可用：按「服务端本地可写」展示
      const fallback: SessionLeaseDTO = {
        session_id: sessionId,
        holder: '',
        holder_kind: 'server',
        epoch: 0,
        active: true,
      }
      if (!(e instanceof ApiError) || e.code !== 'device.disabled') {
        set({ error: deviceErrorText(e) })
      }
      set((s) => ({ leases: { ...s.leases, [sessionId]: fallback } }))
      return fallback
    }
  },
}))
