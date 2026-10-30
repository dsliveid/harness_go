import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  // 单二进制内嵌部署：前端由 Go 从根路径提供，故用绝对 base（/assets/*）
  base: '/',
  plugins: [react()],
  server: { port: 5612, strictPort: false },
  build: { outDir: 'dist', sourcemap: false },
})
