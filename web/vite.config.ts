import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

/**
 * 后端地址：`--web`（vite dev server）只负责前端热更新，
 * 真实接口通过 dev proxy 转发给后端，避免跨端口 CORS / Cookie 丢失。
 * 可用环境变量 HARNESS_BACKEND 覆盖（如 http://127.0.0.1:8080）。
 *
 * 注意：这里读的是 Node 的 process.env（vite.config 运行在 Node 侧，不进浏览器包），
 * 加上 declare 是为了在无 @types/node 的纯前端工程里也能通过 tsc -b 的类型检查。
 */
declare const process: { env: Record<string, string | undefined }
const BACKEND = process.env.HARNESS_BACKEND || 'http://127.0.0.1:8080'

export default defineConfig({
  // 单二进制内嵌部署：前端由 Go 从根路径提供，故用绝对 base（/assets/*）
  base: '/',
  plugins: [react()],
  server: {
    port: 5612,
    strictPort: false,
    // dev 联调必备：把接口与健康检查转发给后端（否则请求会打到 vite 自己，404）
    proxy: {
      '/api': { target: BACKEND, changeOrigin: false },
      '/healthz': { target: BACKEND, changeOrigin: false },
    },
  },
  // emptyOutDir: false —— dist 内含 .gitkeep 占位文件（git 只跟踪该文件，产物本身不入库），
  // 默认的清空行为会把它删掉，导致干净检出后 //go:embed all:dist 找不到目录而编译失败
  build: { outDir: 'dist', emptyOutDir: false, sourcemap: false },
})
