import { useEffect } from 'react'
import { AppShell } from './layout/AppShell'
import { LoginPage } from './features/auth/LoginPage'
import { useAuth } from './stores/authStore'

export default function App() {
  const user = useAuth((s) => s.user)
  const booted = useAuth((s) => s.booted)
  const boot = useAuth((s) => s.boot)

  // 启动静默探测：real 模式先问 GET /auth/me，Cookie 有效则直接进工作台（不闪登录页）
  useEffect(() => {
    if (!booted) void boot()
  }, [booted, boot])

  if (!booted) {
    return (
      <div className="min-h-dvh w-full bg-bg text-ink flex items-center justify-center">
        <div className="flex flex-col items-center gap-3 fade-in">
          <div className="w-9 h-9 rounded-xl bg-gradient-to-tr from-accent to-accent2 animate-pulse" />
          <span className="text-[12px] text-inkfaint">正在连接服务…</span>
        </div>
      </div>
    )
  }

  // 登录门禁：未登录只渲染登录页，工作台数据不在未认证状态下挂载
  return user ? <AppShell /> : <LoginPage />
}
