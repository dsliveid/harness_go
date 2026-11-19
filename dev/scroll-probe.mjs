#!/usr/bin/env node
/**
 * 页面最外层滚动条回归探针（CDP 直连，无额外 npm 依赖）。
 *
 * 存在原因（2026-11 实测事故）：.app-shell 用 100dvh、html/body 用 100% 时是**两套高度源**。
 * 768px 平板断点下顶栏横向溢出 45px → 出现横向滚动条 → 吃掉 6px 可用高度 →
 * 100dvh 比 100% 链高 6px → body.scrollHeight > clientHeight → 最外层凭空长出竖向滚动条。
 * 该现象只在特定视口宽度 + 有横向溢出的组合下出现，人工点检极易漏判，故固化为脚本。
 *
 * 断言（每档视口）：
 *   1. docHasVScroll=false            文档级不出现竖向滚动条
 *   2. vhShortBy=0                    可用高度没被横向滚动条吃掉
 *   3. wideOutCount=0                 无越出视口左右边的非滚动区元素（横向溢出元凶）
 *   4. 内部滚动容器 scrollable=true   文档级锁死后，滚动确实由内部容器承担（不丢内容）
 *
 * 用法：node --experimental-websocket dev/scroll-probe.mjs
 * 环境变量：PROBE_BASE（默认 http://127.0.0.1:5611）、HARNESS_USER、HARNESS_PWD、
 *           PROBE_W / PROBE_H（登录后首屏视口）、PROBE_DEBUG_PORT
 */
import { spawn, spawnSync } from 'node:child_process'
import { existsSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'

const BASE = process.env.PROBE_BASE || 'http://127.0.0.1:5611'
const USER = process.env.HARNESS_USER || 'admin'
const PWD = process.env.HARNESS_PWD || ''
const DEBUG_PORT = Number(process.env.PROBE_DEBUG_PORT || 9444)
const W = Number(process.env.PROBE_W || 1440)
const H = Number(process.env.PROBE_H || 900)

const VIEWPORTS = [
  { w: 360, h: 640, mobile: true },
  { w: 414, h: 800, mobile: true },
  { w: 768, h: 1024, mobile: false },
  { w: 1280, h: 720, mobile: false },
  { w: 1440, h: 900, mobile: false },
  { w: 1440, h: 420, mobile: false },
  { w: 1024, h: 300, mobile: false },
  { w: 1920, h: 1080, mobile: false },
]

const BROWSER_CANDIDATES = [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
]

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

class CDP {
  constructor(ws) {
    this.ws = ws
    this.id = 0
    this.pending = new Map()
    ws.addEventListener('message', (ev) => {
      let msg
      try {
        msg = JSON.parse(ev.data)
      } catch {
        return
      }
      if (msg.id && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id)
        this.pending.delete(msg.id)
        msg.error ? reject(new Error(msg.error.message)) : resolve(msg.result)
      }
    })
  }
  static async connect(wsUrl) {
    const ws = new WebSocket(wsUrl)
    await new Promise((resolve, reject) => {
      ws.addEventListener('open', resolve, { once: true })
      ws.addEventListener('error', () => reject(new Error('ws fail')), { once: true })
    })
    return new CDP(ws)
  }
  send(method, params = {}) {
    const id = ++this.id
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      this.ws.send(JSON.stringify({ id, method, params }))
    })
  }
  close() {
    try {
      this.ws.close()
    } catch {}
  }
}

