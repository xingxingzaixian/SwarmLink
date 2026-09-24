import { ref } from 'vue'

import { isMockMode } from '../api'
import { pickFiles, pickImages } from '../api/dialog'
import { useChatStore } from '../stores/chat'
import { useTransferStore } from '../stores/transfer'
import { useUiStore } from '../stores/ui'
import { baseName } from '../utils/format'

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
      const jobIds: string[] = []
      for (const path of paths) {
        try {
          const jobId = await transfers.sendFile(peerId, path)
          jobIds.push(jobId)
          
          // 发送文件消息到聊天窗口
          const conv = chat.activeConversation
          if (conv) {
            await chat.sendFile(conv, jobId, baseName(path))
          }
        } catch (e: any) {
          ui.notify(String(e?.message ?? e) || `发送文件失败: ${baseName(path)}`)
        }
      }
      
      if (jobIds.length > 0) {
        ui.notify(jobIds.length === 1 ? '已开始发送' : `已开始发送 ${jobIds.length} 个文件`)
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

  /**
   * 发送一张图片。
   *
   * 刻意【不】调用 ui.focusTransfer()：图片走消息链路、不产生传输任务，
   * 把右栏切到传输面板只会让用户以为发错了地方。
   */
  async function sendImage(path: string): Promise<boolean> {
    const conv = chat.activeConversation
    if (!conv) {
      ui.notify('先打开一个会话')
      return false
    }
    try {
      await chat.sendImage(conv, path)
      return true
    } catch (e: any) {
      ui.notify(String(e?.message ?? e) || '图片发送失败')
      return false
    }
  }

  /** 图片按钮：选图片文件后立即发送（一次可多选，串行发）。 */
  async function attachImage(): Promise<void> {
    if (!chat.activeConversation) {
      ui.notify('先打开一个会话')
      return
    }
    const res = await pickImages()
    if (!res.available) {
      ui.notify('当前环境无法打开文件对话框')
      return
    }
    if (!res.picked) return // 用户点了取消
    for (const p of res.paths) await sendImage(p)
  }

  return { sending, sendPaths, attach, sendImage, attachImage }
}
