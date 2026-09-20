import type { SwarmApi } from './types'

type AnyFn = (...args: any[]) => any
type AnyMod = Record<string, any>

/**
 * 生成绑定的【唯一】接触点。
 *
 * wails3 generate bindings 会把 Go 服务导出到 frontend/bindings/<模块路径>/。
 * 不同 beta 版本的目录结构与导出命名存在差异，因此这里用「动态 import + 名字映射」
 * 隔离：一旦对不上，只需要改本文件，其余前端代码不受影响。
 */
export async function loadBindings(): Promise<SwarmApi | null> {
  // 路径写成变量：避免 Vite 在构建期静态解析（未生成绑定时会导致构建失败）。
  const modulePath = '../../bindings/swarmlink/internal/adapters/wails/index.js'
  try {
    const mod = (await import(/* @vite-ignore */ modulePath)) as AnyMod
    return adapt(mod)
  } catch {
    return null
  }
}

function adapt(mod: AnyMod): SwarmApi {
  const S = pick(mod, 'SettingsService')
  const C = pick(mod, 'ChatService')
  const T = pick(mod, 'TransferService')
  const P = pick(mod, 'PeerService')
  const G = pick(mod, 'GroupService')

  return {
    getSettings: () => S.Get(),
    saveSettings: (s) => S.Save(s),
    listInterfaces: () => S.ListInterfaces(),
    downloadDir: () => S.DownloadDir(),

    sendMessage: (peerId, text) => C.SendMessage(peerId, text),
    history: (peerId, limit, before) => C.History(peerId, limit, before),
    conversations: () => C.Conversations(),

    sendFile: (peerId, path) => T.SendFile(peerId, path),
    transfers: () => T.List(),

    self: () => P.Self(),
    peerList: () => P.List(),
    diagnostics: () => P.Diagnostics(),

    groupList: () => G.List(),
    groupCreate: (name, ids) => G.Create(name, ids),
    groupAddMember: (gid, mid) => G.AddMember(gid, mid),
    groupSendMessage: (gid, text) => G.SendMessage(gid, text),
    groupHistory: (gid, limit, before) => G.History(gid, limit, before)
  }
}

function pick(mod: AnyMod, name: string): Record<string, AnyFn> {
  const svc = mod[name] ?? mod.default?.[name]
  if (!svc) {
    throw new Error(
      `bindings 缺少服务 ${name}：请运行 wails3 generate bindings 后确认导出名，` +
        `并修正 src/api/bindings.ts`
    )
  }
  return svc as Record<string, AnyFn>
}