const PROBE = `(() => {
  const de = document.documentElement
  const vh = de.clientHeight
  const vw = de.clientWidth
  const desc = (el) => ({
    tag: el.tagName.toLowerCase(),
    cls: (el.getAttribute('class') || '').slice(0, 90),
    top: Math.round(el.getBoundingClientRect().top),
    bottom: Math.round(el.getBoundingClientRect().bottom),
    left: Math.round(el.getBoundingClientRect().left),
    right: Math.round(el.getBoundingClientRect().right),
    h: Math.round(el.getBoundingClientRect().height),
  })
  // 找出「自身底部越出视口底边」的可见元素（排除 fixed 与滚动容器内部）
  const beyond = []
  // 横向越界元素（真正造成横向滚动条的元凶）
  const wideOut = []
  const inHScroll = (el) => {
    for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) {
      const ox = getComputedStyle(p).overflowX
      if (ox === 'auto' || ox === 'scroll') return true
    }
    return false
  }
  for (const el of document.querySelectorAll('body *')) {
    const cs = getComputedStyle(el)
    if (cs.display === 'none' || cs.visibility === 'hidden') continue
    const r = el.getBoundingClientRect()
    if (r.width === 0 || r.height === 0) continue
    if (cs.position !== 'fixed' && r.bottom > vh + 1) beyond.push({ ...desc(el), pos: cs.position, of: cs.overflowY })
    if ((r.right > vw + 1 || r.left < -1) && !inHScroll(el) && cs.position !== 'fixed') {
      wideOut.push({ ...desc(el), ow: el.scrollWidth, cw: el.clientWidth, ofx: cs.overflowX })
    }
  }
  beyond.sort((a, b) => b.bottom - a.bottom)
  wideOut.sort((a, b) => b.right - a.right)
  // 内部滚动容器可用性：文档级已 overflow:hidden，滚动必须由内部容器承担。
  // 逐个把 scrollTop 设为 80，看是否真的能滚（不能滚则说明用户会「看不到下面的内容」）。
  const scrollers = []
  for (const el of document.querySelectorAll('body *')) {
    const cs = getComputedStyle(el)
    if (!/^(auto|scroll)$/.test(cs.overflowY)) continue
    const over = el.scrollHeight - el.clientHeight
    if (over <= 4) continue
    const before = el.scrollTop
    el.scrollTop = 80
    const applied = el.scrollTop > before
    el.scrollTop = before
    scrollers.push({ cls: (el.getAttribute('class') || '').slice(0, 60), over, scrollable: applied })
  }
  return JSON.stringify({
    innerHeight: window.innerHeight,
    innerWidth: window.innerWidth,
    docClientHeight: de.clientHeight,
    docScrollHeight: de.scrollHeight,
    docClientWidth: vw,
    docScrollWidth: de.scrollWidth,
    bodyClientHeight: document.body.clientHeight,
    bodyScrollHeight: document.body.scrollHeight,
    bodyHasVScroll: document.body.scrollHeight > document.body.clientHeight + 1,
    docHasVScroll: de.scrollHeight > de.clientHeight + 1,
    /** 关键判据：可用高度是否被横向滚动条吃掉 */
    vhShortBy: window.innerHeight - de.clientHeight,
    rootH: document.getElementById('root')?.getBoundingClientRect().height,
    shellH: document.querySelector('.app-shell')?.getBoundingClientRect().height,
    bodyOverflowY: getComputedStyle(document.body).overflowY,
    htmlOverflowY: getComputedStyle(de).overflowY,
    bodyStyleH: getComputedStyle(document.body).height,
    beyondCount: beyond.length,
    beyond: beyond.slice(0, 5),
    wideOutCount: wideOut.length,
    wideOut: wideOut.slice(0, 6),
    scrollers,
  })
})()`

async function evaluate(cdp, expression) {
  const { result } = await cdp.send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
  return result.value
}

async function waitFor(cdp, expr, timeoutMs = 12000, stepMs = 150) {
  const dl = Date.now() + timeoutMs
  while (Date.now() < dl) {
    if (await evaluate(cdp, expr)) return true
    await sleep(stepMs)
  }
  return false
}

const exe = BROWSER_CANDIDATES.find((p) => existsSync(p))
if (!exe) {
  console.log('FAIL no chromium')
  process.exit(1)
}
const profile = mkdtempSync(path.join(tmpdir(), 'probe-cdp-'))
const child = spawn(
  exe,
  [
    '--headless=new',
    '--disable-gpu',
    '--no-first-run',
    '--no-default-browser-check',
    `--user-data-dir=${profile}`,
    `--remote-debugging-port=${DEBUG_PORT}`,
    'about:blank',
  ],
  { stdio: 'ignore', windowsHide: true },
)

