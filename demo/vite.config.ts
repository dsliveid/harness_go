import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  base: './',
  plugins: [react()],
  server: { port: 5610, strictPort: false },
  build: { outDir: 'dist', sourcemap: false },
})
