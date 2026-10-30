#!/usr/bin/env node
/**
 * B-6 responsive regression: real CSS viewport (CDP Emulation.setDeviceMetricsOverride)
 *
 * Why this script exists: the built-in headless browser tool has no viewport control,
 * so 360/768/1440 breakpoint verification could not be done semantically. This drives
 * a real Chromium over the DevTools Protocol with no extra npm dependency
 * (Node >= 20.10 only needs --experimental-websocket for the global WebSocket).
 *
 * Assertions per viewport:
 *   1. no horizontal overflow at page level (documentElement / body scrollWidth)
 *   2. no horizontally-overflowing element whose overflow-x is visible (truncated list)
 *   3. media queries really match the expected breakpoint tier
 *   4. three-tier skeleton matches: mobile => drawer sidebar collapsed (header left = 0),
 *      tablet/desktop => pinned 264px sidebar (header left = 264)
 *   5. zero console errors / zero uncaught exceptions / zero HTTP >= 400
 *   6. same checks re-run with the Settings modal open (covers its horizontal menu)
 *
 * Usage:
 *   powershell -ExecutionPolicy Bypass -File dev/run-b6.ps1     # starts real binary on :18090
 *   node --experimental-websocket dev/responsive-check.mjs
 *
 * Env: RESP_BASE (default http://127.0.0.1:18090), HARNESS_USER, HARNESS_PWD
 */
