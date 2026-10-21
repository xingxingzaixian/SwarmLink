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
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'

import Icon from '../components/Icon.vue'
import ShortcutEditor from '../components/ShortcutEditor.vue'
import { getApi, type SeedTest, type SelfCheck, type Settings } from '../api'
import { useAppearanceStore } from '../stores/appearance'
import { useHotkeysStore } from '../stores/hotkeys'
import { usePeersStore } from '../stores/peers'
import { useUiStore } from '../stores/ui'
import { useUpdateStore } from '../stores/update'

const ui = useUiStore()
const update = useUpdateStore()
const peers = usePeersStore()
const appearance = useAppearanceStore()
const hotkeys = useHotkeysStore()

/** 恢复出厂键位。不二次确认：它是可逆的（重新录一遍即可），且用户就在这一页。 */
async function resetShortcuts(): Promise<void> {
  if (await hotkeys.resetAll()) {
    ui.notify('已恢复默认快捷键')
  }
}

type CatKey =
  | 'general'
  | 'keys'
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
    key: 'keys',
    label: '快捷键',
    icon: 'grid',
    // 这一页不走底条的「保存」：快捷键改完即时生效并落盘，
    // 底条在这里换成「恢复默认」（见模板末尾）。
    desc: '点一下键位即可重新录制。系统级快捷键在应用退到后台时也生效。'
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
const selfCheck = ref<SelfCheck | null>(null)
const checking = ref(false)

/**
 * 种子编辑器的一行（D-14）。
 *
 * 不再用 textarea：跨网段是「一台台机器分别配」的场景，纯文本里删错一个字符
 * 就会让那颗种子静默失效，而用户看不出哪一行坏了。结构化之后每一行都能
 * 单独测试、单独删除。
 */
interface SeedRow {
  /** 稳定 key：用下标当 key 时删行会让输入框焦点错位到下一行。 */
  id: number
  addr: string
  testing: boolean
  /** 本行的即时测试结果（null = 还没测过，显示后台刷新记录的常态）。 */
  result: SeedTest | null
}

const seedRows = ref<SeedRow[]>([])
let seedSeq = 0

function newSeedRow(addr = ''): SeedRow {
  seedSeq += 1
  return { id: seedSeq, addr, testing: false, result: null }
}

async function load(): Promise<void> {
  const api = await getApi()
  settings.value = await api.getSettings()
  interfaces.value = await api.listInterfaces()
  seedRows.value = (settings.value.seeds ?? []).map((a) => newSeedRow(a))
  // 更新状态单独拉一次：事件只在变化时推，打开设置页时可能已经错过了
  // 「下载完成」那一帧（比如应用启动时就已经 ready）。
  void update.refresh().catch(() => {
    /* 拉不到就显示「—」，不影响其它设置项 */
  })
}

// ---------------------------------------------------------------- 外观 / 背景图

const bgInput = ref<HTMLInputElement | null>(null)
const bgError = ref('')
const previewEl = ref<HTMLElement | null>(null)

/**
 * 预览里用的模糊半径。
 *
 * 同一个 px 值，在更小的框里相对更重（模糊是按屏幕像素算的，覆盖的是
 * 画面的更大一块）。所以预览要按「预览宽 / 窗口宽」折算，才是整窗里的
 * 真实观感 —— 否则用户会以为 6px 很重，实际在整窗里几乎看不出来。
 *
 * 代价是预览里的模糊总是「比看起来轻」，所以滑杆旁边那句话必须说清楚：
 * 预览是缩略图，模糊在整窗里更明显。
 */
const viewportW = ref(window.innerWidth)

function onResize(): void {
  viewportW.value = window.innerWidth
}

onMounted(() => window.addEventListener('resize', onResize))
onUnmounted(() => window.removeEventListener('resize', onResize))

/** 预览里的模糊半径（已按比例折算）。 */
const previewBlur = computed(() => {
  const w = previewEl.value?.clientWidth ?? 0
  if (!w) return appearance.state.blur
  return Math.max(0, appearance.state.blur * (w / Math.max(1, viewportW.value)))
})

const previewLayerStyle = computed(() => ({
  backgroundImage: `url("${appearance.state.dataUrl}")`,
  filter: `blur(${previewBlur.value}px)`
}))

const previewScrimStyle = computed(() => ({
  opacity: String(appearance.state.dim / 100)
}))

