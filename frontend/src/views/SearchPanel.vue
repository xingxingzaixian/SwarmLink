<script setup lang="ts">
/**
 * 搜索聊天记录（整窗两栏弹层，ADR-013）。
 *
 * 为什么是弹层而不是右栏：搜索是【跨会话】的全局能力 —— 它天生要同时看到
 * 所有会话的命中，而右栏要留给「对方的资料」和「正在发的文件」。这与设置 /
 * 共享中心是同一类东西，因此复用同一套弹层机制（ui.PanelKind）。
 *
 * 四条不显眼但必须守住的规则：
 *
 *  1. **不做命中计数**。后端刻意不返回总数（20 万条消息下 count(*) 是 213ms、
 *     按会话分组是 1.9s，见 ADR-013 的实测表），因此界面只能说「还有更早的」，
 *     不能说「共 N 条」—— 那个数字没有便宜的来源。左栏同理，一个数字都不显示。
 *
 *  2. **左栏由已加载的结果派生**，不额外查询：按 convId 分组取各组最大 sentAt
 *     作为「最近命中时间」倒序。代价是它随滚动增长 —— 这正是诚实的地方：
 *     它回答的是「你翻到的结果里有哪些会话」，而不是一个需要全表扫描的答案。
 *
 *  3. **竞态必须用请求序号守卫**。Wails 调用不可取消，连打几个字就有多个查询
 *     在飞；旧响应晚到若直接写进 results，界面会跳回上一次的关键词。
 *     序号 +加载中禁止翻页，两个一起才够。
 *
 *  4. **大小写规则与后端 LIKE 一致**（utils/search 的 ASCII-only 折叠）：
 *     后端只回原文，高亮的那一处是前端按关键词定位的，两边规则不一致时
 *     会出现「搜到了但一个字都没高亮」。
 */
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'

import Avatar from '../components/Avatar.vue'
import Icon from '../components/Icon.vue'
import Modal from '../components/Modal.vue'
import { getApi } from '../api'
import type { Message } from '../api/types'
import { useChatStore } from '../stores/chat'
import { usePeersStore } from '../stores/peers'
import { useUiStore } from '../stores/ui'
import { bubbleTime, listTime } from '../utils/format'
import { previewText } from '../utils/message'
import { indexOfFoldAscii } from '../utils/search'

const chat = useChatStore()
const peers = usePeersStore()
const ui = useUiStore()

/** 单页条数：与后端 ports.DefaultSearchLimit 同值（改一处就要改两处）。 */
const PAGE = 50
/** 输入防抖：短到感觉不到，长到能把一次连打收敛成一个查询。 */
const DEBOUNCE_MS = 250
/** 片段里命中词左右各留的字符数。 */
const CONTEXT = 30

const query = ref(ui.searchSeed)
const queryEl = ref<HTMLInputElement | null>(null)

/** 收窄到的会话；空串 = 全部会话（后端此时用「可见会话集合」）。 */
const activeConvId = ref('')

const results = ref<Message[]>([])
const hasMore = ref(false)
const loading = ref(false)
const error = ref('')

/**
 * 请求序号：每发起一次查询自增，响应回来时对不上就丢弃。
 *
 * 这是本组件唯一处理竞态的手段，且必须由【发号】与【验号】两半配合 ——
 * 只在成功分支验号不够：失败分支同样会把旧错误写到界面上。
 */
let seq = 0
let timer: number | undefined

async function run(reset: boolean): Promise<void> {
  const text = query.value.trim()
  if (!text) {
    // 清空关键词：立刻收起结果，并让在飞的请求作废
    seq++
    results.value = []
    hasMore.value = false
    error.value = ''
    loading.value = false
    return
  }
  if (!reset && loading.value) return

  const my = ++seq
  loading.value = true
  if (reset) error.value = ''

  try {
    const api = await getApi()
    // 游标取「已加载的最后一条」（结果整体是新→旧，所以它就是最旧的那条）。
    // 首页必须传空串 —— 后端空串才是首页语义，传一个假游标会得到空结果。
    const cursor = reset ? '' : (results.value[results.value.length - 1]?.msgId ?? '')
    const page = await api.searchHistory(
      text,
      activeConvId.value ? [activeConvId.value] : [],
      PAGE,
      cursor
    )
    if (my !== seq) return
    results.value = reset ? page : [...results.value, ...page]
    // 只有「满一页」才可能还有下一页：后端不返回总数，这是唯一可用的判据。
    hasMore.value = page.length >= PAGE
  } catch (e: any) {
    if (my !== seq) return
    error.value = String(e?.message ?? e)
    hasMore.value = false
  } finally {
    // 只有仍是当前请求才解除 loading：否则会把后发的那次查询标成「不在加载中」
    if (my === seq) loading.value = false
  }
}

