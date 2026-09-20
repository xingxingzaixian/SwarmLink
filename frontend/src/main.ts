import { createApp } from 'vue'
import { createPinia } from 'pinia'

import App from './App.vue'
import './style.css'
import { registerEvents } from './events'

const app = createApp(App)
app.use(createPinia())

// 事件订阅【集中注册】——不要散落在各组件里，
// 否则组件卸载时忘记退订就会泄漏并重复更新（P1-10 的另一半）。
registerEvents()

app.mount('#app')