function pickBackground(): void {
  bgError.value = ''
  bgInput.value?.click()
}

/**
 * 选中文件后交给 store 处理。
 *
 * 立刻清空 input.value：否则用户「选同一张图」时不会触发 change
 * （文件对象没变），看起来像按钮失灵。
 */
async function onBgFile(e: Event): Promise<void> {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return

  bgError.value = ''
  try {
    await appearance.chooseFile(file)
  } catch (err: any) {
    bgError.value = String(err?.message ?? err) || '设置背景失败'
  }
}

async function removeBackground(): Promise<void> {
  bgError.value = ''
  try {
    await appearance.clear()
  } catch (err: any) {
    bgError.value = String(err?.message ?? err) || '移除背景失败'
  }
}

/** 拖滑杆只改本地样式，落盘由 store 防抖（每帧都发 RPC 会一直重写 config.toml）。 */
function onDimInput(e: Event): void {
  appearance.setLook(Number((e.target as HTMLInputElement).value), appearance.state.blur)
}

function onBlurInput(e: Event): void {
  appearance.setLook(appearance.state.dim, Number((e.target as HTMLInputElement).value))
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
      // 空行直接丢掉：用户在最后一行按回车留下空串是常态，
      // 而空串会让后端把整次保存判成「第 N 条格式不对」。
      seeds: seedRows.value
        .map((r) => r.addr.trim())
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

// ---------------------------------------------------------------- 种子编辑器

function addSeedRow(): void {
  seedRows.value.push(newSeedRow())
}

function removeSeedRow(id: number): void {
  seedRows.value = seedRows.value.filter((r) => r.id !== id)
}

/**
 * 测试单颗种子（真实两跳：UDP 探测 + TCP 建链，最坏 5 秒）。
 *
 * 结果按【地址】归属：测完再改这一行的地址，旧结果就会失效
 * （见 seedStatus 里的 addr 比对），不会出现「地址都改了还显示上次的绿」。
 */
async function testSeedRow(row: SeedRow): Promise<void> {
  const addr = row.addr.trim()
  if (!addr || row.testing) return
  row.testing = true
  row.result = null
  try {
    const api = await getApi()
    row.result = await api.testSeed(addr)
  } catch (e: any) {
    row.result = {
      addr,
      ok: false,
      udpOk: false,
      tcpOk: false,
      rttMs: 0,
      nodeId: '',
      displayName: '',
      subnet: '',
      tcpPort: 0,
      error: String(e?.message ?? e)
    }
  } finally {
    row.testing = false
  }
}

type SeedRowKind = 'ok' | 'warn' | 'bad' | 'idle'

/**
 * 一行的状态徽标。
 *
 * 优先级：本次测试结果 > 后台刷新记录的常态 > 未探测。
 * 常态数据来自 peers.diag（App.vue 每 3 秒拉一次），因此保存之后不用
 * 手动点任何东西，下一轮就能看到「可用 / 失败 N 次」。
 */
function seedStatus(row: SeedRow): { kind: SeedRowKind; label: string; title: string } {
  if (row.testing) return { kind: 'idle', label: '测试中…', title: '' }

  const addr = row.addr.trim()
  if (row.result && row.result.addr === addr) {
    const r = row.result
    if (r.ok) {
      const who = r.displayName || r.nodeId.slice(0, 8)
      return {
        kind: 'ok',
        label: `${r.rttMs}ms`,
        title: `${who} · ${r.subnet} · TCP ${r.tcpPort}`
      }
    }
    return { kind: 'bad', label: r.udpOk ? 'TCP 不通' : 'UDP 不通', title: r.error }
  }

  const d = peers.diag?.seedDetails?.find((s) => s.addr === addr)
  if (!d) return { kind: 'idle', label: '未探测', title: '保存后由后台刷新探测' }
  if (d.available && d.lastSeen) {
    return { kind: 'ok', label: '可用', title: `上次探测成功：${formatTime(d.lastSeen)}` }
  }
  if (d.available) {
    return { kind: 'idle', label: '待探测', title: '还没轮到它（每轮随机探 N 颗，属正常）' }
  }
  return { kind: 'bad', label: `失败 ${d.failCount} 次`, title: '正在退避，稍后自动重试' }
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
      <button v-for="c in CATS" :key="c.key" class="cat" :class="{ on: cat === c.key }"
        :aria-current="cat === c.key ? 'page' : undefined" @click="cat = c.key">
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

        <!-- 外观：背景图。改动立即生效，不走「保存」（它不在这张表单里） -->
        <div class="appearance">
          <div class="appearance__head">
            <span class="appearance__title">背景图</span>
            <div class="appearance__actions">
              <button class="btn btn-soft appearance__btn" :disabled="appearance.busy" @click="pickBackground">
                {{ appearance.hasBackground ? '更换图片' : '选择图片' }}
              </button>
              <button
                v-if="appearance.hasBackground"
                class="btn btn-soft appearance__btn"
                :disabled="appearance.busy"
                @click="removeBackground"
              >
                移除
              </button>
            </div>
          </div>

          <!-- 预览 = 整窗背景的缩略：图片 + 遮罩 + 模糊，拖滑杆时实时变。
               里面那颗「文字」用的是真正的 --bg-panel（此时已是半透明），
               所以用户能直接看出「文字压在背景上还读不读得清」，
               不用关掉弹框去猜。 -->
          <div v-if="appearance.hasBackground" ref="previewEl" class="appearance__preview">
            <div class="appearance__preview__layer" :style="previewLayerStyle" />
            <div class="appearance__preview__scrim" :style="previewScrimStyle" />
            <div class="appearance__preview__chip">文字压在背景上</div>
          </div>

          <p class="note appearance__hint">
            {{
              appearance.hasBackground
                ? '图片铺满整个窗口，面板会自动转为半透明，文字依然压在遮罩之上。'
                : '默认使用内置的渐变底。选一张照片，整个窗口都会以它为背景。'
            }}
          </p>

          <label v-if="appearance.hasBackground" class="appearance__slider">
            <span>遮罩 {{ appearance.state.dim }}%</span>
            <input
              type="range"
              min="0"
              max="100"
              step="1"
              :value="appearance.state.dim"
              @input="onDimInput"
            />
          </label>

          <label v-if="appearance.hasBackground" class="appearance__slider">
            <span>模糊 {{ appearance.state.blur }}px</span>
            <input
              type="range"
              min="0"
              max="40"
              step="1"
              :value="appearance.state.blur"
              @input="onBlurInput"
            />
          </label>

          <p v-if="bgError" class="appearance__err">{{ bgError }}</p>

          <input
            ref="bgInput"
            type="file"
            accept="image/png,image/jpeg,image/webp,image/gif"
            class="appearance__file"
            @change="onBgFile"
          />
        </div>
      </section>

      <section v-else-if="cat === 'keys'">
        <ShortcutEditor />
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
          第一个报文就石沉大海。保存后立即生效，无需重启。
        </p>

        <div class="seed-list">
          <div v-for="row in seedRows" :key="row.id" class="seed-row">
            <input
              v-model="row.addr"
              class="mono seed-row__addr"
              placeholder="192.168.2.10:2425"
              spellcheck="false"
              :disabled="row.testing"
            />
            <span
              class="seed-row__state"
              :class="`seed-row__state--${seedStatus(row).kind}`"
              :title="seedStatus(row).title"
            >
              {{ seedStatus(row).label }}
            </span>
            <button
              class="btn btn-soft seed-row__test"
              :disabled="row.testing || !row.addr.trim()"
              @click="testSeedRow(row)"
            >
              {{ row.testing ? '测试中…' : '测试' }}
            </button>
            <button
              class="icon-btn mini"
              title="删除这颗种子"
              aria-label="删除这颗种子"
              @click="removeSeedRow(row.id)"
            >
              <Icon name="trash" :size="13" />
            </button>
          </div>
          <p v-if="!seedRows.length" class="note seed-list__empty">
            还没有配置种子。单网段部署不需要种子。
          </p>
        </div>

        <button class="btn btn-soft" @click="addSeedRow">
          <Icon name="plus" :size="14" />添加种子
        </button>

        <div class="pair seed-tuning">
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
          <input v-model.number="settings.updateAutoInstallHours" type="number" min="0" max="72" />
        </label>
        <p class="note">
          下载完成后这么久仍没有任何操作，就自动安装并重启（对应夜间无人值守）。
          填 <b>0</b> 表示永不自动安装
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
        <span v-else-if="cat === 'keys'" class="faint">快捷键改完立即生效，无需保存</span>
      </div>
      <!-- 快捷键页不走「保存」：它即时生效、即时落盘（见 ShortcutEditor 的说明）。
           给一个「恢复默认」，是因为改键改乱了需要一条确定的退路。 -->
      <button
        v-if="cat === 'keys'"
        class="btn btn-soft"
        :disabled="hotkeys.loading"
        @click="resetShortcuts"
      >
        <Icon name="refresh" :size="14" />恢复默认
      </button>
      <button v-else class="btn btn-primary" :disabled="saving || !settings" @click="save">
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

/* ---------------------------------------------------------------- 种子编辑器 */
.seed-list {
  display: grid;
  gap: 6px;
  margin-bottom: 8px;
}

.seed-row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto auto auto;
  gap: 6px;
  align-items: center;
}

.seed-row__addr {
  min-width: 0;
}

/* 状态徽标固定最小宽度：三行不同的文案（未探测 / 12ms / 失败 3 次）
   不换行，输入框也就不会在测完一颗种子后左右跳一格。 */
.seed-row__state {
  min-width: 56px;
  text-align: right;
  font-family: var(--font-mono);
  font-size: 11px;
  white-space: nowrap;
  color: var(--text-muted);
}

.seed-row__state--ok {
  color: var(--accent);
}

.seed-row__state--warn {
  color: var(--warn);
}

.seed-row__state--bad {
  color: var(--danger);
}

/* 行内按钮要贴合 32px 的输入框高度，直接用 .btn 会高出半行 */
.seed-row__test {
  height: 30px;
  padding: 0 10px;
  font-size: var(--fs-xs);
}

.seed-list__empty {
  margin: 0;
}

.seed-tuning {
  margin-top: 10px;
}

/* ---------------------------------------------------------------- 外观 / 背景图 */
/* 用上分割线把它与上方的表单字段分开：它是「立即生效」的，不参与底部保存 */
.appearance {
  margin-top: 16px;
  padding-top: 14px;
  border-top: 1px solid var(--divider);
}

.appearance__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.appearance__title {
  font-size: var(--fs-md);
  font-weight: 600;
}

.appearance__actions {
  display: flex;
  gap: 6px;
}

.appearance__btn {
  height: 28px;
  padding: 0 10px;
  font-size: var(--fs-xs);
}

/* 预览是「整窗背景的缩略」：图层 + 遮罩 + 文字样本，三层顺序与真实
   窗口一致（照片 → 遮罩 → 面板）。所以这里看到的就是整窗的样子。 */
.appearance__preview {
  position: relative;
  margin-top: 10px;
  height: 150px;
  overflow: hidden;
  border: 1px solid var(--divider);
  border-radius: var(--r-sm);
  background: var(--bg-sunken);
}

/* 图层要外扩：模糊会把边缘晕成半透明，不外扩就会看到一圈亮边 */
.appearance__preview__layer {
  position: absolute;
  inset: -16px;
  background-position: center;
  background-size: cover;
  background-repeat: no-repeat;
}

.appearance__preview__scrim {
  position: absolute;
  inset: 0;
  background: #000;
}

/* 文字样本用真正的 --bg-panel：设了背景图之后它已经是半透明的了，
   于是这一小块就是「面板压在照片上」的真实观感。 */
.appearance__preview__chip {
  position: absolute;
  left: 10px;
  bottom: 10px;
  padding: 4px 9px;
  border: 1px solid var(--divider);
  border-radius: var(--r-xs);
  background: var(--bg-panel);
  color: var(--text-primary);
  font-size: var(--fs-xs);
  line-height: 1.4;
}

.appearance__hint {
  margin: 8px 0 0;
}

.appearance__hint--tight {
  margin-top: 4px;
}

.appearance__slider {
  display: grid;
  grid-template-columns: 92px minmax(0, 1fr);
  align-items: center;
  gap: 10px;
  margin-top: 8px;
}

.appearance__slider > span {
  font-size: var(--fs-xs);
  color: var(--text-muted);
  font-family: var(--font-mono);
}

/* 原生 range 在深色主题下默认是浅灰轨道，accent-color 一条就够对齐品牌色 */
.appearance__slider input[type='range'] {
  width: 100%;
  accent-color: var(--accent);
}

.appearance__err {
  margin: 8px 0 0;
  font-size: var(--fs-xs);
  color: var(--danger);
}

/* 真正的文件选择器：display:none 在部分 WebView 上会让 click() 失效 */
.appearance__file {
  position: absolute;
  width: 1px;
  height: 1px;
  opacity: 0;
  pointer-events: none;
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
