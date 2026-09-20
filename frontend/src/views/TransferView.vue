<script setup lang="ts">
import { computed, ref, watch } from 'vue'

import ProgressBar from '../components/ProgressBar.vue'
import { usePeersStore } from '../stores/peers'
import { useTransferStore } from '../stores/transfer'

const props = defineProps<{ preselectedPeer?: string }>()

const peers = usePeersStore()
const transfers = useTransferStore()

const peerId = ref('')
const path = ref('')
const sending = ref(false)
const notice = ref('')

watch(
  () => props.preselectedPeer,
  (v) => {
    if (v) peerId.value = v
  }
)

const peerName = computed(() => (id: string) => peers.nameOf(id))

function human(bytes: number): string {
  if (!bytes) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = bytes
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 10 || i === 0 ? 0 : 1)} ${units[i]}`
}

function statusTag(j: { status: string }): { text: string; cls: string } {
  switch (j.status) {
    case 'done':
      return { text: '完成', cls: 'ok' }
    case 'failed':
      return { text: '失败', cls: 'err' }
    case 'verifying':
      return { text: '校验中', cls: 'warn' }
    case 'paused':
      return { text: '已暂停', cls: 'warn' }
    case 'queued':
      return { text: '排队中', cls: '' }
    case 'cancelled':
      return { text: '已取消', cls: '' }
    default:
      return { text: '传输中', cls: 'info' }
  }
}

async function send(): Promise<void> {
  notice.value = ''
  if (!peerId.value || !path.value.trim()) {
    notice.value = '请选择节点并填写文件路径'
    return
  }
  sending.value = true
  try {
    await transfers.sendFile(peerId.value, path.value.trim())
    path.value = ''
    notice.value = '已开始发送（结果通过事件通知）'
    await transfers.refresh()
  } catch (e: any) {
    notice.value = String(e?.message ?? e)
  } finally {
    sending.value = false
  }
}
</script>

<template>
  <div class="pane">
    <div class="pane-head">
      <div class="pane-title">文件传输</div>
      <button @click="transfers.refresh()">刷新</button>
    </div>

    <div class="send-form">
      <select v-model="peerId">
        <option value="">（选择接收方）</option>
        <option v-for="p in peers.peers" :key="p.nodeId" :value="p.nodeId">
          {{ p.displayName }} · {{ p.shortId }}
        </option>
      </select>
      <input v-model="path" placeholder="本地文件绝对路径（如 /Users/me/a.zip）" />
      <button class="primary" :disabled="sending" @click="send">发送文件</button>
      <div v-if="notice" class="faint">{{ notice }}</div>
      <div class="faint tip">
        支持断点续传：同一文件重复发送会自动从已完成处继续。<br />
        原生文件选择对话框需在 Wails 侧接入后再替换此输入框。
      </div>
    </div>

    <div class="pane-body">
      <div v-if="!transfers.jobs.length" class="empty">暂无传输任务。</div>

      <div v-for="j in transfers.jobs" :key="j.jobId" class="job">
        <div class="job-head">
          <span class="row-title">{{ j.fileName || j.jobId.slice(0, 8) }}</span>
          <span class="tag" :class="statusTag(j).cls">{{ statusTag(j).text }}</span>
        </div>
        <div class="row-sub">
          {{ j.direction === 'send' ? '发送至' : '接收自' }} {{ peerName(j.peerId) }} ·
          {{ human(j.fileSize) }}
          <span v-if="j.direction === 'send'">· {{ j.percent.toFixed(0) }}%</span>
        </div>
        <ProgressBar :percent="j.percent" :status="j.status" />
        <div v-if="j.error" class="err-text">{{ j.error }}</div>
        <div v-if="j.status === 'done' && j.localPath" class="faint mono path">
          {{ j.localPath }}
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.send-form {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 12px;
  border-bottom: 1px solid var(--border);
}

.tip {
  font-size: 11px;
  line-height: 1.45;
}

.job {
  padding: 8px;
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-sm);
  margin-bottom: 8px;
  display: flex;
  flex-direction: column;
  gap: 5px;
}

.job-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.err-text {
  color: var(--err);
  font-size: 11px;
}

.path {
  font-size: 11px;
  word-break: break-all;
}
</style>
