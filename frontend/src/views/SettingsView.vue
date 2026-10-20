<script setup lang="ts">
/**
 * 设置（弹层的「设置」页）。
 *
 * 布局是「左分类 + 右配置项」：设置项之间的差异远大于它们之间的联系
 * （端口/网卡与显示名毫无关系），一屏平铺两列时用户只能靠扫读去找；
 * 分类栏把「找到那一项」变成一次点击，也让每一页都能写清楚自己在管什么。
 *
 * 分类粒度宁可细一点：不把无关的项塞进同一页 —— 「跨网段种子」和「传输」
 * 都跟带宽有关，但一个是发现面、一个是数据面，混在一起只会让两边的说明
 * 都变长，而找起来仍然是猜。
 *
 * 两处刻意保留的老行为（别顺手改掉）：
 *   - 动作（保存）做成常驻底条：弹层里没有整屏可用，且任何一页都可能比
 *     视口高，保证不滚动也能落盘；
 *   - 保存成功即关闭弹层、结果交给全局轻提示（见 save 的注释）。
 */
import { computed, onMounted, ref, watch } from 'vue'

import Icon from '../components/Icon.vue'
import { getApi, type SelfCheck, type Settings } from '../api'
import { useUiStore } from '../stores/ui'
import { useUpdateStore } from '../stores/update'

const ui = useUiStore()
const update = useUpdateStore()

type CatKey =
  | 'general'
  | 'notify'
  | 'network'
  | 'connect'
  | 'transfer'
  | 'seed'
  | 'update'
  | 'diag'

interface Cat {
  key: CatKey
  label: string
  icon: string
  /** 右侧标题下的导语：一句话说清这页在管什么、以及为什么要注意。 */
  desc: string
  /** 本页含「改完需重启才生效」的项 → 分类栏上点一个警示点。 */
  restart?: boolean
}

/**
 * 顺序按使用频率排：改名/换接收目录是常事，部署自检只在跨网段上线前跑一次。
 *
 * `restart` 只挂在「网络」上 —— 后端 Save 返回的需重启项就是
 * tcp_port / udp_port / interface_mode 三个，全在这一页。
 */
const CATS: Cat[] = [
  {
    key: 'general',
    label: '通用',
    icon: 'sliders',
    desc: '显示名与接收目录。显示名对端在资料卡里看得到。'
  },
  {
    key: 'notify',
    label: '提示与启动',
    icon: 'bell',
    desc: '提示音、托盘行为与开机自启。'
  },
  {
    key: 'network',
    label: '网络',
    icon: 'signal',
    restart: true,
    desc: '端口与网卡。改完需重启应用才生效 —— 监听端口是内核层绑定的，无法热切换。'
  },
  {
    key: 'connect',
    label: '连接与安全',
    icon: 'shield',
    desc: '空闲连接回收、握手认证与加密通道。'
  },
  {
    key: 'transfer',
    label: '传输',
    icon: 'download',
    desc: '并发、窗口与分块共同决定实际吞吐。'
  },
  {
    key: 'seed',
    label: '跨网段种子',
    icon: 'globe',
    desc: '跨网段发现只认种子清单；这里配错了，跨网段就等于断网。'
  },
  {
    key: 'update',
    label: '更新',
    icon: 'download',
    desc: '从局域网里版本更高的机器拉取安装包。默认开启，下载完成后再问你什么时候装。'
  },
  {
    key: 'diag',
    label: '部署自检',
    icon: 'activity',
    desc: '两条前置条件只能靠改网段规划来修，软件无能为力。'
  }
]

const cat = ref<CatKey>('general')
const current = computed<Cat>(() => CATS.find((c) => c.key === cat.value) ?? CATS[0])

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
  // 更新状态单独拉一次：事件只在变化时推，打开设置页时可能已经错过了
  // 「下载完成」那一帧（比如应用启动时就已经 ready）。
  void update.refresh().catch(() => {
    /* 拉不到就显示「—」，不影响其它设置项 */
  })
}

// ---------------------------------------------------------------- 局域网更新

/** 是否有可立即安装的包（ready / scheduled 都算）。 */
const canInstall = computed(
  () => update.state?.phase === 'ready' || update.state?.phase === 'scheduled'
)

