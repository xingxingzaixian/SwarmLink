<script setup lang="ts">
import { onMounted, ref } from 'vue'

import { getApi, type Settings } from '../api'

const settings = ref<Settings | null>(null)
const interfaces = ref<string[]>([])
const restartNeeded = ref<string[]>([])
const notice = ref('')
const saving = ref(false)

const seedsText = ref('')

async function load(): Promise<void> {
  const api = await getApi()
  settings.value = await api.getSettings()
  interfaces.value = await api.listInterfaces()
  seedsText.value = (settings.value.seeds ?? []).join('\n')
}

async function save(): Promise<void> {
  if (!settings.value) return
  saving.value = true
  notice.value = ''
  restartNeeded.value = []
  try {
    const api = await getApi()
    const payload: Settings = {
      ...settings.value,
      seeds: seedsText.value
        .split('\n')
        .map((s) => s.trim())
        .filter(Boolean)
    }
    restartNeeded.value = await api.saveSettings(payload)
    notice.value = restartNeeded.value.length
      ? '已保存。以下改动需重启应用后生效。'
      : '已保存。'
  } catch (e: any) {
    notice.value = String(e?.message ?? e)
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="pane">
    <div class="pane-head">
      <div class="pane-title">设置</div>
      <button class="primary" :disabled="saving || !settings" @click="save">保存</button>
    </div>

    <div class="pane-body form">
      <div v-if="!settings" class="empty">正在加载…</div>

      <template v-else>
        <div v-if="notice" class="banner">{{ notice }}</div>
        <div v-if="restartNeeded.length" class="restart">
          需重启生效：<span class="mono">{{ restartNeeded.join(', ') }}</span>
        </div>

        <section>
          <h4>通用</h4>
          <label>显示名<input v-model="settings.displayName" /></label>
          <label>接收目录<input v-model="settings.downloadDir" /></label>
          <label class="check">
            <input v-model="settings.autoOpen" type="checkbox" />
            接收完成后自动打开
          </label>
        </section>

        <section>
          <h4>跨网段种子</h4>
          <p class="faint">
            每网段至少 2 颗，共 6–10 条，格式 <span class="mono">IP:UDP端口</span>。<br />
            被选为种子的机器必须固定 UDP 端口（不允许回退），否则跨网段节点第一个报文就找不到它。
          </p>
          <label>
            种子清单（每行一条）
            <textarea v-model="seedsText" rows="5" class="mono" />
          </label>
          <label>
            每轮拉取种子数
            <input v-model.number="settings.seedsPerRefresh" type="number" min="1" max="8" />
          </label>
          <p class="faint">
            建议 2：拉更多只是成倍增加流量，可靠性并不提高（ADR-011）。
          </p>
          <label>
            目录刷新周期（秒）
            <input v-model.number="settings.seedRefreshSec" type="number" min="30" />
          </label>
        </section>

        <section>
          <h4>传输</h4>
          <label>全局并发传输上限<input v-model.number="settings.maxConcurrent" type="number" min="1" /></label>
          <label>分块大小（字节，建议 2 的幂）<input v-model.number="settings.chunkSize" type="number" min="65536" /></label>
          <label>初始窗口大小<input v-model.number="settings.windowSize" type="number" min="1" max="64" /></label>
        </section>

        <section>
          <h4>连接</h4>
          <label>空闲连接回收（秒）<input v-model.number="settings.idleTimeoutSec" type="number" min="30" /></label>
          <label class="check">
            <input v-model="settings.requireAuth" type="checkbox" />
            强制 Ed25519 握手认证（强烈建议开启）
          </label>
          <p class="faint">关闭后任何节点都可冒充他人身份，仅用于本地调试。</p>
        </section>

        <section>
          <h4>网络（需重启生效）</h4>
          <label>TCP 端口（0 = 自动）<input v-model.number="settings.tcpPort" type="number" min="0" /></label>
          <label>UDP 端口（0 = 自动）<input v-model.number="settings.udpPort" type="number" min="0" /></label>
          <label>
            网卡模式
            <select v-model="settings.interfaceMode">
              <option value="auto">自动</option>
              <option value="manual">指定网卡</option>
              <option value="seed_only">仅显式种子（VPN / 安全敏感）</option>
            </select>
          </label>
          <div class="faint mono ifaces">
            当前可用：{{ interfaces.length ? interfaces.join(', ') : '（无）' }}
          </div>
          <label>排除网卡（每行一条，支持 * 通配）
            <textarea
              :value="(settings.denyInterfaces ?? []).join('\n')"
              rows="3"
              class="mono"
              @input="settings.denyInterfaces = ($event.target as HTMLTextAreaElement).value.split('\n').map(s => s.trim()).filter(Boolean)"
            />
          </label>
        </section>

        <section>
          <h4>安全</h4>
          <label>
            加密（v1.1 启用）
            <select v-model="settings.encryption">
              <option value="off">off（v1.0 默认）</option>
              <option value="prefer">prefer</option>
              <option value="require">require</option>
            </select>
          </label>
          <p class="faint">
            v1.0 已用 Ed25519 三段握手解决「冒充」；加密通道的接缝
            （Flags.ENCRYPTED + KEY_EXCHANGE）已在协议里预留。
          </p>
        </section>

        <section>
          <h4>部署前置条件（必须由部署方保证）</h4>
          <p class="faint">
            <b>P-1</b> 任意两网段的种子 IP 之间可 TCP 直连（三层路由可达，非 NAT 隔离）。<br />
            不满足则跨网段发现与消息全部失效，只能引入中继。<br /><br />
            <b>P-2</b> 各网段使用不重叠的地址段（不得都是 192.168.1.0/24）。<br />
            不满足会出现「node_id 不同、IP 相同」，跨网段寻址崩溃且现象隐蔽。<br /><br />
            自检：<span class="mono">swarmlink-cli --self-check</span>
          </p>
        </section>
      </template>
    </div>
  </div>
</template>

<style scoped>
.form {
  padding: 12px;
}

section {
  margin-bottom: 16px;
  padding-bottom: 12px;
  border-bottom: 1px solid var(--border-soft);
}

h4 {
  margin: 0 0 8px;
  font-size: 12px;
  color: var(--text);
  letter-spacing: 0.03em;
}

label {
  display: block;
  margin-bottom: 8px;
  color: var(--text-dim);
  font-size: 11px;
}

label input,
label select,
label textarea {
  margin-top: 3px;
  font-size: 12px;
}

label.check {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--text);
}

label.check input {
  width: auto;
  margin: 0;
}

textarea {
  resize: vertical;
  font-family: inherit;
}

.restart {
  padding: 6px 8px;
  margin-bottom: 10px;
  border-radius: var(--radius-sm);
  background: rgba(210, 153, 34, 0.12);
  color: var(--warn);
  font-size: 11px;
}

.ifaces {
  margin-bottom: 8px;
  word-break: break-all;
}
</style>