watch(query, () => {
  if (timer) window.clearTimeout(timer)
  timer = window.setTimeout(() => void run(true), DEBOUNCE_MS)
})

// 左栏筛选不走防抖：它是点击而不是连打，延迟只会让人觉得「点了没反应」。
// 但要把还没到点的防抖任务取消掉 —— 否则刚打完字就点筛选，会在 250ms 后
// 再重复查一次同一个组合（结果正确，只是白发一次请求）。
watch(activeConvId, () => {
  if (timer) window.clearTimeout(timer)
  void run(true)
})

onMounted(() => {
  queryEl.value?.focus()
  if (query.value.trim()) void run(true)
})

onBeforeUnmount(() => {
  if (timer) window.clearTimeout(timer)
})

function loadMore(): void {
  void run(false)
}

// ---------------------------------------------------------------- 左栏（筛选维度）

interface Facet {
  convId: string
  /** 该会话最近一条命中的时间。 */
  at: number
}

const facets = computed<Facet[]>(() => {
  const out = new Map<string, number>()
  for (const m of results.value) {
    const at = out.get(m.convId) ?? 0
    if (m.sentAt > at) out.set(m.convId, m.sentAt)
  }
  // 收窄之后结果里只剩这一个会话，它当然在上面；但「零命中」时会浮不上来，
  // 那样左栏就选不中任何东西，用户看不到自己在看哪个会话 —— 补一条进去。
  if (activeConvId.value && !out.has(activeConvId.value)) out.set(activeConvId.value, 0)

  return [...out]
    .map(([convId, at]) => ({ convId, at }))
    .sort((a, b) => b.at - a.at)
})

function pickConv(convId: string): void {
  activeConvId.value = convId
}

// ---------------------------------------------------------------- 会话名与发言人

/**
 * 会话名由【已有的 conversations store】解析，而不是后端在每条结果里带标题。
 *
 * 理由：搜索结果就是 MessageDTO（后端不为它另造 DTO），而会话名在左栏本来就
 * 有一份最新的 —— 让后端为每条命中重复算一次名字，只会多一处会不一致的来源。
 * 取不到时回退「未知会话」：那通常意味着对端已被本地移除（它的历史仍在库里）。
 */
const titles = computed<Record<string, string>>(() => {
  const out: Record<string, string> = {}
  for (const c of chat.conversations) out[c.convId] = c.title
  return out
})

function titleOf(convId: string): string {
  return titles.value[convId] || '未知会话'
}

function whoOf(m: Message): string {
  return m.direction === 'out' ? '我' : peers.nameOf(m.senderId)
}

// ---------------------------------------------------------------- 命中片段

interface Part {
  text: string
  hit: boolean
}

/**
 * 把一条命中压成「关键词周围一小段」。
 *
 * 为什么要用 previewText 而不是 m.content：图片 / 文件消息的 content 是 JSON
 * 信封（实测里有 base64），直接拿去当片段就是一段裸 JSON —— 这正是这个项目
 * 反复出现过的事故形态。文件和文件夹消息的文件名在 previewText 里仍然可见，
 * 因此「搜文件名命中」依然高亮得出来。
 *
 * 只高亮【首处】：一条消息里同一个词出现十次时逐个包 mark 既慢又难看，
 * 而且用户要的是「为什么这条命中了」，一处就够。
 */
function snippetOf(m: Message): Part[] {
  const text = previewText(m)
  const needle = query.value.trim()
  const i = needle ? indexOfFoldAscii(text, needle) : -1

  if (i < 0) {
    // 命中在 JSON 里、而片段用的是人话版本（例如搜 base64 里的一段）：不编造
    // 高亮，只给开头一小段，用户仍能看到这条是什么。
    const head = text.slice(0, CONTEXT * 2)
    return [{ text: text.length > head.length ? `${head}…` : head, hit: false }]
  }

  const from = Math.max(0, i - CONTEXT)
  const to = Math.min(text.length, i + needle.length + CONTEXT)
  const parts: Part[] = []
  if (from > 0) parts.push({ text: '…', hit: false })
  parts.push({ text: text.slice(from, i), hit: false })
  parts.push({ text: text.slice(i, i + needle.length), hit: true })
  parts.push({ text: text.slice(i + needle.length, to), hit: false })
  if (to < text.length) parts.push({ text: '…', hit: false })
  return parts.filter((p) => p.text !== '')
}

