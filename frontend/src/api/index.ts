import { loadBindings } from './bindings'
import { createMockApi } from './mock'
import type { SwarmApi } from './types'

export * from './types'

let cached: SwarmApi | null = null
let usingMock = false

/**
 * 返回后端 API。优先使用 Wails 生成的绑定；不可用时降级为本地 mock，
 * 使前端在没有 Go 工具链的环境里也能构建与预览。
 */
export async function getApi(): Promise<SwarmApi> {
  if (cached) return cached
  const real = await loadBindings()
  if (real) {
    cached = real
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
