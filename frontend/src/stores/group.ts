import { defineStore } from 'pinia'
import { ref } from 'vue'

import { getApi, type Group, type GroupMember } from '../api'
import { memberStateLabel as memberStateLabelOfMember } from '../utils/group'

/** 群列表与成员管理（v1.0 上限 20 人）。 */
export const useGroupStore = defineStore('group', () => {
  const groups = ref<Group[]>([])
  const error = ref('')

  const MAX_MEMBERS = 20

  async function refresh(): Promise<void> {
    try {
      groups.value = await (await getApi()).groupList()
    } catch (e: any) {
      error.value = String(e?.message ?? e)
    }
  }

  async function create(name: string, memberIds: string[]): Promise<Group> {
    if (memberIds.length + 1 > MAX_MEMBERS) {
      throw new Error(`群成员上限 ${MAX_MEMBERS} 人（v1.0）`)
    }
    const g = await (await getApi()).groupCreate(name, memberIds)
    await refresh()
    return g
  }

  async function addMember(groupId: string, memberId: string): Promise<Group> {
    const g = await (await getApi()).groupAddMember(groupId, memberId)
    await refresh()
    return g
  }

  /** 群主移除成员（epoch+1）。 */
  async function removeMember(groupId: string, memberId: string): Promise<Group> {
    const g = await (await getApi()).groupRemoveMember(groupId, memberId)
    await refresh()
    return g
  }

  /** 群主改名（epoch+1）。 */
  async function rename(groupId: string, name: string): Promise<Group> {
    const g = await (await getApi()).groupRename(groupId, name)
    await refresh()
    return g
  }

  /** 群主解散群（epoch+1；成员侧之后停止收发）。 */
  async function dissolve(groupId: string): Promise<Group> {
    const g = await (await getApi()).groupDissolve(groupId)
    await refresh()
    return g
  }

  /**
   * 退出群（本人）。本地立即生效，不依赖群主在线。
   *
   * 退出后仍保留群记录（后端不删成员行），因此会话列表里的这一项还在 ——
   * 面板会把它显示成「已退出」，而不是让它凭空消失。
   */
  async function leave(groupId: string): Promise<Group> {
    const g = await (await getApi()).groupLeave(groupId)
    await refresh()
    return g
  }

  /**
   * 在群人数。
   *
   * 判据用后端算好的 `active`，不用 `state`：state 只是派生缓存，
   * 两处各算一遍迟早会出现「会话状态行说 3 人在线、面板里只有 2 行是在群」。
   */
  function activeCount(g: Group): number {
    return g.members.filter((m) => m.active).length
  }

  /**
   * 在群人数（分母）。
   *
   * 单独一个方法而不是到处写 `members.length`：后者含已退出/已移出的**记录**
   * （它们刻意保留以便审计与 epoch 一致），拿来当分母会出现「3/5 人在线」
   * 而面板里只有 3 行是在群。
   */
  function memberCount(g: Group): number {
    return g.members.filter((m) => m.active).length
  }

  /** 本机在某群里是否还在群（面板据此决定显示「退出群」还是「已退出」）。 */
  function meIn(group: Group, selfId: string): GroupMember | undefined {
    return group.members.find((m) => m.nodeId === selfId)
  }

  /**
   * 成员的「不在群」标签；仍在群时返回空串。
   *
   * 实现放在 utils/group.ts（纯函数、有测试）：这条判据在群信息面板与
   * 联系人卡片上都要用，两处各写一份迟早会分叉成「面板说已退出、卡片说不在群」。
   */
  const memberStateLabel = memberStateLabelOfMember

  /** 按 groupId 取群（会话标题/状态行等只拿到 convId 的地方用）。 */
  function byId(groupId: string): Group | undefined {
    return groups.value.find((g) => g.groupId === groupId)
  }

  /**
   * 每个群最后见到的「名册签名」：name + state + 名册摘要。
   *
   * 用途只有一个 —— 抑制重复刷新。名册同步本身就是「多发几次也无害」的设计
   * （推送 + 拉取 + 60s 抽查），但每次都整份重拉会让面板在毫无变化时反复重渲染，
   * 看起来像界面在抽风。
   *
   * 为什么 name 也要进签名：**名字不在名册摘要里**（ADR-012 附 B-#1：
   * DisplayName 不进签名域，群名进群主签名但不进摘要）。只看摘要会让
   * 「改名」既刷新不了标题、也刷新不了面板。
   */
  const signatures = ref<Record<string, string>>({})

  /** 签名是否变了；变了就顺手记下（相同则返回 false，调用方据此跳过重拉）。 */
  function signatureChanged(groupId: string, signature: string): boolean {
    if (!groupId) return false
    if (!signature) return true // 老后端不带摘要：保守地照旧刷新
    if (signatures.value[groupId] === signature) return false
    signatures.value = { ...signatures.value, [groupId]: signature }
    return true
  }

  return {
    groups,
    error,
    refresh,
    create,
    addMember,
    removeMember,
    rename,
    dissolve,
    leave,
    activeCount,
    memberCount,
    byId,
    meIn,
    memberStateLabel,
    signatureChanged,
    MAX_MEMBERS
  }
})
