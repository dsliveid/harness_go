import { useEffect, useState } from 'react'

export interface PwaState {
  supported: boolean
  registered: boolean
  offline: boolean
  updateReady: boolean
  installAvailable: boolean
}

const initial: PwaState = {
  supported: false,
  registered: false,
  offline: false,
  updateReady: false,
  installAvailable: false,
}

let state: PwaState = { ...initial }
const listeners = new Set<(s: PwaState) => void>()

let waitingWorker: ServiceWorker | null = null
let deferredPrompt: (Event & { prompt: () => Promise<void>; userChoice: Promise<{ outcome: string }> }) | null = null
let initialized = false

function emit(patch: Partial<PwaState>) {
  state = { ...state, ...patch }
  listeners.forEach((fn) => fn(state))
}

export function subscribePwa(fn: (s: PwaState) => void): () => void {
  listeners.add(fn)
  fn(state)
  return () => listeners.delete(fn)
}

export function getPwaState(): PwaState {
  return state
}

export async function initPwa(): Promise<void> {
  if (initialized) return
  initialized = true

  const updateOnline = () => emit({ offline: !navigator.onLine })
  window.addEventListener('online', updateOnline)
  window.addEventListener('offline', updateOnline)

  window.addEventListener('beforeinstallprompt', (e) => {
    e.preventDefault()
    deferredPrompt = e as typeof deferredPrompt
    emit({ installAvailable: true })
  })
  window.addEventListener('appinstalled', () => {
    deferredPrompt = null
    emit({ installAvailable: false })
  })

  emit({ offline: !navigator.onLine })

  if (!('serviceWorker' in navigator)) {
    emit({ supported: false })
    return
  }
  emit({ supported: true })

  // 开发模式不注册 SW，避免缓存干扰 HMR
  if (!import.meta.env.PROD) return

  try {
    const reg = await navigator.serviceWorker.register('./sw.js', { scope: './' })
    emit({ registered: true })

    if (reg.waiting) {
      waitingWorker = reg.waiting
      emit({ updateReady: true })
    }

    reg.addEventListener('updatefound', () => {
      const installing = reg.installing
      if (!installing) return
      installing.addEventListener('statechange', () => {
        if (installing.state === 'installed' && navigator.serviceWorker.controller) {
          waitingWorker = installing
          emit({ updateReady: true })
        }
      })
    })

    navigator.serviceWorker.addEventListener('controllerchange', () => {
      emit({ updateReady: false })
    })
  } catch {
    emit({ registered: false })
  }
}

export function applyUpdate(): void {
  if (waitingWorker) {
    waitingWorker.postMessage('SKIP_WAITING')
    setTimeout(() => window.location.reload(), 240)
  } else {
    window.location.reload()
  }
}

export async function promptInstall(): Promise<boolean> {
  if (!deferredPrompt) return false
  await deferredPrompt.prompt()
  const choice = await deferredPrompt.userChoice
  deferredPrompt = null
  emit({ installAvailable: false })
  return choice.outcome === 'accepted'
}

/** React hook：订阅 PWA 状态 */
export function usePwa(): PwaState {
  const [s, setS] = useState<PwaState>(state)
  useEffect(() => {
    const off = subscribePwa(setS)
    return () => {
      off()
    }
  }, [])
  return s
}
