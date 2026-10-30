import type { ClientOS, DeviceNode, PlatformSupport } from '../types'

// ---------------- 客户端平台支持判定（客户端自行安装的兼容性门禁） ----------------
// 口径：PC 端必须区分 Windows / Linux；移动端合并为 mobile 一档。
// 缺省（platforms 未声明）视为全平台支持，避免遗漏字段直接阻断安装。

export const ALL_PLATFORMS: PlatformSupport = { windows: true, linux: true, mobile: true }
export const PC_ONLY: PlatformSupport = { windows: true, linux: true, mobile: false }
export const NONE_PLATFORMS: PlatformSupport = { windows: false, linux: false, mobile: false }

/** 当前端是否满足该能力的平台要求 */
export function supportsClient(platforms: PlatformSupport | undefined, os: ClientOS): boolean {
  if (!platforms || os === 'web') return true
  if (os === 'windows') return platforms.windows
  if (os === 'linux') return platforms.linux
  return platforms.mobile
}

/** 该能力支持的平台中文文案列表，如 ['Windows', 'Linux'] */
export function platformNames(platforms: PlatformSupport | undefined): string[] {
  if (!platforms) return ['Windows', 'Linux', '移动端']
  const out: string[] = []
  if (platforms.windows) out.push('Windows')
  if (platforms.linux) out.push('Linux')
  if (platforms.mobile) out.push('移动端')
  return out
}

/** 端不支持时的提示文案（安装按钮 disabled 的 title） */
export function unsupportedReason(platforms: PlatformSupport | undefined, client: DeviceNode): string {
  if (supportsClient(platforms, client.os)) return ''
  const need = platformNames(platforms).join(' / ') || '无'
  return `${client.name}（${osLabel(client.os)}）不支持该能力：仅支持 ${need}`
}

export const osLabel = (os: ClientOS): string =>
  ({ windows: 'Windows', linux: 'Linux', ios: 'iOS', android: 'Android', web: 'Web' } as const)[os]

/** 端节点的平台短标签（用于客户端切换条） */
export const clientPlatformLabel = (c: DeviceNode): string =>
  c.platform === 'mobile' ? '移动端' : c.platform === 'desktop' ? 'PC 端' : '服务端'
