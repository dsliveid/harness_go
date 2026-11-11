/**
 * 浮标定位几何自测（`web/src/lib/fabdrag.ts` 的纯函数）。
 *
 * ## 为什么是脚本而不是单测
 *
 * 本项目前端没有测试运行器（package.json 只有 build/preview），引入 vitest 会
 * 拉一整套依赖。这里改用已在 node_modules 里的 esbuild（vite 的依赖）把 TS
 * 源码即时转成 ESM 再断言，零新增依赖、毫秒级跑完。
 *
 * 运行：`node scripts/verify-fabdrag.mjs`（或 `npm run verify:fab`）
 *
 * ## 断言的都是「错了会明显不对」的语义
 *
 * 吸附方向、夹取边界、比例往返、SideRail 让位 —— 这些一旦算错，用户看到的是
 * 「按钮飞到屏幕外 / 贴着底边挡住操作 / 吸反了边」，都是难以用肉眼在小屏幕上
 * 立刻发现的回归。
 */
import { build } from 'esbuild'
import { pathToFileURL } from 'node:url'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const outDir = mkdtempSync(join(tmpdir(), 'fabdrag-'))
const outFile = join(outDir, 'fabDrag.mjs')

await build({
  entryPoints: ['src/lib/fabDrag.ts'],
  outfile: outFile,
  bundle: true,
  format: 'esm',
  platform: 'neutral',
  logLevel: 'silent',
})

const m = await import(pathToFileURL(outFile).href)
rmSync(outDir, { recursive: true, force: true })

let failures = 0
function check(name, actual, expected) {
  const a = JSON.stringify(actual)
  const e = JSON.stringify(expected)
  if (a !== e) {
    failures++
    console.error(`  ✗ ${name}\n      实际: ${a}\n      期望: ${e}`)
  } else {
    console.log(`  ✓ ${name}`)
  }
}

const S = m.FAB_SIZE
const G = m.EDGE_GAP

console.log('[verify-fabdrag] 常量与安全内距')
check('FAB_SIZE 与组件 w-9 一致', S, 36)
check('EDGE_GAP > 0（左缘手势区留白）', G, 12)

console.log('\n[verify-fabdrag] 默认位：右侧、上下居中')
const def = m.defaultFabPos(1000, 800)
check('贴右边', def.x, 1000 - S - G)
check('垂直居中', def.y, (800 - S) / 2)
const tinyDef = m.defaultFabPos(10, 10)
check('视口比浮标还小时不出现负坐标', tinyDef.x >= 0 && tinyDef.y >= 0, true)

console.log('\n[verify-fabdrag] 夹取：绝不跑出视口')
check(
  '左上越界回夹到 EDGE_GAP',
  m.clampFabPos({ x: -500, y: -500 }, 1000, 800),
  { x: G, y: G },
)
check(
  '右下越界回夹到内边界',
  m.clampFabPos({ x: 5000, y: 5000 }, 1000, 800),
  { x: 1000 - S - G, y: 800 - S - G },
)

console.log('\n[verify-fabdrag] 吸附：按中心点吸最近的左右边')
check('左侧松手 → 吸左边缘，y 保留', m.snapFabPos({ x: 100, y: 300 }, 1000, 800), { x: G, y: 300 })
check('右侧松手 → 吸右边缘，y 保留', m.snapFabPos({ x: 900, y: 300 }, 1000, 800), {
  x: 1000 - S - G,
  y: 300,
})
// 跨越中线即换边（严格大于中线才吸右，等于中线仍算左）
check('中心略偏左 → 左', m.snapFabPos({ x: 480, y: 100 }, 1000, 800).x, G)
check('中心略偏右 → 右', m.snapFabPos({ x: 520, y: 100 }, 1000, 800).x, 1000 - S - G)
check('吸附后 y 越界也回夹', m.snapFabPos({ x: 900, y: 9999 }, 1000, 800).y, 800 - S - G)

console.log('\n[verify-fabdrag] 避让右侧编排胶囊栏')
const rail = { top: 300, right: 1000, bottom: 420, left: 900 }
const overlapping = m.snapFabPos({ x: 900, y: 320 }, 1000, 800)
const avoided = m.avoidSideRail(overlapping, rail, 1000, 800)
check('竖向相交时下移让开', avoided.y >= rail.bottom, true)
check('x 保持贴右', avoided.x, 1000 - S - G)
check(
  '不相交时不动（用户左侧选择不被干扰）',
  m.avoidSideRail({ x: G, y: 320 }, rail, 1000, 800),
  { x: G, y: 320 },
)
check('没有胶囊栏时不动', m.avoidSideRail({ x: 900, y: 320 }, null, 1000, 800), {
  x: 900,
  y: 320,
})
// 下方塞不下（胶囊栏贴到底）→ 上移
const lowRail = { top: 700, right: 1000, bottom: 800, left: 900 }
const up = m.avoidSideRail(m.snapFabPos({ x: 900, y: 760 }, 1000, 800), lowRail, 1000, 800)
check('下方无空间时上移让开', up.y + S <= lowRail.top, true)

console.log('\n[verify-fabdrag] 持久化：归一化比例往返')
const p1 = m.fabPosToPersisted({ x: 1000 - S - G, y: 382 }, 1000, 800)
check('右侧归一化', p1.side, 'right')
check('中心比例 ≈ 0.5', Math.abs(p1.ry - 0.5) < 1e-9, true)
const back = m.fabPosFromPersisted(p1, 1000, 800)
check('往返回到同一像素位', back, { x: 1000 - S - G, y: (800 - S) / 2 })
const pLeft = m.fabPosToPersisted({ x: G, y: 0 }, 1000, 800)
check('左侧归一化', pLeft.side, 'left')
// 小视口下比例换算会算出负 y，必须被夹取回视口内（这正是夹取存在的意义）。
const small = m.fabPosFromPersisted(pLeft, 500, 400)
check('换分辨率后仍在视口内', small.y >= G && small.y <= 400 - S - G, true)

console.log('\n[verify-fabdrag] 持久化解析：坏数据一律回落 null')
check('空串', m.parsePersistedFabPos(''), null)
check('非法 JSON', m.parsePersistedFabPos('{oops'), null)
check('side 非法', m.parsePersistedFabPos('{"side":"middle","ry":0.5}'), null)
check('ry 越界', m.parsePersistedFabPos('{"side":"left","ry":1.5}'), null)
check('ry NaN', m.parsePersistedFabPos('{"side":"left","ry":"x"}'), null)
check('合法值', m.parsePersistedFabPos('{"side":"left","ry":0.25}'), { side: 'left', ry: 0.25 })

if (failures > 0) {
  console.error(`\n[verify-fabdrag] ✗ ${failures} 项未通过`)
  process.exit(1)
}
console.log('\n[verify-fabdrag] ✓ 全部通过')