// ---------------------------------------------------------------- 打开与提示

/**
 * 打开命中所在的会话并定位到这一条（P3）。
 *
 * openAt 会取「以该条为中心的一页」替换聊天区，再由 ChatView 滚到视口中央
 * 并短暂高亮 —— 消息可能在几千条之前，靠「打开会话看最新一页」是找不到的。
 *
 * 会话不在列表里（对端已被本地移除）时不静默失败：给出提示，
 * 否则用户会以为「点了没反应」。
 */
async function openResult(m: Message): Promise<void> {
  const conv = chat.conversations.find((c) => c.convId === m.convId)
  if (!conv) {
    ui.notify('该会话已不在会话列表中（历史消息仍在）')
    return
  }
  ui.closePanel()
  await chat.openAt(conv, m.msgId)
}

const subtitle = computed(() => {
  if (!query.value.trim()) return '按子串匹配（中文两个字即可），只搜本机保存的消息'
  const n = results.value.length
  if (!n) return '没有命中'
  return hasMore.value ? `已显示最近 ${n} 条 · 还有更早的` : `已显示 ${n} 条`
})
</script>

<template>
  <Modal
    title="搜索聊天记录"
    :subtitle="subtitle"
    :width="960"
    :height="640"
    @close="ui.closePanel()"
  >
    <template #head>
      <label class="find">
        <Icon name="search" :size="14" />
        <input
          ref="queryEl"
          v-model="query"
          type="text"
          placeholder="在全部会话里查找…"
          spellcheck="false"
        />
        <button
          v-if="query"
          class="icon-btn mini"
          title="清除"
          aria-label="清除关键词"
          @click="query = ''"
        >
          <Icon name="x" :size="12" />
        </button>
      </label>
    </template>

    <div class="panel">
      <!-- 左：会话筛选。没有计数 —— 按会话统计命中数在全量数据上是 1.9s（ADR-013）。 -->
      <aside class="facets scroll">
        <button class="facet" :class="{ on: !activeConvId }" @click="pickConv('')">
          <span class="facet__glyph"><Icon name="hash" :size="14" /></span>
          <span class="facet__name nowrap">全部会话</span>
        </button>

        <button
          v-for="f in facets"
          :key="f.convId"
          class="facet"
          :class="{ on: activeConvId === f.convId }"
          :title="titleOf(f.convId)"
          @click="pickConv(f.convId)"
        >
          <Avatar :seed="f.convId" :name="titleOf(f.convId)" :size="26" :online="null" />
          <span class="facet__name nowrap">{{ titleOf(f.convId) }}</span>
          <span class="facet__time mono">{{ f.at ? listTime(f.at) : '' }}</span>
        </button>

        <div v-if="!facets.length" class="facet-hint faint small">
          命中过的会话会列在这里，点一个即可只看它。
        </div>
      </aside>

      <!-- 右：结果（新 → 旧，与后端一致）。 -->
      <section class="hits scroll">
        <div v-if="error" class="banner err">{{ error }}</div>

        <div v-else-if="!query.trim()" class="empty">
          <div class="empty-title">搜索本机聊天记录</div>
          <div>
            关键词按子串匹配、英文大小写不敏感。<br />
            被清空过的消息不会出现在结果里（与聊天区看到的一致）。
          </div>
        </div>

        <div v-else-if="loading && !results.length" class="empty">
          <div>正在搜索…</div>
        </div>

        <div v-else-if="!results.length" class="empty">
          <div class="empty-title">没有命中</div>
          <div>试试更短的关键词；英文只忽略大小写，不做拼写纠错。</div>
        </div>

        <template v-else>
          <!-- 行用 div 而不是 button（与左栏 .list-row 同一套做法）：
               button 的内容模型只允许 phrasing content，而每行带头像（div）
               与两段文本，塞进 button 里是一份不合法的 HTML。 -->
          <div
            v-for="m in results"
            :key="m.msgId"
            class="hit"
            :title="`${titleOf(m.convId)} · ${whoOf(m)} · ${bubbleTime(m.sentAt)}`"
            @click="openResult(m)"
          >
            <Avatar :seed="m.convId" :name="titleOf(m.convId)" :size="32" :online="null" />
            <div class="hit__main">
              <div class="hit__head">
                <span class="hit__conv nowrap">{{ titleOf(m.convId) }}</span>
                <span class="hit__who nowrap">{{ whoOf(m) }}</span>
                <span class="hit__time mono">{{ bubbleTime(m.sentAt) }}</span>
              </div>
              <div class="hit__text">
                <template v-for="(p, i) in snippetOf(m)" :key="i">
                  <mark v-if="p.hit">{{ p.text }}</mark>
                  <template v-else>{{ p.text }}</template>
                </template>
              </div>
            </div>
          </div>

          <button v-if="hasMore" class="btn btn-soft more" :disabled="loading" @click="loadMore">
            {{ loading ? '加载中…' : '加载更早的命中' }}
          </button>
          <div v-else class="tail faint small">没有更早的命中了</div>
        </template>
      </section>
    </div>
  </Modal>
