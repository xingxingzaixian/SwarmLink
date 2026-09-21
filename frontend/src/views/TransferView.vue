<script setup lang="ts">
/**
 * 文件传输（弹层的「传输」页）：全局记录。
 *
 * 它和右栏的「传输」页职责不同，不能合并：
 *   - 右栏：当前会话的进度，回答「我刚发的那个文件到哪了」；
 *   - 这里：所有任务（含接收），回答「刚才谁给我发的东西存哪了」。
 *
 * 发起发送的入口不在这里 —— 它属于会话上下文，放在聊天区。
 * 旧版在传输页放了一个「选择接收方 + 填路径」的表单，本质是把
 * 「发给谁」这件事从会话里摘出来，反而多了一步。
 */
import { computed, onMounted, ref } from 'vue'

import Icon from '../components/Icon.vue'
import TransferList from '../components/TransferList.vue'
import { getApi } from '../api'
import { isActive, useTransferStore } from '../stores/transfer'
import { useUiStore } from '../stores/ui'

const transfers = useTransferStore()
const ui = useUiStore()
const loading = ref(false)
const clearing = ref(false)

const done = computed(() => transfers.jobs.filter((j) => j.status === 'done').length)
const finished = computed(() => transfers.jobs.filter((j) => !isActive(j.status)).length)

/**
 * 只清【已结束】的：进行中的任务不该由界面打断 ——
 * 清掉它们会让用户以为传输被取消了，而实际上后台还在跑。
 */
async function clearFinished(): Promise<void> {
  if (!finished.value) return
  clearing.value = true
  try {
    const n = await (await getApi()).clearTransfers()
    ui.notify(n > 0 ? `已清除 ${n} 条记录` : '没有可清除的记录')
    await transfers.refresh()
  } catch (e: any) {
    ui.notify(String(e?.message ?? e))
  } finally {
    clearing.value = false
  }
}

async function load(): Promise<void> {
  loading.value = true
  try {
    await transfers.refresh()
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="transfers">
    <div class="bar">
      <span class="faint">
        共 {{ transfers.jobs.length }} 个任务<template v-if="done">，{{ done }} 个已完成</template>
      </span>
      <span class="spacer" />
      <button
        class="btn btn-soft"
        :disabled="clearing || !finished"
        :title="finished ? `清除 ${finished} 条已结束的记录（进行中的不受影响）` : '没有已结束的记录'"
        @click="clearFinished"
      >
        <Icon name="trash" :size="14" />{{ clearing ? '清除中…' : '清空已结束' }}
      </button>
      <button class="btn btn-soft" :disabled="loading" @click="load">
        <Icon name="refresh" :size="14" />{{ loading ? '刷新中…' : '刷新' }}
      </button>
    </div>

    <div v-if="transfers.error" class="banner err">{{ transfers.error }}</div>

    <TransferList :jobs="transfers.jobs" show-peer />
  </div>
</template>

<style scoped>
.transfers {
  display: flex;
  flex-direction: column;
}

.bar {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 12px 20px 0;
  font-size: var(--fs-sm);
}

.spacer {
  flex: 1 1 auto;
}

.banner {
  margin: 10px 20px 0;
  border-radius: var(--r-md);
  border-bottom: 0;
}

.transfers :deep(.wrap) {
  padding: 12px 20px 18px;
}
</style>
