<script setup lang="ts">
/**
 * 设置（弹层的「设置」页）。
 *
 * 不复用旧的整页布局：弹层里没有整屏可用，因此把动作（保存）做成吸底条，
 * 保证滚动到任何位置都能落盘，而不用先滚回顶部。
 */
import { onMounted, ref } from 'vue'

import Icon from '../components/Icon.vue'
import { getApi, type SelfCheck, type Settings } from '../api'

const settings = ref<Settings | null>(null)
const interfaces = ref<string[]>([])
const restartNeeded = ref<string[]>([])
const notice = ref('')
const saving = ref(false)
const seedsText = ref('')
const selfCheck = ref<SelfCheck | null>(null)
const checking = ref(false)

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
      ? '已保存。带「需重启」标记的改动要重启应用才生效。'
      : '已保存。'
  } catch (e: any) {
    notice.value = String(e?.message ?? e)
  } finally {
    saving.value = false
  }
}

/**
 * 跑一次部署前置条件自检。
 *
 * 它要真实探一轮种子（最坏 3 秒），因此按钮必须给出「检查中」反馈 ——
 * 否则用户会以为卡住了，而这恰恰是他最需要这次结果的时刻。
 * 改过种子后结果就失效，所以每次都重新跑，不缓存。
 */
async function runSelfCheck(): Promise<void> {
  checking.value = true
  selfCheck.value = null
  try {
    const api = await getApi()
    selfCheck.value = await api.runSelfCheck()
  } catch (e: any) {
    notice.value = String(e?.message ?? e)
  } finally {
    checking.value = false
  }
}

/** 把种子列表压成一行（失败次数只在 >0 时才值得占版面）。 */
function seedLine(s: { addr: string; failCount: number }): string {
  return s.failCount > 0 ? `${s.addr}（失败 ${s.failCount} 次）` : s.addr
}

/** textarea ↔ string[] 的双向桥（避免每处都写一遍 split/filter）。 */
function lines(v: string): string[] {
  return v.split('\n').map((s) => s.trim()).filter(Boolean)
}

onMounted(load)
</script>

<template>
  <div class="settings">
    <div v-if="!settings" class="empty">正在加载…</div>

    <template v-else>
      <div class="col">
        <section>
          <h4>通用</h4>
          <label>
            <span>显示名</span>
            <input v-model="settings.displayName" placeholder="其他节点看到的名字" />
          </label>
          <label>
            <span>接收目录</span>
            <input v-model="settings.downloadDir" class="mono" />
          </label>
          <label class="check">
            <input v-model="settings.autoOpen" type="checkbox" />
            <span>接收完成后自动打开</span>
          </label>
        </section>

        <section>
          <h4>跨网段种子</h4>
          <p class="note">
            每网段至少 2 颗、共 6–10 条，格式 <code>IP:UDP端口</code>。
            被选为种子的机器必须固定 UDP 端口（不允许回退），否则跨网段节点的
            第一个报文就石沉大海。
          </p>
          <label>
            <span>种子清单（每行一条）</span>
            <textarea v-model="seedsText" rows="5" class="mono" />
          </label>
          <div class="pair">
            <label>
              <span>每轮拉取种子数</span>
              <input v-model.number="settings.seedsPerRefresh" type="number" min="1" max="8" />
            </label>
            <label>
              <span>目录刷新周期（秒）</span>
              <input v-model.number="settings.seedRefreshSec" type="number" min="30" />
            </label>
          </div>
          <p class="note">建议值 2：拉更多只是成倍增加流量，可靠性并不提高（ADR-011）。</p>
        </section>

        <section>
          <h4>传输</h4>
          <div class="pair">
            <label>
              <span>全局并发上限</span>
              <input v-model.number="settings.maxConcurrent" type="number" min="1" />
            </label>
            <label>
              <span>初始窗口大小</span>
              <input v-model.number="settings.windowSize" type="number" min="1" max="64" />
            </label>
          </div>
          <label>
            <span>分块大小（字节，建议 2 的幂）</span>
            <input v-model.number="settings.chunkSize" type="number" min="65536" />
          </label>
        </section>
      </div>

      <div class="col">
        <section>
          <h4>网络<span class="req">需重启</span></h4>
          <div class="pair">
            <label>
              <span>TCP 端口（0 = 自动）</span>
              <input v-model.number="settings.tcpPort" type="number" min="0" />
            </label>
            <label>
              <span>UDP 端口（0 = 自动）</span>
              <input v-model.number="settings.udpPort" type="number" min="0" />
            </label>
          </div>
          <label>
            <span>网卡模式</span>
            <select v-model="settings.interfaceMode">
              <option value="auto">自动</option>
              <option value="manual">指定网卡</option>
              <option value="seed_only">仅显式种子（VPN / 安全敏感）</option>
            </select>
          </label>
          <label>
            <span>排除网卡（每行一条，支持 * 通配）</span>
            <textarea
              :value="(settings.denyInterfaces ?? []).join('\n')"
              rows="4"
              class="mono"
              @input="settings.denyInterfaces = lines(($event.target as HTMLTextAreaElement).value)"
            />
          </label>
          <p class="note mono">当前可用：{{ interfaces.length ? interfaces.join('、') : '（无）' }}</p>
        </section>

        <section>
          <h4>连接</h4>
          <label>
            <span>空闲连接回收（秒）</span>
            <input v-model.number="settings.idleTimeoutSec" type="number" min="30" />
          </label>
          <label class="check">
            <input v-model="settings.requireAuth" type="checkbox" />
            <span>强制 Ed25519 握手认证（强烈建议开启）</span>
          </label>
          <p class="note">关闭后任何节点都可冒充他人身份，仅用于本地调试。</p>
        </section>

        <section>
          <h4>安全</h4>
          <label>
            <span>加密通道</span>
            <select v-model="settings.encryption">
              <option value="off">off（v1.0 默认）</option>
              <option value="prefer">prefer</option>
              <option value="require">require</option>
            </select>
          </label>
          <p class="note">
            v1.0 已用 Ed25519 三段握手解决「冒充」；加密通道的接缝
            （Flags.ENCRYPTED + KEY_EXCHANGE）已在协议里预留，v1.1 启用。
          </p>
        </section>

        <section class="plain">
          <h4>部署前置条件（代码救不了）</h4>
          <p class="note">
            <b>P-1</b> 任意两网段的种子 IP 之间可 TCP 直连（三层路由可达，非 NAT 隔离）。
            不满足则跨网段发现与消息全部失效。<br /><br />
            <b>P-2</b> 各网段使用不重叠的地址段（不得都是 192.168.1.0/24）。
            不满足会出现「node_id 不同、IP 相同」，跨网段寻址崩溃且现象隐蔽。<br /><br />
            这两条只能靠改网段规划来修，软件无能为力 —— 所以跨网段部署前先跑一次自检。
          </p>
          <button class="btn btn-soft" :disabled="checking" @click="runSelfCheck">
            <Icon name="refresh" :size="14" />{{ checking ? '检查中…' : '运行自检' }}
          </button>
          <div v-if="selfCheck" class="check-result">
            <p
              v-if="selfCheck.performed"
              :class="['line', selfCheck.p1OK ? 'ok' : 'bad']"
            >
              P-1：{{ selfCheck.p1Detail }}
            </p>
            <p
              v-if="selfCheck.performed"
              :class="['line', selfCheck.p2Overlap ? 'warn' : 'ok']"
            >
              P-2：{{ selfCheck.p2Detail }}
            </p>
            <p v-if="!selfCheck.performed" class="line faint">
              {{ selfCheck.p1Detail }}
            </p>
            <p v-if="selfCheck.performed && selfCheck.seeds.length" class="line faint mono">
              种子 {{ selfCheck.seedCount }} 颗：{{ selfCheck.seeds.map(seedLine).join('、') }}
            </p>
          </div>
        </section>
      </div>
    </template>

    <footer class="bar">
      <div class="bar-msg">
        <span v-if="notice" :class="{ warn: restartNeeded.length }">{{ notice }}</span>
        <span v-if="restartNeeded.length" class="mono faint">
          需重启：{{ restartNeeded.join('、') }}
        </span>
      </div>
      <button class="btn btn-primary" :disabled="saving || !settings" @click="save">
        <Icon name="check" :size="14" />{{ saving ? '保存中…' : '保存' }}
      </button>
    </footer>
  </div>