</template>

<style scoped>
/* 标题右侧的搜索：与共享中心、联系人列表同一套观感 */
.find {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 300px;
  height: 30px;
  padding: 0 8px;
  border: 1px solid var(--divider);
  border-radius: var(--r-sm);
  background: var(--bg-elevated);
  color: var(--text-muted);
}

.find:focus-within {
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft);
}

.find input {
  flex: 1 1 auto;
  min-width: 0;
  border: 0;
  background: transparent;
  font-size: var(--fs-sm);
}

.find input:hover,
.find input:focus {
  background: transparent;
  border-color: transparent;
  box-shadow: none;
}

.icon-btn.mini {
  width: 20px;
  height: 20px;
}

/* ---------------------------------------------------------------- 两栏 */

.panel {
  display: grid;
  grid-template-columns: 216px minmax(0, 1fr);
  height: 100%;
  min-height: 0;
}

.facets {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-height: 0;
  padding: 0 8px;
  border-right: 1px solid var(--divider);
}

.facet {
  display: flex;
  align-items: center;
  gap: 7px;
  width: 100%;
  padding: 6px 8px;
  border: 1px solid transparent;
  border-radius: var(--r-md);
  text-align: left;
  transition: background var(--dur-1) var(--ease), border-color var(--dur-1) var(--ease);
}

.facet:hover {
  background: var(--bg-hover);
}

.facet.on {
  border-color: var(--accent);
  background: var(--accent-soft);
}

/* 「全部会话」没有头像，给它一个等宽等高的占位容器，
   否则这一行会比其它行矮半格 */
.facet__glyph {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  flex: 0 0 auto;
  width: 26px;
  height: 26px;
  border-radius: var(--r-xs);
  background: var(--bg-sunken);
  color: var(--text-secondary);
}

.facet__name {
  flex: 1 1 auto;
  min-width: 0;
  font-size: var(--fs-sm);
  color: var(--text-primary);
}

.facet__time {
  flex: 0 0 auto;
  color: var(--text-muted);
}

.facet-hint {
  padding: 6px 8px;
  line-height: 1.6;
}

/* ---------------------------------------------------------------- 结果 */

.hits {
  min-height: 0;
  padding: 0 4px 0 12px;
}

.hit {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 9px 10px;
  cursor: pointer;
  border: 1px solid transparent;
  border-radius: var(--r-md);
  text-align: left;
  transition: background var(--dur-1) var(--ease), border-color var(--dur-1) var(--ease);
}

.hit:hover {
  background: var(--bg-hover);
  border-color: var(--divider);
}

.hit__main {
  flex: 1 1 auto;
  min-width: 0;
}

.hit__head {
  display: flex;
  align-items: baseline;
  gap: 8px;
  margin-bottom: 3px;
}

.hit__conv {
  flex: 0 1 auto;
  font-family: var(--font-display);
  font-size: var(--fs-md);
  font-weight: 600;
  color: var(--text-primary);
}

.hit__who {
  flex: 0 1 auto;
  font-size: var(--fs-xs);
  color: var(--text-muted);
}

/* 时间推到最右：用 margin-left:auto 而不是 justify-content: space-between，
   否则「会话名 + 发言人」会被拆到两端，读起来像两列 */
.hit__time {
  flex: 0 0 auto;
  margin-left: auto;
  color: var(--text-muted);
}

.hit__text {
  font-size: var(--fs-sm);
  color: var(--text-secondary);
  line-height: 1.55;
  overflow-wrap: anywhere;
}

/* 高亮只上底色，不改字重：一改字重整行会重排，滚动时看起来在抖 */
.hit__text mark {
  padding: 0 1px;
  border-radius: 2px;
  background: var(--accent-soft);
  color: var(--accent);
  font-weight: 600;
}

/* .btn 是 inline-flex，`margin: auto` 对行内级盒子不居中，因此这里改回块级 */
.more {
  display: flex;
  margin: 10px auto 4px;
}

.tail {
  padding: 12px 0 4px;
  text-align: center;
}
</style>
