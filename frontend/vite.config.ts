import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Wails 会把 frontend/dist 嵌入二进制，因此 outDir 固定为 dist。
export default defineConfig({
  plugins: [vue()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // Wails 的 WebView 版本可控，无需为老浏览器降级到 es5 体积
    target: 'es2020'
  },
  server: {
    port: 5173,
    strictPort: true
  }
})
