<script setup lang="ts">
/**
 * 设置（弹层的「设置」页）。
 *
 * 不复用旧的整页布局：弹层里没有整屏可用，因此把动作（保存）做成吸底条，
 * 保证滚动到任何位置都能落盘，而不用先滚回顶部。
 */
import { onMounted, ref, watch } from 'vue'

import Icon from '../components/Icon.vue'
import { getApi, type SelfCheck, type Settings } from '../api'
import { useUiStore } from '../stores/ui'

const ui = useUiStore()

const settings = ref<Settings | null>(null)
const interfaces = ref<string[]>([])
/** 保存/自检的**失败**信息。成功反馈走轻提示（弹层会关闭，见 save）。 */
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
  try {
    const api = await getApi()
    const payload: Settings = {
      ...settings.value,
      seeds: seedsText.value
        .split('\n')
        .map((s) => s.trim())
        .filter(Boolean)
    }
    const needRestart = await api.saveSettings(payload)

    // 保存成功 → 就地关闭弹层，结果交给全局轻提示。
    //
    // 为什么必须换通道：轻提示是弹层关掉之后【仍然存在】的那块反馈面，
    // 而「哪些项要重启才生效」不能因为弹层消失就丢掉 —— 用户合上设置、
    // 回聊天里又发现端口没变，那种困惑排查起来最费劲。
    ui.notify(needRestart.length ? `已保存，重启后生效：${needRestart.join('、')}` : '已保存')
    ui.closePanel()
  } catch (e: any) {
    // 失败【不关】：用户要对着这条错误原地改。关掉弹层等于把错误一起关掉，
    // 而「点了保存却没生效」恰恰是这一页最不能含糊的一件事。
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

/**
 * 托盘菜单改了配置 → 只回读那三个提示开关。
 *
 * 刻意不整份重载：用户可能正在编辑种子清单等字段，整份 load() 会把没保存的
 * 输入冲掉。而提示开关没有「编辑中」状态（点一下即生效），回读是安全的。
 */
watch(
  () => ui.configRevision,
  async () => {
    if (!settings.value) return
    try {
      const api = await getApi()
      const fresh = await api.getSettings()
      settings.value.notifySound = fresh.notifySound
      settings.value.notifyMinimizeToTray = fresh.notifyMinimizeToTray
      settings.value.autoStart = fresh.autoStart
    } catch {
      /* 回读失败就维持现状，用户仍可手动保存 */
    }
  }
)

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
          <h4>提示</h4>
          <label class="check">
            <input v-model="settings.notifySound" type="checkbox" />
            <span>新消息提示音</span>
          </label>
          <label class="check">
            <input v-model="settings.notifyMinimizeToTray" type="checkbox" />
            <span>关闭时最小化到托盘</span>
          </label>
          <label class="check">
            <input v-model="settings.autoStart" type="checkbox" />
            <span>开机自启动</span>
          </label>
          <p class="note">
            右键托盘图标也有提示音和托盘设置，与这里是同一份配置，改哪边都一样。
            关掉提示音只影响声音，托盘图标闪烁与未读计数照常工作。
          </p>
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
            <textarea :value="(settings.denyInterfaces ?? []).join('\n')" rows="4" class="mono"
              @input="settings.denyInterfaces = lines(($event.target as HTMLTextAreaElement).value)" />
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
            <p v-if="selfCheck.performed" :class="['line', selfCheck.p1OK ? 'ok' : 'bad']">
              P-1：{{ selfCheck.p1Detail }}
            </p>
            <p v-if="selfCheck.performed" :class="['line', selfCheck.p2Overlap ? 'warn' : 'ok']">
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
        <span v-if="notice">{{ notice }}</span>
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
}

.settings>.col:last-child {
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
  border-bottom: 1px solid var(--divider);
}

h4 {
  display: flex;
  align-items: center;
  gap: 7px;
  margin: 0 0 10px;
  font-family: var(--font-display);
  font-size: var(--fs-lg);
  font-weight: 600;
}

.req {
  padding: 0 6px;
  border-radius: 999px;
  background: var(--warn-soft);
  color: var(--warn);
  font-family: var(--font-mono);
  font-size: 10px;
  font-weight: 500;
  line-height: 16px;
}

label {
  display: block;
  margin-bottom: 9px;
  font-size: var(--fs-xs);
  color: var(--text-muted);
}

label>span {
  display: block;
  margin-bottom: 4px;
}

label.check {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--text-secondary);
  font-size: var(--fs-sm);
}

label.check>span {
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
  color: var(--text-muted);
}

.note code {
  padding: 1px 5px;
  border-radius: var(--r-xs);
  background: var(--bg-sunken);
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text-secondary);
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
  color: var(--text-muted);
}

.line.ok {
  color: var(--accent);
}

.line.bad {
  color: var(--danger);
}

.line.warn {
  color: var(--warn);
}

/* 吸底条：渐隐到弹层自己的底色，而不是某个实色 ——
   实色会在底部切出一条可见的横带。
   用 color-mix 派生，浅色主题不必再写一份覆写。 */
.bar {
  grid-column: 1 / -1;
  position: sticky;
  bottom: 0;
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 0;
  margin-top: 4px;
  background: linear-gradient(to top,
      color-mix(in oklch, var(--bg-panel) 94%, transparent) 62%,
      transparent);
}

/* 吸底条上只剩下失败信息了（成功反馈已改走轻提示），
   因此它固定是警示色 —— 这一栏出现的每一句话都是「没成功」。 */
.bar-msg {
  flex: 1 1 auto;
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: var(--fs-sm);
  color: var(--danger);
}
</style>
