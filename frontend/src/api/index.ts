import { backendAvailable, realApi } from './bindings'
import { createMockApi } from './mock'
import type { SwarmApi } from './types'

export * from './types'

let cached: SwarmApi | null = null
let usingMock = false

/**
 * 返回后端 API。
 *
 * 优先使用 Wails 生成的绑定；若当前不在 Wails 宿主里（例如 `npm run dev`
 * 单独调样式，或 CI 里只跑构建），则降级为本地 mock，让前端仍可运行与构建。
 */
export async function getApi(): Promise<SwarmApi> {
  if (cached) return cached

  if (await backendAvailable()) {
    cached = realApi
    usingMock = false
  } else {
    cached = createMockApi()
    usingMock = true
  }
  return cached
}

/** 当前是否运行在降级模式（UI 应据此给出提示）。 */
export function isMockMode(): boolean {
  return usingMock
}

/** 同步读取已加载的 API；未加载时返回 null（供事件回调使用）。 */
export function api(): SwarmApi | null {
  return cached
}
