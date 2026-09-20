/**
 * 生成绑定的【唯一】接触点。
 *
 * 生成命令（需要在仓库根目录执行）：
 *
 *   wails3 generate bindings -f "-tags wails" -clean ./cmd/swarmlink-gui/
 *
 * 产物在 frontend/bindings/ 下（已提交入库，因此前端可以脱离 Go 工具链构建）。
 * 若生成目录结构变化，只需要改本文件的 import 路径。
 *
 * 注意：绑定被【静态】导入，这样 Vite 会连同 @wailsio/runtime 一起打包；
 * 若改用运行时动态 import，产物里的相对路径会在打包后失效。
 * 「不在 Wails 宿主里」的情况由 backendAvailable() 探测并降级到 mock。
 */
import {
  ChatService,
  GroupService,
  PeerService,
  SettingsService,
  TransferService
} from '../../bindings/github.com/swarmlink/swarmlink/internal/adapters/wails/index.js'

import type { SwarmApi } from './types'

/** 真实后端实现（由 Wails 生成的服务绑定驱动）。 */
export const realApi: SwarmApi = {
  getSettings: async () => SettingsService.Get(),
  saveSettings: async (s) => SettingsService.Save(s),
  listInterfaces: async () => SettingsService.ListInterfaces(),
  downloadDir: async () => SettingsService.DownloadDir(),

  sendMessage: async (peerId, text) => ChatService.SendMessage(peerId, text),
  history: async (peerId, limit, before) => ChatService.History(peerId, limit, before),
  conversations: async () => ChatService.Conversations(),

  sendFile: async (peerId, path) => TransferService.SendFile(peerId, path),
  transfers: async () => TransferService.List(),

  self: async () => PeerService.Self(),
  peerList: async () => PeerService.List(),
  diagnostics: async () => PeerService.Diagnostics(),

  groupList: async () => GroupService.List(),
  groupCreate: async (name, memberIds) => GroupService.Create(name, memberIds),
  groupAddMember: async (groupId, memberId) => GroupService.AddMember(groupId, memberId),
  groupSendMessage: async (groupId, text) => GroupService.SendMessage(groupId, text),
  groupHistory: async (groupId, limit, before) => GroupService.History(groupId, limit, before)
}

/**
 * 探测后端是否真的在。
 *
 * 生成的绑定在普通浏览器里调用会抛错（没有 Wails runtime），
 * 因此用一次最轻量的调用做探测，据此决定是否降级到 mock。
 * 只在启动时执行一次。
 */
export async function backendAvailable(): Promise<boolean> {
  try {
    await SettingsService.Get()
    return true
  } catch {
    return false
  }
}