</template>

<style scoped>
/* 两列：设置项多而短，单列会在弹层里留下大量空白。
   底部留出吸底条的高度，最后一行才能完整滚出来，不会永远被压住一截。 */
.settings {
  display: grid;
  grid-template-columns: 1fr 1fr;
  align-content: start;
  gap: 0 28px;
  padding: 18px 22px 0;
}

.settings > .col:last-child {
  padding-bottom: 56px;
}

.col {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}

section {
  padding-bottom: 18px;
  margin-bottom: 16px;
  border-bottom: 1px solid var(--line-soft);
}

h4 {
  display: flex;
  align-items: center;
  gap: 7px;
  margin: 0 0 10px;
  font-size: var(--fs-sm);
  font-weight: 600;
  letter-spacing: 0.02em;
}

.req {
  padding: 0 6px;
  border-radius: 999px;
  background: var(--warn-soft);
  color: var(--warn-ink);
  font-size: 10px;
  font-weight: 500;
  line-height: 16px;
}

label {
  display: block;
  margin-bottom: 9px;
  font-size: var(--fs-xs);
  color: var(--t3);
}

label > span {
  display: block;
  margin-bottom: 4px;
}

label.check {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--t2);
  font-size: var(--fs-sm);
}

label.check > span {
  margin: 0;
}

.pair {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px;
}

.note {
  margin: 0 0 9px;
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--t3);
}

.note code {
  padding: 1px 5px;
  border-radius: var(--r-xs);
  background: var(--surface-sunken);
  font-family: var(--mono);
  font-size: 11px;
  color: var(--t2);
}

/* 自检结果：三条结论各自带色，让人扫一眼就知道哪条不成立。
   种子明细压到最小号字 —— 它是排查时的补充，不该抢结论的位置。 */
.check-result {
  display: grid;
  gap: 6px;
  margin-top: 10px;
}

.line {
  margin: 0;
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--t3);
}

.line.ok {
  color: var(--ok-ink);
}

.line.bad {
  color: var(--err-ink);
}

.line.warn {
  color: var(--warn-ink);
}

/* 吸底条：渐隐到弹层自己的玻璃色，而不是某个实色 ——
   实色会在弹层底部切出一条可见的横带 */
.bar {
  grid-column: 1 / -1;
  position: sticky;
  bottom: 0;
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 0;
  margin-top: 4px;
  background: linear-gradient(to top, rgba(18, 9, 44, 0.92) 62%, rgba(18, 9, 44, 0));
}

.bar-msg {
  flex: 1 1 auto;
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: var(--fs-sm);
  color: var(--ok);
}

.bar-msg .warn {
  color: var(--warn-ink);
}

html[data-theme='light'] .bar {
  background: linear-gradient(to top, rgba(255, 255, 255, 0.95) 62%, rgba(255, 255, 255, 0));
}
</style>
