import { useEffect, useState } from 'react'
import { Sparkles, Eye, EyeOff, LogIn, Sun, Moon, User, Lock, Zap } from 'lucide-react'
import { Button, Switch, inputCls, cn } from '../../components/ui'
import { useAuth } from '../../stores/authStore'
import { useUI } from '../../stores/uiStore'
import { apiMode } from '../../lib/api'

/**
 * 极简自托管登录界面：
 * - 紧凑居中单卡片设计，干净克制，去除冗余宣传与繁杂说明
 * - 纯粹 1px 精密线框，全站无多余阴影
 * - 顶部集成深浅主题无缝切换
 * - 提供便捷的一键填入演示账号快捷操作
 */
export function LoginPage() {
  const { login, loginDirect, ssoLogin, loading, error, clearError } = useAuth()
  const { theme, toggleTheme } = useUI()
  // 演示入口仅在 mock 模式出现（real 模式账号由服务端引导创建）
  const isDemo = apiMode === 'mock'

  // SSO 兜底自愈：dev / 纯静态部署时服务端不拦截 GET /，由前端读 query 换取 Cookie，
  // 并立刻把凭据从地址栏抹掉（不留浏览器历史、不被 Referer 带出）
  useEffect(() => {
    const params = new URLSearchParams(window.location.search)
    const userName = params.get('user_name')
    if (!userName) return
    const pwd = params.get('pwd') ?? undefined
    for (const k of ['user_name', 'pwd', 'ts', 'sign']) params.delete(k)
    const qs = params.toString()
    window.history.replaceState(
      {},
      '',
      `${window.location.pathname}${qs ? `?${qs}` : ''}${window.location.hash}`,
    )
    void ssoLogin(userName, pwd)
  }, [ssoLogin])

  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [remember, setRemember] = useState(true)
  const [showPwd, setShowPwd] = useState(false)

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    await login(username, password, remember)
  }

  const fillDemo = () => {
    clearError()
    setUsername('admin')
    setPassword('harness123')
  }

  return (
    <div className="min-h-dvh w-full bg-bg text-ink flex flex-col justify-between p-4 sm:p-6 select-none">
      {/* 顶栏微型工具条：主题切换 */}
      <header className="w-full max-w-[420px] mx-auto flex items-center justify-end">
        <button
          type="button"
          onClick={toggleTheme}
          className="h-8 w-8 rounded-lg border border-edge bg-panel hover:bg-panel2 text-inkdim hover:text-ink flex items-center justify-center transition-colors"
          title={theme === 'dark' ? '切换为浅色主题' : '切换为深色主题'}
          aria-label="切换主题"
        >
          {theme === 'dark' ? <Moon size={14} /> : <Sun size={14} />}
        </button>
      </header>

      {/* 主体居中登录卡片 */}
      <main className="w-full max-w-[380px] mx-auto my-auto fade-in">
        <form
          onSubmit={submit}
          className="w-full rounded-2xl border border-edge bg-panel p-6 sm:p-7 flex flex-col gap-5"
        >
          {/* 品牌与标题 */}
          <div className="flex flex-col items-center text-center">
            <div className="w-10 h-10 rounded-xl bg-gradient-to-tr from-accent to-accent2 flex items-center justify-center text-white mb-3">
              <Sparkles size={18} />
            </div>
            <div className="flex items-center gap-2">
              <h1 className="text-[17px] font-semibold tracking-tight text-ink">harness_go</h1>
              <span className="px-1.5 py-0.5 rounded text-[10px] font-mono bg-panel2 text-inkfaint border border-edge">Web</span>
            </div>
            <p className="text-[12px] text-inkfaint mt-1">本地自托管 AI Agent 工作台</p>
          </div>

          {/* 表单输入域 */}
          <div className="space-y-3.5">
            <label className="block space-y-1.5">
              <span className="text-[12px] font-medium text-inkdim flex items-center gap-1.5">
                <User size={12} className="text-inkfaint" />
                <span>账号</span>
              </span>
              <input
                className={cn(inputCls, 'h-10 text-[13px] bg-panel2/50')}
                value={username}
                onChange={(e) => { setUsername(e.target.value); if (error) clearError() }}
                placeholder="admin"
                autoComplete="username"
                autoFocus
                disabled={loading}
              />
            </label>

            <label className="block space-y-1.5">
              <span className="text-[12px] font-medium text-inkdim flex items-center gap-1.5">
                <Lock size={12} className="text-inkfaint" />
                <span>密码</span>
              </span>
              <div className="relative">
                <input
                  className={cn(inputCls, 'h-10 pr-9 text-[13px] bg-panel2/50')}
                  type={showPwd ? 'text' : 'password'}
                  value={password}
                  onChange={(e) => { setPassword(e.target.value); if (error) clearError() }}
                  placeholder="••••••••"
                  autoComplete="current-password"
                  disabled={loading}
                />
                <button
                  type="button"
                  className="absolute right-1.5 top-1/2 -translate-y-1/2 w-7 h-7 rounded-md flex items-center justify-center text-inkfaint hover:text-ink hover:bg-panel2 transition-colors"
                  title={showPwd ? '隐藏密码' : '显示密码'}
                  onClick={() => setShowPwd((v) => !v)}
                  tabIndex={-1}
                >
                  {showPwd ? <EyeOff size={13} /> : <Eye size={13} />}
                </button>
              </div>
            </label>

            {/* 记住设备选项 */}
            <div className="flex items-center justify-between pt-0.5 text-[12px]">
              <label className="flex items-center gap-2 cursor-pointer select-none text-inkdim">
                <Switch checked={remember} onChange={setRemember} title="记住此设备" />
                <span>记住此设备（7 天）</span>
              </label>
            </div>

            {/* 错误提示 */}
            {error && (
              <div className="rounded-lg border border-err/40 bg-err/[0.08] px-3 py-2 text-[12px] text-err flex items-center gap-2">
                <span>{error}</span>
              </div>
            )}

            {/* 登录主操作按钮 */}
            <Button
              type="submit"
              variant="primary"
              className="w-full justify-center gap-1.5 h-10 text-[13px] font-medium mt-1"
              disabled={loading}
            >
              <LogIn size={14} />
              <span>{loading ? '正在验证…' : '进入工作台'}</span>
            </Button>

            {/* 演示直通 + 一键填入：仅 mock 演示模式（real 模式账号由服务端引导创建） */}
            {isDemo ? (
              <>
                <Button
                  type="button"
                  variant="outline"
                  className="w-full justify-center gap-1.5 h-9 text-[13px]"
                  disabled={loading}
                  title="演示模式：免输入账号密码，以内置账号直接进入工作台（仅 mock 数据模式可用）"
                  onClick={() => { clearError(); void loginDirect() }}
                >
                  <Zap size={13} className="text-warn" />
                  <span>直接登录（演示模式）</span>
                </Button>

                <div className="rounded-xl border border-edge bg-panel2/40 px-3 py-2 flex items-center justify-between text-[12px]">
                  <span className="text-inkdim">
                    演示账号：<code className="font-mono text-accent font-medium">admin</code>
                  </span>
                  <button
                    type="button"
                    className="text-accent hover:underline font-medium cursor-pointer"
                    onClick={fillDemo}
                  >
                    一键填入
                  </button>
                </div>
              </>
            ) : (
              <div className="rounded-xl border border-edge bg-panel2/40 px-3 py-2 text-[11px] text-inkfaint leading-relaxed">
                账号由服务端创建：首次启动见日志或{' '}
                <code className="font-mono text-accent">data/initial_password</code>
                <br />
                支持免登链接 <code className="font-mono text-inkdim">/?user_name=xxx&pwd=xxx</code>
              </div>
            )}
          </div>
        </form>
      </main>

      {/* 极简底栏 */}
      <footer className="w-full text-center text-[11px] text-inkfaint py-2">
        <span>harness_go · 本地安全自托管</span>
      </footer>
    </div>
  )
}