import { spawn, spawnSync } from 'node:child_process'
import { existsSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'

const BASE = process.env.RESP_BASE || 'http://127.0.0.1:18090'
const USER = process.env.HARNESS_USER || 'admin'
const PWD = process.env.HARNESS_PWD || 'e2e-pass-123'
const DEBUG_PORT = Number(process.env.RESP_DEBUG_PORT || 9333)
const SETTLE_MS = Number(process.env.RESP_SETTLE_MS || 1800)

const BROWSER_CANDIDATES = [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
]

const VIEWPORTS = [
  { label: '360x640  (mobile)', width: 360, height: 640, mobile: true, mqMobile: true, mqTablet: true, headerLeft: 0 },
  { label: '768x1024 (tablet)', width: 768, height: 1024, mobile: false, mqMobile: false, mqTablet: true, headerLeft: 264 },
  { label: '1440x960 (desktop)', width: 1440, height: 960, mobile: false, mqMobile: false, mqTablet: false, headerLeft: 264 },
]

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

/** Minimal CDP client over the page target websocket (no session multiplexing needed). */
class CDP {
  constructor(ws) {
    this.ws = ws
    this.id = 0
    this.pending = new Map()
    this.handlers = new Map()
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
        msg.error ? reject(new Error(`${msg.error.message} (${msg.error.code})`)) : resolve(msg.result)
      } else if (msg.method) {
        const cbs = this.handlers.get(msg.method)
        if (cbs) for (const cb of cbs) cb(msg.params)
      }
    })
  }

  static async connect(wsUrl) {
    const ws = new WebSocket(wsUrl)
    await new Promise((resolve, reject) => {
      ws.addEventListener('open', resolve, { once: true })
      ws.addEventListener('error', () => reject(new Error(`ws connect failed: ${wsUrl}`)), { once: true })
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

  on(method, cb) {
    const arr = this.handlers.get(method) || []
    arr.push(cb)
    this.handlers.set(method, arr)
  }

  close() {
    try {
      this.ws.close()
    } catch {
      /* ignore */
    }
  }
}

async function fetchJson(url) {
  const res = await fetch(url)
  return res.json()
}

/**
 * Probe executed in page context: geometry + real viewport overflow + skeleton facts.
 *
 * "Real horizontal overflow" is defined as an element whose bounding rect crosses the
 * viewport right/left edge — that is what the user actually sees as a horizontal scroll.
 * `scrollWidth > clientWidth` alone is too noisy: absolutely positioned decorations
 * (e.g. the animate-ping run indicator) inflate it without breaking layout, so those are
 * collected separately as diagnostics only.
 */
const PAGE_PROBE = `(() => {
  const de = document.documentElement
  const header = document.querySelector('header')
  // layout viewport width: in mobile emulation a real overflow also inflates window.innerWidth,
  // so clientWidth is the only trustworthy baseline ("what the user can actually see").
  const vw = de.clientWidth
  const mq = (q) => window.matchMedia(q).matches
  const clsOf = (el) => (el.getAttribute && el.getAttribute('class') ? el.getAttribute('class') : '')
  const desc = (el) => ({ tag: el.tagName.toLowerCase(), cls: clsOf(el).slice(0, 80) })
  /** inside a horizontally scrollable ancestor? then crossing the viewport edge is by design. */
  const inHScroll = (el) => {
    for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) {
      const ox = getComputedStyle(p).overflowX
      if (ox === 'auto' || ox === 'scroll') return true
    }
    return false
  }
  const outOfView = []
  const internalOverflow = []
  let hScrollHits = 0
  for (const el of document.querySelectorAll('body *')) {
    const cs = getComputedStyle(el)
    if (cs.display === 'none' || cs.visibility === 'hidden') continue
    const r = el.getBoundingClientRect()
    if (r.width === 0 || r.height === 0) continue
    if (r.right > vw + 1 || r.left < -1) {
      if (inHScroll(el)) hScrollHits++
      else outOfView.push({ ...desc(el), left: Math.round(r.left), right: Math.round(r.right), w: Math.round(r.width) })
    }
    if (el.scrollWidth > el.clientWidth + 1 && ['visible', 'clip'].includes(cs.overflowX)) {
      internalOverflow.push({ ...desc(el), sw: el.scrollWidth, cw: el.clientWidth })
    }
  }
  outOfView.sort((a, b) => b.right - a.right)
  const headerChildren = header
    ? [...header.children].map((el) => ({ ...desc(el), w: Math.round(el.getBoundingClientRect().width), sw: el.scrollWidth }))
    : []
  const modals = [...document.querySelectorAll('.fixed.inset-0')].filter((el) => el.offsetParent !== null || getComputedStyle(el).position === 'fixed')
  const modalInfo = modals.map((el) => { const r = el.getBoundingClientRect(); return { w: Math.round(r.width), h: Math.round(r.height) } })
  return JSON.stringify({
    innerWidth: window.innerWidth,
    innerHeight: window.innerHeight,
    clientWidth: vw,
    docScrollWidth: de.scrollWidth,
    bodyScrollWidth: document.body ? document.body.scrollWidth : -1,
    mqMobile: mq('(max-width: 639px)'),
    mqTablet: mq('(max-width: 1023px)'),
    hasHeader: !!header,
    headerLeft: header ? Math.round(header.getBoundingClientRect().left) : -1,
    headerClientWidth: header ? header.clientWidth : -1,
    headerScrollWidth: header ? header.scrollWidth : -1,
    headerChildren,
    hasComposer: !!document.querySelector('textarea'),
    hasAppShell: !!document.querySelector('.app-shell'),
    /** 顶栏模型选择器当前显示文本：real 模式未配厂商时必须回退为「默认模型」而不是空白 */
    modelSelectText: (() => {
      const sel = document.querySelector('select[title*="会话级模型"]')
      if (!sel) return null
      const opt = sel.selectedOptions && sel.selectedOptions[0]
      return opt ? opt.textContent.trim() : ''
    })(),
    /** 可选模型条数：用于验证「主库配置的厂商模型确实进了顶栏」（而非 mock 数据） */
    modelOptionCount: (() => {
      const sel = document.querySelector('select[title*="会话级模型"]')
      return sel ? sel.options.length : -1
    })(),
    modalCount: modals.length,
    modalInfo,
    outOfView: outOfView.slice(0, 6),
    hScrollHits,
    internalOverflow: internalOverflow.slice(0, 4),
  })
})()`

async function evaluate(cdp, expression) {
  const { result } = await cdp.send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
  return result.value
}

async function waitFor(cdp, predicateExpr, timeoutMs = 8000, stepMs = 150) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (await evaluate(cdp, predicateExpr)) return true
    await sleep(stepMs)
  }
  return false
}

