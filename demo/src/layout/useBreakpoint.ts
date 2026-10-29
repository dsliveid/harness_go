import { useEffect } from 'react'
import { useUI } from '../stores/uiStore'

/** 基于 matchMedia 的断点监听（非 resize 轮询），写入 uiStore */
export function useBreakpoint() {
  const setBreakpoint = useUI((s) => s.setBreakpoint)

  useEffect(() => {
    // 支持 ?bp=mobile|tablet|desktop 强制断点，便于三端走查
    const forced = new URLSearchParams(window.location.search).get('bp')
    if (forced === 'mobile' || forced === 'tablet' || forced === 'desktop') {
      setBreakpoint(forced)
      return
    }
    const mobile = window.matchMedia('(max-width: 639px)')
    const tablet = window.matchMedia('(max-width: 1023px)')
    const update = () =>
      setBreakpoint(mobile.matches ? 'mobile' : tablet.matches ? 'tablet' : 'desktop')
    update()
    mobile.addEventListener('change', update)
    tablet.addEventListener('change', update)
    return () => {
      mobile.removeEventListener('change', update)
      tablet.removeEventListener('change', update)
    }
  }, [setBreakpoint])
}