let cdp
try {
  let version = null
  for (let i = 0; i < 80; i++) {
    try {
      version = await (await fetch(`http://127.0.0.1:${DEBUG_PORT}/json/version`)).json()
      break
    } catch {
      await sleep(150)
    }
  }
  if (!version) throw new Error('devtools not ready')
  const created = await (await fetch(`http://127.0.0.1:${DEBUG_PORT}/json/new?about:blank`, { method: 'PUT' })).json()
  cdp = await CDP.connect(created.webSocketDebuggerUrl)
  await cdp.send('Page.enable')
  await cdp.send('Runtime.enable')

  // 登录页单独在矮视口下探一次：验证「文档级 overflow:hidden 后登录页自身仍可滚动」
  for (const lv of [{ w: 900, h: 380 }, { w: 1440, h: 900 }]) {
    await cdp.send('Emulation.setDeviceMetricsOverride', { width: lv.w, height: lv.h, deviceScaleFactor: 1, mobile: false })
    await cdp.send('Page.navigate', { url: `${BASE}/?probe=${lv.h}` })
    await sleep(2000)
    const r = JSON.parse(await evaluate(cdp, PROBE))
    const loginScroller = (r.scrollers || []).find((s) => s.scrollable)
    console.log(
      `=== 登录页 ${lv.w}x${lv.h} === docVScroll=${r.docHasVScroll} wideOut=${r.wideOutCount} 可滚动内部容器=${loginScroller ? 'yes(over=' + loginScroller.over + ')' : 'no'}`,
    )
  }

  await cdp.send('Emulation.setDeviceMetricsOverride', { width: W, height: H, deviceScaleFactor: 1, mobile: false })

  const url = `${BASE}/`
  await cdp.send('Page.navigate', { url })
  await sleep(2500)

  // 表单登录（React 受控组件：必须走原生 setter + input 事件，否则 state 不更新）
  const logged = await evaluate(
    cdp,
    `(() => {
      const setVal = (el, v) => {
        const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set
        setter.call(el, v)
        el.dispatchEvent(new Event('input', { bubbles: true }))
      }
      const inputs = [...document.querySelectorAll('input')]
      const pwd = inputs.find((i) => i.type === 'password')
      const usr = inputs.find((i) => i.type !== 'password')
      if (!usr || !pwd) return 'no-inputs'
      setVal(usr, ${JSON.stringify(USER)})
      setVal(pwd, ${JSON.stringify(PWD)})
      const btn = [...document.querySelectorAll('button')].find((b) => (b.textContent || '').includes('进入工作台'))
      if (!btn) return 'no-button'
      btn.click()
      return 'clicked'
    })()`,
  )
  console.log(`login-action: ${logged}`)
  await sleep(1500)

  const ok = await waitFor(cdp, `!!document.querySelector('.app-shell')`, 12000)
  const fails = []
  if (!ok) {
    console.log('FAIL 未能进入工作台（app-shell 缺失，无法判定布局）')
    fails.push('boot: app-shell missing')
  } else {
    await sleep(2000)
    for (const vp of VIEWPORTS) {
      await cdp.send('Emulation.setDeviceMetricsOverride', {
        width: vp.w,
        height: vp.h,
        deviceScaleFactor: 1,
        mobile: vp.mobile,
      })
      await sleep(700)
      const r = JSON.parse(await evaluate(cdp, PROBE))
      const tag = `${vp.w}x${vp.h}`
      const lines = []
      const check = (name, cond, detail) => {
        lines.push(`  ${cond ? 'PASS' : 'FAIL'} ${tag}/${name} ${detail}`)
        if (!cond) fails.push(`${tag}/${name}: ${detail}`)
      }
      check('no-doc-vscroll', r.docHasVScroll === false, `docScrollHeight=${r.docScrollHeight} docClientHeight=${r.docClientHeight}`)
      check('no-vh-short', r.vhShortBy === 0, `innerHeight-clientHeight=${r.vhShortBy}`)
      check('no-h-overflow', r.wideOutCount === 0, r.wideOutCount ? JSON.stringify(r.wideOut.slice(0, 3)) : 'none')
      // 有内容时必须有可滚动的内部容器（否则内容被裁切且无法滚动查看）
      const hasContent = r.beyondCount > 0
      const anyScrollable = (r.scrollers || []).some((s) => s.scrollable)
      check('inner-scroll', !hasContent || anyScrollable, hasContent ? `scrollers=${(r.scrollers || []).length}` : 'no-overflow-content')
      lines.forEach((l) => console.log(l))
    }
  }

  console.log('')
  if (fails.length === 0) {
    console.log('SCROLL_PROBE PASS')
  } else {
    console.log(`SCROLL_PROBE FAIL (${fails.length} issue(s))`)
    fails.forEach((f) => console.log(`  - ${f}`))
    process.exitCode = 1
  }
} catch (err) {
  console.log(`FAIL ${err.message}`)
} finally {
  try {
    await cdp?.send('Browser.close')
  } catch {}
  cdp?.close()
  await sleep(300)
  try {
    if (process.platform === 'win32') spawnSync('taskkill', ['/PID', String(child.pid), '/T', '/F'], { stdio: 'ignore' })
    else child.kill('SIGKILL')
  } catch {}
  try {
    rmSync(profile, { recursive: true, force: true })
  } catch {}
}
// 保留上面判定的退出码（process.exit(0) 会把 SCROLL_PROBE FAIL 的失败信号吞掉）
process.exit(process.exitCode ?? 0)