/** 「待安装」那一行的文案（没有待安装项时为空，整行不显示）。 */
const pendingLabel = computed(() => {
  const st = update.state
  if (!st?.targetVersion || !canInstall.value) return ''
  const size = st.size ? `，${formatBytes(st.size)}` : ''
  if (st.phase === 'scheduled' && st.scheduledAt) {
    return `v${st.targetVersion}，已安排在 ${formatTime(st.scheduledAt)}${size}`
  }
  return `v${st.targetVersion}，等待你确认${size}`
})

function formatBytes(n: number): string {
  if (!n) return '—'
  const mb = n / (1024 * 1024)
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb.toFixed(1)} MB`
}

function formatTime(ms: number): string {
  const d = new Date(ms)
  const pad = (v: number): string => String(v).padStart(2, '0')
  return `${d.getMonth() + 1}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

async function checkUpdate(): Promise<void> {
  notice.value = ''
  try {
    await update.checkNow()
  } catch (e: any) {
    notice.value = String(e?.message ?? e)
  }
}

async function applyUpdate(): Promise<void> {
  notice.value = ''
  try {
    await update.applyNow()
    // 安装成功后进程会退出：这句提示给的是「正在发生什么」，
    // 而不是「已经完成」—— 前端等不到完成的那一刻。
    ui.notify('正在安装更新，应用会立即重启')
  } catch (e: any) {
    notice.value = String(e?.message ?? e)
  }
}

/** 快捷排期：今晚 22:00（已过 22:00 则顺延到明天）。 */
async function scheduleTonight(): Promise<void> {
  const at = new Date()
  at.setHours(22, 0, 0, 0)
  if (at.getTime() <= Date.now()) at.setDate(at.getDate() + 1)
  await scheduleAt(at)
}

/** 快捷排期：明天 02:00（凌晨无人值守的常规窗口）。 */
async function scheduleTomorrowNight(): Promise<void> {
  const at = new Date()
  at.setDate(at.getDate() + 1)
  at.setHours(2, 0, 0, 0)
  await scheduleAt(at)
}

async function scheduleAt(at: Date): Promise<void> {
  notice.value = ''
  try {
    await update.scheduleAt(at.getTime())
    ui.notify(`已安排在 ${formatTime(at.getTime())} 安装`)
  } catch (e: any) {
    notice.value = String(e?.message ?? e)
  }
}