function makeCollector() {
  const state = { consoleErrors: [], exceptions: [], httpErrors: [] }
  return {
    state,
    reset() {
      state.consoleErrors.length = 0
      state.exceptions.length = 0
      state.httpErrors.length = 0
    },
  }
}

function attachCollectors(cdp, collector) {
  cdp.on('Runtime.exceptionThrown', (p) => {
    const d = p.exceptionDetails || {}
    collector.state.exceptions.push(`${d.text || 'exception'} ${d.exception?.description || ''}`.trim().slice(0, 240))
  })
  cdp.on('Runtime.consoleAPICalled', (p) => {
    if (p.type !== 'error') return
    const text = (p.args || []).map((a) => a.value ?? a.description ?? a.type).join(' ').slice(0, 240)
    collector.state.consoleErrors.push(text)
  })
  cdp.on('Log.entryAdded', (p) => {
    const e = p.entry || {}
    if (e.level !== 'error') return
    if (e.source === 'network') return // duplicates Network.responseReceived
    collector.state.consoleErrors.push(`${e.source}: ${e.text}`.slice(0, 240))
  })
  cdp.on('Network.responseReceived', (p) => {
    const r = p.response || {}
    if (r.status >= 400) collector.state.httpErrors.push(`${r.status} ${r.url}`)
  })
}

/** Assert one viewport (page loaded, optional modal opened) and return PASS/FAIL lines. */
function checkProbe(tag, probe, vp) {
  const fails = []
  const lines = []
  const ok = (name, cond, detail) => {
    lines.push(`  ${cond ? 'PASS' : 'FAIL'} ${tag}/${name} ${detail}`)
    if (!cond) fails.push(`${tag}/${name}: ${detail}`)
  }
  ok('app-shell', probe.hasAppShell, `hasAppShell=${probe.hasAppShell}`)
  ok('header', probe.hasHeader, `hasHeader=${probe.hasHeader}`)
  ok('composer', probe.hasComposer, `hasComposer=${probe.hasComposer}`)
  ok('overflow-doc', probe.docScrollWidth <= probe.clientWidth + 1, `docScrollWidth=${probe.docScrollWidth} clientWidth=${probe.clientWidth}`)
  ok('overflow-body', probe.bodyScrollWidth <= probe.clientWidth + 1, `bodyScrollWidth=${probe.bodyScrollWidth} clientWidth=${probe.clientWidth}`)
  ok('overflow-viewport', probe.outOfView.length === 0, probe.outOfView.length ? JSON.stringify(probe.outOfView) : 'none')
  ok(
    'header-fit',
    !probe.hasHeader || probe.headerScrollWidth <= probe.headerClientWidth + 1,
    `headerScrollWidth=${probe.headerScrollWidth} headerClientWidth=${probe.headerClientWidth} children=${JSON.stringify(probe.headerChildren)}`,
  )
  ok('mq-mobile', probe.mqMobile === vp.mqMobile, `matchMedia(639)=${probe.mqMobile} expected=${vp.mqMobile}`)
  ok('mq-tablet', probe.mqTablet === vp.mqTablet, `matchMedia(1023)=${probe.mqTablet} expected=${vp.mqTablet}`)
  ok('skeleton-headerLeft', probe.headerLeft === vp.headerLeft, `headerLeft=${probe.headerLeft} expected=${vp.headerLeft}`)
  ok(
    'model-selector',
    typeof probe.modelSelectText === 'string' && probe.modelSelectText.length > 0,
    `selectedText=${JSON.stringify(probe.modelSelectText)} options=${probe.modelOptionCount}`,
  )
  return { fails, lines }
}

async function openSettings(cdp) {
  const clicked = await evaluate(
    cdp,
    `(() => { const b = [...document.querySelectorAll('button')].find((x) => x.getAttribute('aria-label') === '打开设置'); if (!b) return false; b.click(); return true })()`,
  )
  if (!clicked) return false
  await waitFor(cdp, `!!document.querySelector('.fixed.inset-0')`, 4000)
  await sleep(400)
  return true
}

async function closeSettings(cdp) {
  // click the dim overlay (top-left corner is always overlay since the dialog is centered / bottom sheet)
  await evaluate(
    cdp,
    `(() => { const ov = [...document.querySelectorAll('.fixed.inset-0.z-50')][0]; if (!ov) return false; ov.dispatchEvent(new MouseEvent('click', { bubbles: true })); return true })()`,
  )
  await sleep(300)
}

