import { AppShell } from './layout/AppShell'
import { LoginPage } from './features/auth/LoginPage'
import { useAuth } from './stores/authStore'

export default function App() {
  const user = useAuth((s) => s.user)
  // 登录门禁：未登录只渲染登录页，工作台数据不在未认证状态下挂载
  return user ? <AppShell /> : <LoginPage />
}
