import { createApp } from 'vue'
import { createPinia } from 'pinia'

import App from './App.vue'
import PinWindow from './views/PinWindow.vue'
import ScreenshotOverlay from './views/ScreenshotOverlay.vue'
// 字体先于样式：@font-face 必须在任何用到它的规则之前完成声明
import './fonts.css'
import './style.css'
import { registerEvents } from './events'

/**
 * 窗口分流。
 *
 * 同一个前端产物会跑在三种窗口里：
 *   - 主窗口（聊天界面）          —— 无 `mode` 参数
 *   - 覆盖层窗口（选区与标注层）  —— `/?mode=screenshot&display=<序号>`
 *   - 贴图窗口（桌面置顶的图）    —— `/?mode=pin&id=<贴图 id>`
 *
 * 分流靠 URL 上的 `mode` 参数。为什么在这里分流、而不是在 App.vue 里
 * v-if：覆盖层与贴图窗口不需要聊天界面的任何东西（联系人、传输列表、
 * 事件订阅）。少挂一层，少注册一批事件监听，也避免它们收到「新消息」
 * 这类事件后去更新一个根本不存在的会话。
 */
const overlayMode = new URLSearchParams(window.location.search).get('mode')

const root =
  overlayMode === 'screenshot' ? ScreenshotOverlay : overlayMode === 'pin' ? PinWindow : App

const app = createApp(root)
app.use(createPinia())

// 事件订阅【集中注册】——不要散落在各组件里，
// 否则组件卸载时忘记退订就会泄漏并重复更新（P1-10 的另一半）。
//
// 只有主窗口订阅：覆盖层只跟截图会话打交道，贴图窗口只跟自己的图打交道。
if (!overlayMode) {
  registerEvents()
}

app.mount('#app')
