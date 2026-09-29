import { ref } from 'vue'

import { isMockMode } from '../api'
import { pickFiles, pickImages } from '../api/dialog'
import type { Conversation } from '../api/types'
import { useChatStore } from '../stores/chat'
import { useTransferStore } from '../stores/transfer'
import { useUiStore } from '../stores/ui'
import { baseName } from '../utils/format'

/** sendFilesWithMessages 的结果：成功数 + 逐个失败原因。 */
export interface SendFilesResult {
  sent: number
  errors: string[]
}

/**
 * 「把一个或多个文件发给某个会话对象」这一动作的**唯一**实现。
 *
 * 必须是一次调用完成两件事：
 *   1. 让 TransferApp 真正开始传输（transfers.sendFile）；
 *   2. 往会话里插一条文件消息，让【两端】的聊天框都出现文件卡片。
 *
 * 为什么强调「唯一」：这条链路曾经被两个入口各写一遍 —— 回形针那处调了
 * 第 2 步，拖放那处只调了第 1 步。于是拖进去的文件只出现在右栏传输列表里，
 * 聊天框静悄悄的，用户以为文件没发出去。
 *
 * 导出成模块级函数（而不是只留在 composable 里）就是为了让拖放这类
 * 非组件场景也能走同一条路 —— 下一个入口不必再记得「还要发消息」。
 */
export async function sendFilesWithMessages(
  conv: Conversation,
  paths: string[]
): Promise<SendFilesResult> {
  const chat = useChatStore()
  const transfers = useTransferStore()

  const errors: string[] = []
  let sent = 0

  for (const path of paths) {
    try {
      const jobId = await transfers.sendFile(conv.peerId ?? '', path)
      sent++

      // 文件消息单独兜异常：传输已经跑起来了，卡片插不进去是「少一条记录」，
      // 不该反过来把这次传输报成失败（那会让用户重复发送）。
      try {
        await chat.sendFile(conv, jobId, baseName(path))
      } catch (e: any) {
        errors.push(`文件消息未显示: ${baseName(path)}（${String(e?.message ?? e)}）`)
      }
    } catch (e: any) {
      errors.push(String(e?.message ?? e) || `发送文件失败: ${baseName(path)}`)
    }
  }
  return { sent, errors }
}

/**
 * 「把文件发给当前会话对象」这一动作的唯一实现。
 *
 * 为什么抽成 composable：触发入口有两个 ——
 * 聊天工具栏的回形针，和右栏资料卡上的「发送文件」。两处若各写一遍，
 * 「切右栏 → 发文件 → 提示」这条链路迟早会分叉（比如只在一处切了面板）。
 */
export function useFileSend() {
  const chat = useChatStore()
  const ui = useUiStore()

  const sending = ref(false)

  async function sendPaths(paths: string[]): Promise<void> {
    if (!paths.length) return

    // 没有会话就【必须】说出来。这里曾经是静默 return：用户点回形针、
    // 选完文件，界面毫无反应，也没有任何提示 —— 看起来就像功能坏了。
    const conv = chat.activeConversation
    if (!conv?.peerId) {
      ui.notify('先打开与某个联系人的会话，再发送文件')
      return
    }

    // 先切面板再发起：否则首个进度事件可能早于面板切换，看起来像没反应
    ui.focusTransfer()
    sending.value = true
    try {
      const { sent, errors } = await sendFilesWithMessages(conv, paths)
      for (const msg of errors) ui.notify(msg)
      if (sent > 0) {
        ui.notify(sent === 1 ? '已开始发送' : `已开始发送 ${sent} 个文件`)
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
