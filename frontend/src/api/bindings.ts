/**
 * 生成绑定的【唯一】接触点。
 *
 * 生成命令（需要在仓库根目录执行）：
 *
 *   wails3 generate bindings -f "-tags wails" -clean .
 *
 * 产物在 frontend/bindings/ 下（已提交入库，因此前端可以脱离 Go 工具链构建）。
 * 若生成目录结构变化，只需要改本文件的 import 路径。
 *
 * 注意：绑定被【静态】导入，这样 Vite 会连同 @wailsio/runtime 一起打包；
 * 若改用运行时动态 import，产物里的相对路径会在打包后失效。
 * 「不在 Wails 宿主里」的情况由 backendAvailable() 探测并降级到 mock。
 *
 * ---------------------------------------------------------------------------
 * 为什么这里有一层 normalize：
 *
 * Go 的 slice 字段（`[]string`、`[]PeerDTO` …）在绑定生成时会被标成
 * `T[] | null` —— 因为 Go 的 nil slice 序列化成 JSON null，生成器无法区分
 * 「空集合」与「没有值」。但 UI 侧把 null 当空数组处理才是正确语义
 * （「没有发现的节点」和「列表为空」是同一件事）。
 *
 * 因此这里统一在边界处收敛 null，让 stores / 组件只面对非空数组，
 * 免去每一处渲染都要写 `v-for="x in (list || [])"`。
 * ---------------------------------------------------------------------------
 */
import {
  ChatService,
  GroupService,
  PeerService,
  SettingsService,
  TransferService
} from '../../bindings/github.com/swarmlink/swarmlink/internal/adapters/wails/index.js'
import type {
  DiagnosticsDTO,
  GroupDTO,
  SelfCheckDTO,
  SettingsDTO
} from '../../bindings/github.com/swarmlink/swarmlink/internal/adapters/wails/models.js'

import type { Diagnostics, Group, SelfCheck, Settings, SwarmApi } from './types'

/** 把绑定的可空数组收敛成空数组。 */
function list<T>(v: T[] | null | undefined): T[] {
  return v ?? []
}

function toSettings(s: SettingsDTO): Settings {
  return {
    ...s,
    allowInterfaces: list(s.allowInterfaces),
    denyInterfaces: list(s.denyInterfaces),
    seeds: list(s.seeds)
  }
}

function toGroup(g: GroupDTO): Group {
  return { ...g, members: list(g.members) }
}

function toDiagnostics(d: DiagnosticsDTO): Diagnostics {
  return { ...d, seedDetails: list(d.seedDetails), interfaceSummary: list(d.interfaceSummary) }
}

function toSelfCheck(s: SelfCheckDTO): SelfCheck {
  return { ...s, seeds: list(s.seeds) }
}

/** 真实后端实现（由 Wails 生成的服务绑定驱动）。 */
export const realApi: SwarmApi = {
  getSettings: async () => toSettings(await SettingsService.Get()),
  saveSettings: async (s) => list(await SettingsService.Save(s)),
  listInterfaces: async () => list(await SettingsService.ListInterfaces()),
  downloadDir: async () => SettingsService.DownloadDir(),
  runSelfCheck: async () => toSelfCheck(await SettingsService.RunSelfCheck()),

  sendMessage: async (peerId, text) => ChatService.SendMessage(peerId, text),
  history: async (peerId, limit, before) => list(await ChatService.History(peerId, limit, before)),
  conversations: async () => list(await ChatService.Conversations()),
  sendImage: async (peerId, path) => ChatService.SendImage(peerId, path),
  sendImageBytes: async (peerId, name, dataB64) =>
    ChatService.SendImageBytes(peerId, name, dataB64),
  saveImage: async (msgId, destPath) => {
    await ChatService.SaveImage(msgId, destPath)
  },

  sendFile: async (peerId, path) => TransferService.SendFile(peerId, path),
  transfers: async () => list(await TransferService.List()),
  revealFile: async (path) => {
    await TransferService.Reveal(path)
  },
  clearTransfers: async () => TransferService.ClearFinished(),

  self: async () => PeerService.Self(),
  peerList: async () => list(await PeerService.List()),
  diagnostics: async () => toDiagnostics(await PeerService.Diagnostics()),

  groupList: async () => list(await GroupService.List()).map(toGroup),
  groupCreate: async (name, memberIds) => toGroup(await GroupService.Create(name, memberIds)),
  groupAddMember: async (groupId, memberId) =>
    toGroup(await GroupService.AddMember(groupId, memberId)),
  groupSendMessage: async (groupId, text) => GroupService.SendMessage(groupId, text),
  groupHistory: async (groupId, limit, before) =>
    list(await GroupService.History(groupId, limit, before)),
  groupSendImage: async (groupId, path) => GroupService.SendImage(groupId, path),
  groupSendImageBytes: async (groupId, name, dataB64) =>
    GroupService.SendImageBytes(groupId, name, dataB64)
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
