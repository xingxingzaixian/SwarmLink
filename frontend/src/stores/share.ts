import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { getApi } from '../api'
import type { Share, ShareDownload, ShareItem } from '../api/types'

/**
 * 文件共享（FR-14）状态。
 *
 * 这里只维护【共享清单】。文件清单（Browse 的结果）不落 store：
 * 它属于「正在看哪一条」这个视图状态，跟着视图走才不会在切页后留下
 * 一串与当前选中项无关的文件列表。
 *
 * 两个分组的判据只有一个：`mine`（后端给）。前端不从 ownerId 反推 ——
 * 「谁是我」在前端还有别的来源，多处判据迟早不一致。
 */
export const useShareStore = defineStore('share', () => {
  const shares = ref<Share[]>([])
  const loading = ref(false)
  const error = ref('')
  /** 是否已完成过至少一次刷新（无论成败），用于区分「还没读」与「就是空的」。 */
  const ready = ref(false)

  const mine = computed(() => shares.value.filter((s) => s.mine))
  const remote = computed(() => shares.value.filter((s) => !s.mine))

  function byId(shareId: string): Share | null {
    return shares.value.find((s) => s.shareId === shareId) ?? null
  }

  async function refresh(): Promise<void> {
    loading.value = true
    try {
      shares.value = await (await getApi()).shareList()
      error.value = ''
    } catch (e: any) {
      error.value = String(e?.message ?? e)
    } finally {
      loading.value = false
      ready.value = true
    }
  }

  /** 发布一条共享。失败时抛错，由调用方决定怎么提示（表单要内联显示）。 */
  async function publish(
    rootPath: string,
    name: string,
    visType: string,
    allowed: string[]
  ): Promise<Share> {
    const created = await (await getApi()).sharePublish(rootPath, name, visType, allowed)
    await refresh()
    return created
  }

  async function unpublish(shareId: string): Promise<void> {
    await (await getApi()).shareUnpublish(shareId)
    await refresh()
  }

  /**
   * 改一条本机共享的可见性。
   *
   * 不换 share_id，因此列表里的位置与选中态都不会跳；后端的重新广播会让
   * 对端尽快看到（不广播的话要等下一轮周期通告）。
   */
  async function setVisibility(
    shareId: string,
    visType: string,
    allowed: string[]
  ): Promise<Share> {
    const updated = await (await getApi()).shareSetVisibility(shareId, visType, allowed)
    await refresh()
    return updated
  }

  async function refreshIndex(shareId: string): Promise<Share> {
    const updated = await (await getApi()).shareRefresh(shareId)
    await refresh()
    return updated
  }

  async function browse(shareId: string): Promise<ShareItem[]> {
    return (await getApi()).shareBrowse(shareId)
  }

  /**
   * 下载共享里的文件。
   *
   * 返回即代表共享方已受理；真正的字节传输是异步的，进度走传输列表
   * （复用 transfer:* 事件），因此这里不做任何进度状态。
   */
  async function download(shareId: string, paths: string[]): Promise<ShareDownload> {
    return (await getApi()).shareDownload(shareId, paths)
  }

  return {
    shares,
    loading,
    ready,
    error,
    mine,
    remote,
    byId,
    refresh,
    publish,
    unpublish,
    setVisibility,
    refreshIndex,
    browse,
    download
  }
})
