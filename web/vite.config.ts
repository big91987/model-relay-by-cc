import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 开发时把 /api 与 /v1 代理到本地网关（默认 :3000）
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': process.env.GATEWAY_URL || 'http://localhost:3000',
      '/v1': process.env.GATEWAY_URL || 'http://localhost:3000',
    },
  },
  build: { outDir: 'dist', emptyOutDir: true },
})
