#!/usr/bin/env node
/**
 * 前端产物一致性自检（发布前必跑）。
 *
 * 存在理由（2026-11 实测事故链）：
 *   1. `web/vite.config.ts` 的 `build.emptyOutDir: false` 是为了保住 `dist/.gitkeep`
 *      ——`web/embed.go` 用 `//go:embed all:dist`，编译期必须有该目录。
 *   2. 代价：每次构建都会**累积旧 hash 产物**（实测残留 index-DgR_0AHX / index-d13GkUfi / index-De7vOXHX 三份）。
 *   3. 只要 `dist/index.html` 与实际 assets 不匹配（例如构建中断、手工拷贝、只更新了部分文件），
 *      浏览器就会 404 拿不到入口 JS → **纯白/纯黑屏，且控制台只有一条资源 404**，极难自查。
 *   4. 更隐蔽的一层：`web/embed.go` 是**编译期嵌入**。只跑 `vite build` 而不重新 `go build`，
 *      服务端仍会吐旧前端（本轮事故里用户跑的正是 10-29 的旧二进制 + 旧前端）。
 *
 * 本脚本做两件事（只读校验，不改任何文件）：
 *   A. `index.html` 引用的每个 js/css 必须在 dist 中真实存在；
 *   B. 报告孤儿产物（存在但无人引用的 index-xxx / vendor-xxx 文件），提示手动清理。
 *
 * 用法：node web/scripts/verify-dist.mjs
 * 退出码：0 = 一致；1 = 存在断链（必须修复后再发布）
 */
import { readFileSync, readdirSync, existsSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const distDir = resolve(here, '..', 'dist')
const indexPath = join(distDir, 'index.html')

if (!existsSync(indexPath)) {
  console.error(`[verify-dist] ✗ 未找到 ${indexPath}，请先在 web/ 下执行 npm run build`)
  process.exit(1)
}

const html = readFileSync(indexPath, 'utf8')

// 抓取 <script src>、<link href> 中以 /assets/ 开头的本地资源
const refs = new Set()
for (const m of html.matchAll(/(?:src|href)="(\/assets\/[^"]+)"/g)) {
  refs.add(m[1])
}
// manifest / sw / 图标等根路径资源也纳入检查（若存在引用）
for (const m of html.matchAll(/(?:src|href)="(\/(?!assets\/)[^"]+\.(?:js|css|webmanifest|svg))"/g)) {
  refs.add(m[1])
}

const missing = []
for (const ref of refs) {
  const rel = ref.replace(/^\//, '')
  if (!existsSync(join(distDir, rel))) missing.push(ref)
}

console.log(`[verify-dist] index.html 引用资源 ${refs.size} 个`)
for (const ref of [...refs].sort()) console.log(`  ✓ ${ref}`)

if (missing.length > 0) {
  console.error('[verify-dist] ✗ 以下引用在 dist 中不存在（会导致黑屏/白屏）：')
  for (const m of missing) console.error(`    缺失: ${m}`)
  process.exit(1)
}

// ---- 孤儿产物检测（只报告，不删除：删除策略应由人确认）----
const assetsDir = join(distDir, 'assets')
const orphans = []
if (existsSync(assetsDir)) {
  const referenced = new Set([...refs].map((r) => r.replace(/^\/assets\//, '')))
  for (const f of readdirSync(assetsDir)) {
    // 只对带 hash 的构建产物做孤儿判断，避免误报手工放置的文件
    if (!/-[A-Za-z0-9_-]{8,}\.(js|css)$/.test(f)) continue
    if (!referenced.has(f)) orphans.push(f)
  }
}

if (orphans.length > 0) {
  console.warn(`[verify-dist] ⚠ 发现 ${orphans.length} 个未被引用的历史产物（建议删除以免混淆排查）：`)
  for (const o of orphans.sort()) console.warn(`    孤儿: assets/${o}`)
} else {
  console.log('[verify-dist] ✓ 无孤儿产物')
}

console.log('[verify-dist] ✓ 产物一致')
console.log('[verify-dist] 提醒：前端产物是**编译期**经 //go:embed 嵌入的，改完前端必须重新 go build 才生效')
