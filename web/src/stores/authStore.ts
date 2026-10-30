import { create } from 'zustand'

// ============ 账号密码登录（原型 mock） ============
// 真实实现：POST /api/auth/login { username, password } → 服务端校验 bcrypt 摘要，
// 返回会话 Cookie / token；这里用本地账号表模拟，仅存登录态，不存密码。
// 修改密码同理：真实实现为 PUT /api/auth/password（校验旧密码摘要、写新摘要、吊销其他会话）；
// 原型把「当前生效密码」存 localStorage（harness.auth.credential）模拟服务端落库，明文仅限演示。

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
  login: (username: string, password: string, remember: boolean) => Promise<boolean>
  /** 演示直通：免输入直接登录（原型专用，正式版移除；服务端应禁用匿名/演示入口） */
  loginDirect: () => Promise<boolean>
  /** 修改密码：校验当前密码 + 新密码规则；成功返回 ok，失败返回 error 文案 */
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

const initial = restore()

export const useAuth = create<AuthState>((set, get) => ({
  user: initial?.user ?? null,
  loggedInAt: initial?.loggedInAt ?? null,
  expiresAt: initial?.expiresAt ?? null,
  loading: false,
  error: null,

  login: async (username, password, remember) => {
    set({ loading: true, error: null })
    // 模拟网络与密码校验耗时
    await new Promise((r) => setTimeout(r, 420))

    const u = username.trim()
    const p = password
    if (!u || !p) {
      set({ loading: false, error: '请输入账号与密码' })
      return false
    }
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

  // 演示直通：以内置账号直接登录（免输入；密码用当前生效密码，改过也能进）
  loginDirect: () => {
    const u = USERS[0]
    return get().login(u.username, u.password, true)
  },

  changePassword: async (current, next) => {
    // 模拟网络与旧密码校验耗时
    await new Promise((r) => setTimeout(r, 420))

    if (!current || !next) return { ok: false, error: '请填写当前密码与新密码' }
    if (current !== currentPassword()) return { ok: false, error: '当前密码不正确' }
    if (next.length < 8) return { ok: false, error: '新密码至少 8 位' }
    if (current === next) return { ok: false, error: '新密码不能与当前密码相同' }

    USERS[0].password = next
    try {
      localStorage.setItem(LS_PWD, next)
    } catch {
      /* 存储不可用时仅内存生效（本标签页内有效） */
    }
    return { ok: true }
  },

  logout: () => {
    clearAll()
    set({ user: null, loggedInAt: null, expiresAt: null, error: null, loading: false })
  },

  clearError: () => set({ error: null }),
}))
