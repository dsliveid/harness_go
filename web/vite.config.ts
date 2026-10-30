import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  // 单二进制内嵌部署：前端由 Go 从根路径提供，故用绝对 base（/assets/*）
  base: '/',
  plugins: [react()],
  server: { port: 5612, strictPort: false },
  // emptyOutDir: false —— dist 内含 .gitkeep 占位文件（git 只跟踪该文件，产物本身不入库），
  // 默认的清空行为会把它删掉，导致干净检出后 //go:embed all:dist 找不到目录而编译失败
  build: { outDir: 'dist', emptyOutDir: false, sourcemap: false },
})
