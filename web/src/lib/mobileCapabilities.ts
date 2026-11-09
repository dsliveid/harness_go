/**
 * 移动端 WebView 能力声明读取层（消费 `window.harnessMobile.capabilities`）
 *
 * 背景：移动端 App 用 WebView 承载本 Web UI 时，会由原生侧注入
 *   window.harnessMobile = { installed, version, capabilities: {...}, openDeepLink }
 * 见 harness_mobile/lib/features/webview/js_bridge.dart（注入侧唯一真值，本文件只读消费，不反向修改）。
 * 设计依据：docs/移动端架构调整方案.md §4.3（能力声明注入）与 §4.7.1（审批只读）。
 *
 * ⚠️ 铁律一（零回归）：`window.harnessMobile` 不存在时（桌面端 / 纯浏览器 / 服务端直开页面），
 *   所有能力必须返回**宽松默认值**（全部为 true），保证既有行为与改动前完全一致。
 *
 * ⚠️ 铁律二（不是安全边界）：本文件只是 UI 降级提示层，声明值可被任意脚本篡改或绕过，
 *   **任何安全判定都不得依赖它**。真正的强制在服务端（协议 §5.3：服务端按会话来源与票据权限
 *   拒绝移动端来源的高危审批 / 项目创建请求）。此处的价值仅在于「不让用户点一个注定失败的按钮」。
 */

/** 与移动端注入的四个字段逐字一致（更改需与 js_bridge.dart 同步） */
export interface MobileCapabilities {
  /** 能否在客户端内直接作出审批决定（移动端为 false → 只读，需去电脑端确认） */
  approvalDecide: boolean
  /** 能否新建项目（移动端为 false） */
  createProject: boolean
  /** 能否上传附件（移动端为 true） */
  fileUpload: boolean
  /** 能否触发命令执行类操作（移动端为 false） */
  exec: boolean
}

/** 移动端注入到 window 上的对象形态（字段全部可选，缺失即视为「未注入」） */
export interface HarnessMobileShell {
  /** 是否已安装（字符串形式，如 'true' / 安装时间等），本层不消费，仅透出 */
  installed?: string
  version?: string
  capabilities?: Partial<MobileCapabilities>
  openDeepLink?: (url: string) => void
}

/** 未注入能力声明时的宽松默认值：全部放开，保证桌面端 / 浏览器端行为零回归 */
const LOOSE_CAPABILITIES: MobileCapabilities = {
  approvalDecide: true,
  createProject: true,
  fileUpload: true,
  exec: true,
}

/** 取 window.harnessMobile（SSR / 测试环境无 window 时安全返回 undefined） */
function mobileShell(): HarnessMobileShell | undefined {
  if (typeof window === 'undefined') return undefined
  const shell = (window as unknown as { harnessMobile?: HarnessMobileShell }).harnessMobile
  return shell && typeof shell === 'object' ? shell : undefined
}

/** 当前是否运行在移动端 WebView 壳内（注入了 window.harnessMobile 即为真） */
export function isMobileShell(): boolean {
  return mobileShell() !== undefined
}

/** 移动端壳版本号（未注入时为空串，纯展示用） */
export function mobileShellVersion(): string {
  return mobileShell()?.version ?? ''
}

/**
 * 读取当前有效能力声明。
 * 未注入 → 全部 true；注入了但个别字段缺失 → 该字段按 true（宽松）兜底。
 */
export function getMobileCapabilities(): MobileCapabilities {
  const caps = mobileShell()?.capabilities
  if (!caps) return { ...LOOSE_CAPABILITIES }
  return {
    approvalDecide: caps.approvalDecide ?? true,
    createProject: caps.createProject ?? true,
    fileUpload: caps.fileUpload ?? true,
    exec: caps.exec ?? true,
  }
}

/** 能否在客户端内直接审批（移动端壳内为 false → UI 只展示待审批卡片与提示文案） */
export function canDecideApproval(): boolean {
  return getMobileCapabilities().approvalDecide
}

/** 能否新建项目（移动端壳内为 false → 入口不渲染） */
export function canCreateProject(): boolean {
  return getMobileCapabilities().createProject
}

/** 能否上传附件（移动端壳内为 true，保留入口） */
export function canUploadFiles(): boolean {
  return getMobileCapabilities().fileUpload
}

/** 能否触发命令执行类操作（移动端壳内为 false） */
export function canExec(): boolean {
  return getMobileCapabilities().exec
}
