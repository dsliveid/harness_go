import { defineConfig } from 'vitest/config'

/**
 * 前端单测配置（vitest）。
 *
 * # 为什么单独建文件而不是塞进 vite.config.ts
 *
 * `vite.config.ts` 被 `tsconfig.node.json` 以 composite 方式纳入 `tsc -b` 门禁，
 * 一旦在其中 import `vitest/config`，就把「测试依赖」变成了「构建门禁的依赖」——
 * 生产构建（`npm run build`）在未安装 devDependencies 的环境（如仅装
 * `--omit=dev` 的部署机）会直接失败。测试配置必须与生产构建配置解耦。
 *
 * # 环境选择
 *
 * `jsdom` 而非 `happy-dom`：本项目测试要碰 `document.addEventListener`
 * （sessionEvents.ts 的 visibilitychange 回前台重连）与 `EventSource` 替身，
 * jsdom 对这两者的行为更贴近浏览器。
 *
 * # include 范围
 *
 * 只收 `src/**\/*.test.ts`。排除 `node_modules` 与 `dist` 是默认行为，
 * 但这里显式写出 exclude 是为了防止未来有人把测试文件放到 `scripts/` 下
 * 被意外拾取（scripts 是 Node 侧脚本，跑在 Node 环境而非 jsdom）。
 */
export default defineConfig({
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
    exclude: ['node_modules/**', 'dist/**', 'scripts/**'],
    // 不开启 globals：显式 import { describe, it, expect } 可让编辑器与
    // `tsc --noEmit` 无需额外注入 @types 就能类型检查（少一个全局声明文件）。
    globals: false,
    // 单测不产出覆盖率报告（CI 上另跑），避免每次本地测试都拖慢
    reporters: 'default',
  },
})
