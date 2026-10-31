import { create } from 'zustand'
import { ApiError, api, apiMode } from '../lib/api'
import type { DeviceDTO, PairCodeDTO, SessionLeaseDTO } from '../lib/apiTypes'

/**
 * 设备状态（阶段 C-8）：多端协同 HALO 的设备清单 / 配对码 / 会话租约。
 *
 * 设计口径（与 B 阶段 store 一致）：
 * - **不含 mock 分支**：mock 与 real 的差异全部收敛在 `lib/api.ts` 的两个实现里，
 *   本 store 只调 `api.*`，故 `apiMode==='mock'` 时也天然可用；
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
  loadDevices: () => Promise<void>
  createPairCode: (input?: { name?: string; kind?: DeviceDTO['kind'] }) => Promise<PairCodeDTO | null>
  clearPairCode: () => void
  revoke: (id: string) => Promise<boolean>
  loadLease: (sessionId: string) => Promise<SessionLeaseDTO | null>
}

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

  loadDevices: async () => {
    set({ loading: true })
    try {
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
      // 服务端为唯一真源：吊销后重拉（同时清掉该设备的租约缓存展示）
      const leases = { ...get().leases }
      for (const [sid, l] of Object.entries(leases)) {
        if (l.holder === id) leases[sid] = { ...l, active: false }
      }
      set({ leases })
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

/** 便于调试：控制台可确认当前数据模式（mock 时设备列表来自 api.ts 的样例） */
export const deviceStoreApiMode = apiMode
