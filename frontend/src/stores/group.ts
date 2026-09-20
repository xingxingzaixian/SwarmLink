import { defineStore } from 'pinia'
import { ref } from 'vue'

import { getApi, type Group } from '../api'

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

  function activeCount(g: Group): number {
    return g.members.filter((m) => m.state === 'active').length
  }

  return { groups, error, refresh, create, addMember, activeCount, MAX_MEMBERS }
})