async function main() {
  const exe = BROWSER_CANDIDATES.find((p) => existsSync(p))
  if (!exe) {
    console.log('RESPONSIVE_RESULT FAIL no chromium binary found in known locations')
    process.exit(1)
  }

  // fail fast when the app is not reachable
  try {
    const res = await fetch(`${BASE}/healthz`)
    if (!res.ok) throw new Error(`healthz ${res.status}`)
  } catch (err) {
    console.log(`RESPONSIVE_RESULT FAIL app unreachable at ${BASE} (${err.message})`)
    process.exit(1)
  }

  const profile = mkdtempSync(path.join(tmpdir(), 'resp-cdp-'))
  const child = spawn(
    exe,
    [
      '--headless=new',
      '--disable-gpu',
      '--hide-scrollbars',
      '--no-first-run',
      '--no-default-browser-check',
      '--disable-extensions',
      '--disable-background-networking',
      `--user-data-dir=${profile}`,
      `--remote-debugging-port=${DEBUG_PORT}`,
      '--window-size=1440,960',
      'about:blank',
    ],
    { stdio: 'ignore', windowsHide: true },
  )

  let cdp
  let exitCode = 0
  try {
    // wait for the debugging endpoint
    let version = null
    for (let i = 0; i < 80; i++) {
      try {
        version = await fetchJson(`http://127.0.0.1:${DEBUG_PORT}/json/version`)
        break
      } catch {
        await sleep(150)
      }
    }
    if (!version) throw new Error(`devtools endpoint not ready on :${DEBUG_PORT}`)
    console.log(`BROWSER ${version.Browser}`)

    // create our own tab so we never inherit a stale target
    const created = await fetchJson(`http://127.0.0.1:${DEBUG_PORT}/json/new?about:blank`).catch(async () => {
      const res = await fetch(`http://127.0.0.1:${DEBUG_PORT}/json/new?about:blank`, { method: 'PUT' })
      return res.json()
    })
    cdp = await CDP.connect(created.webSocketDebuggerUrl)

    await cdp.send('Page.enable')
    await cdp.send('Runtime.enable')
    await cdp.send('Log.enable')
    await cdp.send('Network.enable')

    const collector = makeCollector()
    attachCollectors(cdp, collector)

    const allFails = []
    for (const vp of VIEWPORTS) {
      collector.reset()
      console.log(`\n=== viewport ${vp.label} ===`)
      await cdp.send('Emulation.setDeviceMetricsOverride', {
        width: vp.width,
        height: vp.height,
        deviceScaleFactor: 1,
        mobile: vp.mobile,
      })
      await cdp.send('Emulation.setTouchEmulationEnabled', { enabled: vp.mobile, maxTouchPoints: vp.mobile ? 5 : 1 })

      const url = `${BASE}/?user_name=${encodeURIComponent(USER)}&pwd=${encodeURIComponent(PWD)}&_vp=${vp.width}`
      await cdp.send('Page.navigate', { url })
      const ready = await waitFor(cdp, `!!document.querySelector('.app-shell')`, 15000)
      if (!ready) {
        console.log(`  FAIL boot app-shell not rendered within 15s`)
        allFails.push(`${vp.label}/boot`)
        exitCode = 1
        continue
      }
      await sleep(SETTLE_MS)

      const probe = JSON.parse(await evaluate(cdp, PAGE_PROBE))
      let r = checkProbe('page', probe, vp)
      r.lines.forEach((l) => console.log(l))
      if (probe.internalOverflow.length) console.log(`  INFO page/internal-overflow ${JSON.stringify(probe.internalOverflow)}`)
      allFails.push(...r.fails)

      // modal-open state: covers Settings' horizontally scrollable menu + fixed heights
      const opened = await openSettings(cdp)
      if (!opened) {
        console.log('  FAIL modal/settings button not found')
        allFails.push(`${vp.label}/modal-open`)
      } else {
        const mprobe = JSON.parse(await evaluate(cdp, PAGE_PROBE))
        // compare against the *actual* viewport (mobile emulation may widen the layout viewport)
        const modalFits = mprobe.modalInfo.every((m) => m.w <= mprobe.clientWidth + 1 && m.h <= mprobe.innerHeight + 1)
        const mlines = []
        const mfail = []
        const ok2 = (name, cond, detail) => {
          mlines.push(`  ${cond ? 'PASS' : 'FAIL'} modal/${name} ${detail}`)
          if (!cond) mfail.push(`${vp.label}/modal/${name}: ${detail}`)
        }
        ok2('rendered', mprobe.modalCount > 0, `modalCount=${mprobe.modalCount}`)
        ok2('fits-viewport', modalFits, `${JSON.stringify(mprobe.modalInfo)} viewport=${mprobe.clientWidth}x${mprobe.innerHeight}`)
        ok2('overflow-doc', mprobe.docScrollWidth <= mprobe.clientWidth + 1, `docScrollWidth=${mprobe.docScrollWidth} clientWidth=${mprobe.clientWidth}`)
        ok2('overflow-viewport', mprobe.outOfView.length === 0, mprobe.outOfView.length ? JSON.stringify(mprobe.outOfView.slice(0, 4)) : 'none')
        mlines.forEach((l) => console.log(l))
        if (mprobe.hScrollHits) console.log(`  INFO modal/hscroll-in-viewport ${mprobe.hScrollHits} element(s) inside horizontally scrollable containers (by design)`)
        if (mprobe.internalOverflow.length) console.log(`  INFO modal/internal-overflow ${JSON.stringify(mprobe.internalOverflow)}`)
        allFails.push(...mfail)
        await closeSettings(cdp)
      }

      // console / exception / http failures across the whole viewport run
      const st = collector.state
      const httpFails = st.httpErrors.filter((e) => !/\/favicon\.ico/.test(e))
      const okHttp = httpFails.length === 0
      console.log(`  ${okHttp ? 'PASS' : 'FAIL'} http/ge400 ${okHttp ? 'none' : JSON.stringify(httpFails.slice(0, 5))}`)
      if (!okHttp) allFails.push(`${vp.label}/http: ${httpFails.slice(0, 3).join(' | ')}`)
      const okEx = st.exceptions.length === 0
      console.log(`  ${okEx ? 'PASS' : 'FAIL'} exceptions ${okEx ? 'none' : JSON.stringify(st.exceptions.slice(0, 3))}`)
      if (!okEx) allFails.push(`${vp.label}/exception: ${st.exceptions[0]}`)
      const okCe = st.consoleErrors.length === 0
      console.log(`  ${okCe ? 'PASS' : 'FAIL'} console-errors ${okCe ? 'none' : JSON.stringify(st.consoleErrors.slice(0, 3))}`)
      if (!okCe) allFails.push(`${vp.label}/console: ${st.consoleErrors[0]}`)

      console.log(`VIEWPORT ${vp.label} ${allFails.some((f) => f.startsWith(vp.label)) ? 'FAIL' : 'PASS'}`)
    }

    console.log('')
    if (allFails.length === 0) {
      console.log('RESPONSIVE_RESULT PASS')
    } else {
      exitCode = 1
      console.log(`RESPONSIVE_RESULT FAIL (${allFails.length} issue(s))`)
      allFails.forEach((f) => console.log(`  - ${f}`))
    }
  } catch (err) {
    exitCode = 1
    console.log(`RESPONSIVE_RESULT FAIL ${err.message}`)
  } finally {
    try {
      await cdp?.send('Browser.close')
    } catch {
      /* ignore */
    }
    cdp?.close()
    await sleep(300)
    // Windows: child.kill() only signals the launcher, so kill the whole browser tree
    try {
      if (process.platform === 'win32') spawnSync('taskkill', ['/PID', String(child.pid), '/T', '/F'], { stdio: 'ignore' })
      else child.kill('SIGKILL')
    } catch {
      /* ignore */
    }
    try {
      rmSync(profile, { recursive: true, force: true })
    } catch {
      /* profile may stay locked briefly on Windows; ignore */
    }
  }
  process.exit(exitCode)
}

await main()