async function cancelUpdate(): Promise<void> {
  notice.value = ''
  try {
    await update.cancel()
  } catch (e: any) {
    notice.value = String(e?.message ?? e)
  }
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
  <div v-if="!settings" class="empty">正在加载…</div>

  <div v-else class="settings">
    <!-- 左：分类。只有一层导航，每一项都是一个「页面」。 -->
    <nav class="cats" aria-label="设置分类">
      <button
        v-for="c in CATS"
        :key="c.key"
        class="cat"
        :class="{ on: cat === c.key }"
        :aria-current="cat === c.key ? 'page' : undefined"
        @click="cat = c.key"
      >
        <Icon :name="c.icon" :size="15" class="cat__icon" />
        <span class="cat__label">{{ c.label }}</span>
        <!-- 需重启的点只做提示、不改文字：分类名里塞不下「需重启」，
             而把一个圆点解释清楚的成本远低于让用户改完端口白等。 -->
        <span v-if="c.restart" class="cat__dot" title="本页有需重启才生效的项" />
      </button>
    </nav>

    <!-- 右：当前分类的配置项。独立滚动，底条始终可见。 -->
    <div class="pane scroll">
      <!-- 用 div 而不是 header：`<header>` 在没有 sectioning 祖先时会被映射成
           role=banner，而一个弹层里再冒出一个 banner 地标会打乱屏幕阅读器的
           页面结构（真正的 banner 是弹层顶栏）。 -->
      <div class="pane-head">
        <h4>
          {{ current.label }}
          <span v-if="current.restart" class="req">需重启</span>
        </h4>
        <p class="lead">{{ current.desc }}</p>
      </div>

      <section v-if="cat === 'general'">
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

      <section v-else-if="cat === 'notify'">
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

      <section v-else-if="cat === 'network'">
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

      <template v-else-if="cat === 'connect'">
        <section>
          <h5>连接</h5>
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
          <h5>安全</h5>
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
      </template>

      <section v-else-if="cat === 'transfer'">
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

      <section v-else-if="cat === 'seed'">
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

      <section v-else-if="cat === 'update'">
        <label class="check">
          <input v-model="settings.updateEnabled" type="checkbox" />
          <span>允许从局域网里版本更高的机器更新</span>
        </label>
        <p class="note">
          关闭后本机不再下载安装包、也不再提示；但仍可以把本机的安装包提供给别人
          —— 否则只要有一台机器关了开关，全网版本就会卡在它身上。
        </p>

        <div class="urows">
          <div class="urow">
            <span>当前版本</span>
            <b class="mono">v{{ update.state?.currentVersion ?? '—' }}</b>
          </div>
          <div class="urow">
            <span>局域网最新版本</span>
            <b v-if="update.topPeer">
              v{{ update.topPeer.version }}（{{
                update.topPeer.displayName || update.topPeer.peerId.slice(0, 8)
              }}）
            </b>
            <b v-else class="faint">暂无更高版本</b>
          </div>
          <div v-if="pendingLabel" class="urow">
            <span>待安装</span>
            <b>{{ pendingLabel }}</b>
          </div>
          <div v-if="update.state?.phase === 'downloading'" class="urow">
            <span>下载进度</span>
            <b>
              {{ (update.state.percent ?? 0).toFixed(0) }}%（{{
                formatBytes(update.state.downloaded ?? 0)
              }}
              / {{ formatBytes(update.state.size ?? 0) }}）
            </b>
          </div>
          <div v-if="update.state?.error" class="urow">
            <span>最近一次失败</span>
            <b class="bad">{{ update.state.error }}</b>
          </div>
          <div v-if="!update.supported" class="urow">
            <span>平台支持</span>
            <b class="warn">当前平台暂不支持自动安装</b>
          </div>
        </div>

        <label>
          <span>发现新版本后</span>
          <select v-model="settings.updateMode">
            <option value="prompt">下载完成后询问我（推荐）</option>
            <option value="auto">自动安装并重启</option>
            <option value="notify">只提示，我自己决定</option>
          </select>
        </label>

        <label>
          <span>无人值守时</span>
          <input
            v-model.number="settings.updateAutoInstallHours"
            type="number"
            min="0"
            max="72"
          />
        </label>
        <p class="note">
          下载完成后这么久仍没有任何操作，就自动安装并重启（对应夜间无人值守）。
          填 <b>0</b> 表示永不自动安装 —— 那样没人点的机器会一直停在旧版本。
        </p>

        <div class="uacts">
          <button class="btn btn-soft" :disabled="update.busy" @click="checkUpdate">
            <Icon name="refresh" :size="14" />{{
              update.state?.phase === 'checking' ? '检查中…' : '立即检查'
            }}
          </button>
          <template v-if="canInstall">
            <button class="btn btn-primary" @click="applyUpdate">立即更新并重启</button>
            <button class="btn btn-soft" @click="scheduleTonight">今晚 22:00</button>
            <button class="btn btn-soft" @click="scheduleTomorrowNight">明天 02:00</button>
            <button class="btn btn-soft" @click="cancelUpdate">放弃本次</button>
          </template>
        </div>

        <p v-if="update.state && !update.state.canDistribute" class="note">
          本机目前没有缓存的安装包，因此不能作为别人的更新源。安装包会在安装本版本时
          自动留存一份；从旧版本手工升级的机器需要重装一次才会具备。
        </p>

        <div class="urows about">
          <div class="urow">
            <span>数据目录</span>
            <code class="mono">{{ update.state?.dataDir ?? '—' }}</code>
          </div>
          <div class="urow">
            <span>日志文件</span>
            <code class="mono">{{ update.state?.logPath ?? '—' }}</code>
          </div>
        </div>
      </section>

      <section v-else>
        <p class="note">
          <b>P-1</b> 任意两网段的种子 IP 之间可 TCP 直连（三层路由可达，非 NAT 隔离）。
          不满足则跨网段发现与消息全部失效。<br /><br />
          <b>P-2</b> 各网段使用不重叠的地址段（不得都是 192.168.1.0/24）。
          不满足会出现「node_id 不同、IP 相同」，跨网段寻址崩溃且现象隐蔽。
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
/* 左栏固定宽度 + 右栏吃掉剩余；两行分别给「内容（可滚）」和「底条（不动）」。
   高度 100% 依赖 Modal 的 .body 是定高 flex 项（同共享中心、搜索弹层）。 */
.settings {
  display: grid;
  grid-template-columns: 168px minmax(0, 1fr);
  grid-template-rows: minmax(0, 1fr) auto;
  height: 100%;
  min-height: 0;
}

/* ---------------------------------------------------------------- 分类栏 */
/* 与共享中心的左树同一套观感：内缩、右分割线、圆角行。
   两处必须一致 —— 它们是同一个弹层里同一种「左侧导航」。 */
.cats {
  grid-column: 1;
  grid-row: 1 / span 2;
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-height: 0;
  padding: 0 10px 0 0;
  border-right: 1px solid var(--divider);
}

.cat {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 7px 8px;
  /* 透明边框占位：切到 .on 时行宽不跳 */
  border: 1px solid transparent;
  border-radius: var(--r-md);
  text-align: left;
  color: var(--text-secondary);
  font-size: var(--fs-sm);
  transition: background var(--dur-1) var(--ease), color var(--dur-1) var(--ease),
    border-color var(--dur-1) var(--ease);
}

.cat:hover {
  background: var(--bg-hover);
  color: var(--text-primary);
}

.cat.on {
  border-color: var(--accent);
  background: var(--accent-soft);
  color: var(--text-primary);
  font-weight: 600;
}

.cat__icon {
  flex: 0 0 auto;
  color: var(--text-muted);
}

.cat.on .cat__icon {
  color: var(--accent);
}

.cat__label {
  flex: 1 1 auto;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.cat__dot {
  flex: 0 0 auto;
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--warn);
}

/* ---------------------------------------------------------------- 配置区 */

.pane {
  grid-column: 2;
  grid-row: 1;
  min-height: 0;
  padding: 2px 6px 10px 16px;
}

.pane-head {
  margin-bottom: 16px;
}

h4 {
  display: flex;
  align-items: center;
  gap: 7px;
  margin: 0;
  font-family: var(--font-display);
  font-size: var(--fs-lg);
  font-weight: 600;
}

/* 导语：分类名已经说明了「是什么」，这里补「为什么要注意」 */
.lead {
  margin: 4px 0 0;
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--text-muted);
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

/* 一页里有多组时用虚线分隔（与参考的「分组标题 + 点线」一致）；
   最后一组不加，否则页尾会多一条悬空的线。 */
section {
  padding-bottom: 14px;
  margin-bottom: 14px;
  border-bottom: 1px dashed var(--divider);
}

section:last-of-type {
  padding-bottom: 0;
  margin-bottom: 0;
  border-bottom: 0;
}

/* 组标题：只在「一页有多组」时出现（目前只有连接与安全），
   因此比页面标题小两档、不加粗体以外的装饰 —— 它不该跟 h4 抢层级。 */
h5 {
  margin: 0 0 10px;
  font-family: var(--font-display);
  font-size: var(--fs-md);
  font-weight: 600;
  color: var(--text-secondary);
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

/* ---------------------------------------------------------------- 局域网更新 */
/* 只读信息用「左标题 + 右值」的表格行：它不参与编辑，成一列 input 反而会
   让人以为可以改。 */
.urows {
  display: grid;
  gap: 2px;
  margin: 4px 0 12px;
  padding: 10px 12px;
  border: 1px solid var(--divider);
  border-radius: var(--r-md);
  background: var(--bg-sunken);
}

.urow {
  display: grid;
  grid-template-columns: 108px minmax(0, 1fr);
  gap: 10px;
  align-items: baseline;
  font-size: var(--fs-sm);
  line-height: 1.7;
}

.urow>span {
  color: var(--text-muted);
  font-size: var(--fs-xs);
}

.urow>b,
.urow>code {
  min-width: 0;
  overflow-wrap: anywhere;
}

.urow>code {
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text-secondary);
}

.urow .bad {
  color: var(--danger);
}

.urow .warn {
  color: var(--warn);
}

/* 「关于」那一段用虚线分隔：它是排障信息，不属于「更新」这条主流程。 */
.urows.about {
  margin-top: 14px;
  border-style: dashed;
  background: transparent;
}

.uacts {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin: 10px 0 4px;
}

/* ---------------------------------------------------------------- 吸底条 */
/* 独立成网格行而不是 sticky：内容区自己滚，底条永远在同一位置，
   既不用渐变遮罩，也不会在短页面上飘在内容中间。 */
.bar {
  grid-column: 2;
  grid-row: 2;
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 6px 0 16px;
  border-top: 1px solid var(--divider);
}

/* 底条上只剩下失败信息了（成功反馈已改走轻提示），
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
