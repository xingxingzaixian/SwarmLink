import { ref } from 'vue'

import { isMockMode } from '../api'
import { pickFiles } from '../api/dialog'
import { useChatStore } from '../stores/chat'
import { useTransferStore } from '../stores/transfer'
import { useUiStore } from '../stores/ui'

/**
 * 「把文件发给当前会话对象」这一动作的唯一实现。
 *
 * 为什么抽成 composable：触发入口有两个 ——
 * 聊天工具栏的回形针，和右栏资料卡上的「发送文件」。两处若各写一遍，
 * 「切右栏 → 发文件 → 提示」这条链路迟早会分叉（比如只在一处切了面板）。
 */
export function useFileSend() {
  const chat = useChatStore()
  const transfers = useTransferStore()
  const ui = useUiStore()

  const sending = ref(false)

  async function sendPaths(paths: string[]): Promise<void> {
    const peerId = chat.activePeerId
    if (!peerId || !paths.length) return

    // 先切面板再发起：否则首个进度事件可能早于面板切换，看起来像没反应
    ui.focusTransfer()
    sending.value = true
    try {
      const ok = await transfers.sendFiles(peerId, paths)
      if (ok === paths.length) {
        ui.notify(paths.length === 1 ? '已开始发送' : `已开始发送 ${ok} 个文件`)
      } else {
        // 后端现在会把「路径不存在 / 是目录 / 空文件」同步报回来，要透传给用户
        ui.notify(transfers.error || `已发起 ${ok}/${paths.length} 个文件`)
      }
    } finally {
      sending.value = false
    }
  }

  /** 附件按钮：优先原生对话框；不可用时打开手动填路径的兜底输入。 */
  async function attach(): Promise<void> {
    if (!chat.activePeerId) {
      ui.notify('先打开与某个联系人的会话')
      return
    }
    const res = await pickFiles()
    if (!res.available) {
      ui.showPathInput()
      ui.notify(
        isMockMode()
          ? '浏览器预览没有原生对话框，请手动填写文件路径'
          : '无法打开文件对话框，请手动填写路径'
      )
      return
    }
    if (!res.picked) return // 用户点了取消
    await sendPaths(res.paths)
  }

  return { sending, sendPaths, attach }
}
