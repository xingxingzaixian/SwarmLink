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
    strictPort: true,
    // 必须显式绑 IPv4 回环，不能让 host 留在默认的 'localhost'。
    // Node 17+ 在 Windows 上把 localhost 优先解析成 ::1，Vite 于是只监听 [::1]；
    // 而 Wails dev 的 asset server 连的是 127.0.0.1:9245 —— 请求被拒后转成
    // HTTP 502 吐给窗口，终端里 Vite 却一切正常，排查时极容易被带偏。
    // 诊断方法：curl 127.0.0.1:9245 返回 000、curl [::1]:9245 返回 200，即是此症。
    host: '127.0.0.1'
  }
})
