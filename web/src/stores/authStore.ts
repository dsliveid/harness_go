import { create } from 'zustand'
import { api, ApiError } from '../lib/api'

// ============ 账号密码登录 ============
// **real 模式**（构建产物默认）：全部经 `lib/api.ts` 打真实后端
//   POST /api/v1/auth/login | /sso | /logout | /password，会话凭据由服务端 Cookie(hg_sess) 承担，
//   前端不再落任何密码；启动时调 `GET /auth/me` 静默探测，避免闪登录页。
// **mock 模式**（dev 默认，用于离线调 UI）：保留下方 demo 的本地模拟账号表，
//   密码存 localStorage（仅演示，生产构建不会走这条路径）。

const LS_KEY = 'harness.auth.session' // 记住此设备（7 天）
const SS_KEY = 'harness.auth.session.tab' // 仅本标签页（关闭即失效）
const LS_PWD = 'harness.auth.credential' // 当前生效密码（原型模拟服务端凭据）
const DEFAULT_PASSWORD = 'harness123'
const REMEMBER_DAYS = 7

interface MockUser {
  username: string
  password: string
  name: string
  role: 'owner' | 'admin' | 'member'
}

/** 原型内置账号：登录页会提示演示凭据 */
const USERS: MockUser[] = [
  { username: 'admin', password: DEFAULT_PASSWORD, name: 'admin', role: 'owner' },
]

/** 当前生效密码：优先取已修改过的（模拟服务端凭据），否则为默认密码 */
function currentPassword(): string {
  try {
    return localStorage.getItem(LS_PWD) ?? DEFAULT_PASSWORD
  } catch {
    return DEFAULT_PASSWORD
  }
}

// 启动时把已修改过的密码恢复进内存账号表（模拟服务端持久化）
USERS[0].password = currentPassword()

export interface AuthUser {
  username: string
  name: string
  role: MockUser['role']
}

/** 当前是否走真实后端（构建产物默认 real，dev/显式切换为 mock） */
const isRealMode = () => api.mode === 'real'

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

interface PersistedSession {
  user: AuthUser
  loggedInAt: number
  expiresAt: number
  remember: boolean
}

interface AuthState {
  user: AuthUser | null
  loggedInAt: number | null
  expiresAt: number | null
  loading: boolean
  error: string | null
  /** 启动静默探测是否完成（real 模式未完成前渲染骨架，避免闪登录页） */
  booted: boolean
  /** 启动静默探测：real 模式调 GET /auth/me，Cookie 有效则直接进工作台 */
  boot: () => Promise<void>
  login: (username: string, password: string, remember: boolean) => Promise<boolean>
  /** SSO 免登（URL 参数兜底自愈；服务端已在 GET / 拦截，此处仅 dev/纯静态部署时生效） */
  ssoLogin: (userName: string, pwd?: string) => Promise<boolean>
  /** 演示直通：免输入直接登录（**仅 mock 模式**；real 模式无此入口） */
  loginDirect: () => Promise<boolean>
  /** 修改密码：成功返回 ok，失败返回 error 文案（real 模式文案来自服务端错误码） */
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

function readStore(key: string): PersistedSession | null {
  try {
    const raw = localStorage.getItem(key) ?? sessionStorage.getItem(key)
    if (!raw) return null
    const p = JSON.parse(raw) as PersistedSession
    if (!p?.user?.username) return null
    if (p.expiresAt <= Date.now()) {
      localStorage.removeItem(key)
      sessionStorage.removeItem(key)
      return null
    }
    return p
  } catch {
    return null
  }
}

function restore(): PersistedSession | null {
  return readStore(LS_KEY) ?? readStore(SS_KEY)
}

function clearAll() {
  try {
    localStorage.removeItem(LS_KEY)
    sessionStorage.removeItem(SS_KEY)
  } catch {
    /* 忽略隐私模式下的存储异常 */
  }
}

// real 模式不恢复本地会话（以服务端 Cookie 为准）；mock 模式沿用 demo 的本地持久化
const initial = isRealMode() ? null : restore()

export const useAuth = create<AuthState>((set, get) => ({
  user: initial?.user ?? null,
  loggedInAt: initial?.loggedInAt ?? null,
  expiresAt: initial?.expiresAt ?? null,
  loading: false,
  error: null,
  // mock 模式无需探测（本地已有登录态）；real 模式需先问服务端
  booted: !isRealMode(),

  // 启动静默探测：Cookie 有效则直接进工作台（避免首屏闪一下登录页）
  boot: async () => {
    if (!isRealMode()) {
      set({ booted: true })
      return
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

  // SSO 免登（服务端已在 GET / 拦截；此处是 dev/纯静态部署的兜底自愈）
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

    // ---- real 模式：服务端校验 + Cookie 会话（前端不落任何密码） ----
    if (isRealMode()) {
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
    }

    // ---- mock 模式：demo 的本地模拟（离线调 UI） ----
    await new Promise((r) => setTimeout(r, 420))
    const hit = USERS.find((x) => x.username.toLowerCase() === u.toLowerCase())
    if (!hit || hit.password !== p) {
      set({ loading: false, error: '账号或密码错误' })
      return false
    }

    const now = Date.now()
    const expiresAt = remember ? now + REMEMBER_DAYS * 24 * 3600 * 1000 : now + 12 * 3600 * 1000
    const user: AuthUser = { username: hit.username, name: hit.name, role: hit.role }
    const session: PersistedSession = { user, loggedInAt: now, expiresAt, remember }

    clearAll()
    try {
      if (remember) localStorage.setItem(LS_KEY, JSON.stringify(session))
      else sessionStorage.setItem(SS_KEY, JSON.stringify(session))
    } catch {
      /* 存储不可用时仅保留内存登录态 */
    }

    set({ user, loggedInAt: now, expiresAt, loading: false, error: null })
    return true
  },

  // 演示直通：以内置账号直接登录（免输入；**仅 mock 模式**，real 模式直接拒绝）
  loginDirect: () => {
    if (isRealMode()) return Promise.resolve(false)
    const u = USERS[0]
    return get().login(u.username, u.password, true)
  },

  changePassword: async (current, next) => {
    if (!current || !next) return { ok: false, error: '请填写当前密码与新密码' }
    if (next.length < 8) return { ok: false, error: '新密码至少 8 位' }
    if (current === next) return { ok: false, error: '新密码不能与当前密码相同' }

    // ---- real 模式：服务端校验旧密码 + 写新摘要 + 吊销其他会话 ----
    if (isRealMode()) {
      try {
        await api.changePassword({ old_password: current, new_password: next })
        return { ok: true }
      } catch (e) {
        return { ok: false, error: authErrorText(e, '修改失败，请重试') }
      }
    }

    // ---- mock 模式：本地模拟服务端凭据（仅演示） ----
    await new Promise((r) => setTimeout(r, 420))
    if (current !== currentPassword()) return { ok: false, error: '当前密码不正确' }

    USERS[0].password = next
    try {
      localStorage.setItem(LS_PWD, next)
    } catch {
      /* 存储不可用时仅内存生效（本标签页内有效） */
    }
    return { ok: true }
  },

  logout: () => {
    // real 模式同时通知服务端吊销会话（失败不阻塞本地登出）
    if (isRealMode()) void api.logout().catch(() => undefined)
    clearAll()
    set({ user: null, loggedInAt: null, expiresAt: null, error: null, loading: false, booted: true })
  },

  clearError: () => set({ error: null }),
}))
