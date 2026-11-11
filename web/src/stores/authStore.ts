import { create } from 'zustand'
import { api, ApiError } from '../lib/api'
import { takeTicketFromLocation } from '../lib/webviewTicket'

// ============ 账号密码登录 ============
// 全部经 `lib/api.ts` 打真实后端：
//   POST /api/v1/auth/login | /sso | /logout | /password，会话凭据由服务端 Cookie(hg_sess) 承担，
//   前端不落任何密码；启动时调 `GET /auth/me` 静默探测，避免闪登录页。

export interface AuthUser {
  username: string
  name: string
  role: 'owner' | 'admin' | 'member'
}

/** 后端 User DTO → UI AuthUser（display_name 缺省回退 username） */
function toAuthUser(u: { username: string; display_name?: string; role?: string }): AuthUser {
  const role: AuthUser['role'] =
    u.role === 'owner' || u.role === 'admin' || u.role === 'member' ? u.role : 'member'
  return { username: u.username, name: u.display_name || u.username, role }
}

/** ApiError → 中文引导文案（服务端不区分「用户不存在/密码错误」，前端同样不区分） */
function authErrorText(e: unknown, fallback: string): string {
  if (e instanceof ApiError) {
    switch (e.code) {
      case 'auth.invalid_credentials':
        return '账号或密码错误'
      case 'auth.rate_limited':
        return '尝试过于频繁，请稍后再试'
      case 'auth.sign_expired':
        return '免登链接已过期，请手动登录'
      case 'auth.sso.disabled':
        return '单点登录未开启，请手动登录'
      case 'auth.password_policy':
        return '新密码不符合要求：至少 8 位，且不能与当前密码相同'
      case 'auth.session_expired':
        return '登录已过期，请重新登录'
      default:
        return e.message || fallback
    }
  }
  return '无法连接服务端，请确认服务已启动'
}

interface AuthState {
  user: AuthUser | null
  loggedInAt: number | null
  expiresAt: number | null
  loading: boolean
  error: string | null
  /** 启动静默探测是否完成（未完成前渲染骨架，避免闪登录页） */
  booted: boolean
  /**
   * 启动静默探测，职责有**两段**（顺序不可交换）：
   *   ① 先消费 fragment 里的一次性 WebView ticket（协议 §6.2 方案 C）：
   *      读取 → 立刻用 `history.replaceState` 抹掉地址栏 → `POST /auth/webview-exchange`
   *      换一个降级 `hg_sess` Cookie。这是移动端 WebView「免登」的唯一接线点，
   *      缺了它 ticket 就无人消费，首屏必然回落登录页（历史 bug：两轮登录）。
   *   ② 再调 `GET /auth/me` 确认会话，Cookie 有效则直接进工作台（避免首屏闪登录页）。
   */
  boot: () => Promise<void>
  login: (username: string, password: string, remember: boolean) => Promise<boolean>
  /** SSO 免登（URL 参数兜底自愈；服务端已在 GET / 拦截，此处仅纯静态部署时生效） */
  ssoLogin: (userName: string, pwd?: string) => Promise<boolean>
  /** 修改密码：成功返回 ok，失败返回 error 文案（文案来自服务端错误码） */
  changePassword: (current: string, next: string) => Promise<{ ok: boolean; error?: string }>
  logout: () => void
  clearError: () => void
}

const ROLE_LABEL: Record<AuthUser['role'], string> = {
  owner: '所有者',
  admin: '管理员',
  member: '成员',
}
export const roleLabel = (r?: AuthUser['role']) => (r ? ROLE_LABEL[r] : '—')

export const useAuth = create<AuthState>((set) => ({
  // 会话凭据只以服务端 Cookie(hg_sess) 为准，本地不恢复任何登录态
  user: null,
  loggedInAt: null,
  expiresAt: null,
  loading: false,
  error: null,
  // 必须等一次 /auth/me 探测完成才渲染登录页，否则会闪一下登录页
  booted: false,

  // 启动静默探测：Cookie 有效则直接进工作台（避免首屏闪一下登录页）
  boot: async () => {
    // ① 先消费 WebView 免登 ticket（协议 §6.2 方案 C 的 Web 端一半）：
    //    读到 ticket 就**立刻**用 replaceState 抹干净地址栏（先抹再请求，防历史/Referer 重放），
    //    随后才发兑换请求。
    const ticket = takeTicketFromLocation(window.location, (url) =>
      window.history.replaceState({}, '', url),
    )
    if (ticket) {
      try {
        await api.exchangeWebviewTicket(ticket)
      } catch {
        // 静默失败：ticket 可能已过期/已被用过/空白页误带参数。此时回落登录页即可，
        // 绝不把兑换失败升级成全局 error 或白屏（会话有效性统一由下面的 /auth/me 判定）。
      }
    }
    try {
      const me = await api.me()
      const now = Date.now()
      set(
        me
          ? { user: toAuthUser(me), loggedInAt: now, expiresAt: null, booted: true, error: null }
          : { user: null, loggedInAt: null, expiresAt: null, booted: true },
      )
    } catch {
      // 服务未起/网络异常不应把用户永久卡在骨架屏：降级为未登录（登录页会显示原因）
      set({ user: null, booted: true })
    }
  },

  // SSO 免登（服务端已在 GET / 拦截；此处是纯静态部署的兜底自愈）
  ssoLogin: async (userName, pwd) => {
    set({ loading: true, error: null })
    try {
      const res = await api.ssoLogin({ user_name: userName, pwd })
      const now = Date.now()
      set({
        user: toAuthUser(res.user),
        loggedInAt: now,
        expiresAt: null,
        loading: false,
        error: null,
      })
      return true
    } catch (e) {
      set({ loading: false, error: authErrorText(e, '免登失败，请手动登录') })
      return false
    }
  },

  login: async (username, password, remember) => {
    const u = username.trim()
    const p = password
    if (!u || !p) {
      set({ loading: false, error: '请输入账号与密码' })
      return false
    }
    set({ loading: true, error: null })

    // 服务端校验 + Cookie 会话（前端不落任何密码；remember 由服务端 Cookie 有效期承载）
    try {
      const res = await api.login({ username: u, password: p })
      const now = Date.now()
      set({
        user: toAuthUser(res.user),
        loggedInAt: now,
        expiresAt: res.expires_at ?? null,
        loading: false,
        error: null,
      })
      return true
    } catch (e) {
      set({ loading: false, error: authErrorText(e, '登录失败') })
      return false
    }
  },

  changePassword: async (current, next) => {
    if (!current || !next) return { ok: false, error: '请填写当前密码与新密码' }
    if (next.length < 8) return { ok: false, error: '新密码至少 8 位' }
    if (current === next) return { ok: false, error: '新密码不能与当前密码相同' }

    // 服务端校验旧密码 + 写新摘要 + 吊销其他会话
    try {
      await api.changePassword({ old_password: current, new_password: next })
      return { ok: true }
    } catch (e) {
      return { ok: false, error: authErrorText(e, '修改失败，请重试') }
    }
  },

  logout: () => {
    // 同时通知服务端吊销会话（失败不阻塞本地登出）
    void api.logout().catch(() => undefined)
    set({ user: null, loggedInAt: null, expiresAt: null, error: null, loading: false, booted: true })
  },

  clearError: () => set({ error: null }),
}))
