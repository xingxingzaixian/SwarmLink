<script setup lang="ts">
/**
 * 共享文件（独立弹框，FR-14）。
 *
 * 布局照产品给的参照页面：左边选共享、右边文件表格、顶上一排动作 + 搜索。
 * 与参照页面的两处【刻意】差异：
 *
 *  1. 「密码」列与「设置密码」按钮 → 「可见性」列与「设置可见性」按钮。
 *     我们没有口令机制，摆一个密码列是骗用户；而「这条共享谁能看」是已有的、
 *     真实生效的那一列，位置也对得上。
 *  2. 「取消共享」作用于【左树选中的那条共享】，而不是表格行：表格行是共享里的
 *     文件条目，按行取消意味着「按文件发布」，而我们的粒度是整条共享 + 可见性。
 *
 * 下载一律是异步受理：返回只代表共享方已受理，进度走 transfer:* 与传输列表。
 */
import { computed, onMounted, ref, watch } from 'vue'

import Icon from '../components/Icon.vue'
import Modal from '../components/Modal.vue'
import { getApi } from '../api'
import { pickShareTarget, type ShareTargetKind } from '../api/dialog'
import { fileSize, listTime } from '../utils/format'
import {
  absolutePath,
  buildShareGroups,
  filterShareItems,
  pathDepth,
  pathDir
} from '../utils/share'
import { useGroupStore } from '../stores/group'
import { usePeersStore } from '../stores/peers'
import { useShareStore } from '../stores/share'
import { useTransferStore } from '../stores/transfer'
import { useUiStore } from '../stores/ui'
import type { Share, ShareItem } from '../api/types'

const shares = useShareStore()
const peers = usePeersStore()
const groups = useGroupStore()
const transfers = useTransferStore()
const ui = useUiStore()

// ---------------------------------------------------------------- 左树

const selectedId = ref('')
const collapsed = ref<Record<string, boolean>>({})

/** 联系人名字用本机目录里的昵称（比摘要里的 ownerName 更新）。 */
const peerNames = computed<Record<string, string>>(() => {
  const out: Record<string, string> = {}
  for (const p of peers.peers) out[p.nodeId] = p.displayName
  return out
})

const tree = computed(() => buildShareGroups(shares.shares, peerNames.value))

const selected = computed<Share | null>(() => (selectedId.value ? shares.byId(selectedId.value) : null))

function toggleGroup(key: string): void {
  collapsed.value = { ...collapsed.value, [key]: !collapsed.value[key] }
}

// ---------------------------------------------------------------- 右表

const items = ref<ShareItem[]>([])
const itemsLoading = ref(false)
const itemsError = ref('')
const checked = ref<string[]>([])
const query = ref('')

const visibleItems = computed(() => filterShareItems(items.value, query.value))

/** 表格里显示的路径：我的共享给绝对路径，他人共享只给相对路径（对方磁盘路径对我们没意义）。 */
function pathOf(s: Share, it: ShareItem): string {
  return s.mine && s.rootPath ? absolutePath(s.rootPath, it.path) : it.path
}

function mtimeOf(it: ShareItem): string {
  return it.mtime ? listTime(it.mtime * 1000) : '—'
}

const allChecked = computed(
  () => visibleItems.value.length > 0 && checked.value.length === visibleItems.value.length
)

function toggleAll(): void {
  checked.value = allChecked.value ? [] : visibleItems.value.map((i) => i.path)
}

async function selectSharing(s: Share): Promise<void> {
  selectedId.value = s.shareId
  await loadItems()
}

async function loadItems(): Promise<void> {
  const s = selected.value
  if (!s) return
  itemsLoading.value = true
  itemsError.value = ''
  checked.value = []
  try {
    items.value = await shares.browse(s.shareId)
  } catch (e: any) {
    // 失败原因原样展示：「共享方不在线」是用户唯一能行动的线索。
    items.value = []
    itemsError.value = String(e?.message ?? e)
  } finally {
    itemsLoading.value = false
  }
}

/** 选中态在数据变化后必须仍然有效：被取消的共享要让出选中位，否则右栏空得莫名其妙。 */
function ensureSelection(): void {
  if (selectedId.value && shares.byId(selectedId.value)) return
  const first = shares.mine[0] ?? shares.remote[0]
  if (!first) {
    selectedId.value = ''
    items.value = []
    return
  }
  void selectSharing(first)
}

// ---------------------------------------------------------------- 工具栏

const busy = ref(false)

const checkedSize = computed(() =>
  items.value.filter((i) => checked.value.includes(i.path)).reduce((sum, i) => sum + i.size, 0)
)

async function unpublish(): Promise<void> {
  const s = selected.value
  if (!s?.mine) return
  busy.value = true
  try {
    await shares.unpublish(s.shareId)
    selectedId.value = ''
    items.value = []
    ui.notify(`已取消共享「${s.name}」`)
  } catch (e: any) {
    ui.notify(String(e?.message ?? e) || '取消共享失败')
  } finally {
    busy.value = false
  }
}

/** 立即刷新：我的共享重扫磁盘（清单变了），他人共享重拉清单（对方可能刚加了文件）。 */
async function refreshNow(): Promise<void> {
  const s = selected.value
  if (!s) return
  busy.value = true
  itemsError.value = ''
  try {
    if (s.mine) {
      const updated = await shares.refreshIndex(s.shareId)
      ui.notify(`已刷新「${updated.name}」：${updated.fileCount} 个条目`)
    }
    await loadItems()
  } catch (e: any) {
    itemsError.value = String(e?.message ?? e)
  } finally {
    busy.value = false
  }
}

async function downloadSelected(): Promise<void> {
  const s = selected.value
  if (!s || s.mine || !checked.value.length) return
  busy.value = true
  try {
    const res = await shares.download(s.shareId, [...checked.value])
    // 后端在受理时就为每个文件落了库（idle + batch_queued），因此立刻刷一次列表
    // 就能看到这一批 —— 用户不必等下一个进度事件才知道点成功了。
    await transfers.refresh()
    ui.notify(
      res.rejected?.length
        ? `已开始下载 ${res.accepted} 个文件，${res.rejected.length} 个被共享方跳过`
        : `已开始下载 ${res.accepted} 个文件，进度见「文件传输」`
    )
  } catch (e: any) {
    ui.notify(String(e?.message ?? e) || '下载请求失败')
  } finally {
    busy.value = false
  }
}

/** 定位：没勾选时打开共享根目录，勾选了就定位到第一个勾选的文件。 */
async function revealLocal(): Promise<void> {
  const s = selected.value
  if (!s?.mine || !s.rootPath) return
  const target = checked.value[0] ? absolutePath(s.rootPath, checked.value[0]) : s.rootPath
  try {
    await (await getApi()).revealFile(target)
  } catch (e: any) {
    // 最常见的失败是「文件已被手动删除」：原样提示，让用户知道该刷新索引了。
    ui.notify(String(e?.message ?? e) || '打开所在目录失败')
  }
}

// ---------------------------------------------------------------- 向导（发布 / 改可见性）

/** '' 表示没开；两种模式共用同一套可见性字段，避免同一组规则写两遍。 */
const wizard = ref<'' | 'publish' | 'visibility'>('')
const publishKind = ref<ShareTargetKind>('any')
const formPath = ref('')
const formName = ref('')
const formVis = ref('users')
const formAllowed = ref<string[]>([])
const formGroup = ref('')
const formBusy = ref(false)
const formError = ref('')
const formPathAvailable = ref(true)

const wizardTitle = computed(() => {
  if (wizard.value === 'visibility') return '设置可见性'
  return publishKind.value === 'file' ? '共享文件' : publishKind.value === 'folder' ? '共享文件夹' : '共享'
})

const wizardSubtitle = computed(() =>
  wizard.value === 'visibility'
    ? '改完立即生效，并会立刻通知可见范围内的人'
    : publishKind.value === 'file'
      ? '选一个文件共享给可见范围内的人'
      : publishKind.value === 'folder'
        ? '选一个文件夹共享给可见范围内的人（含子目录）'
        : '选一个目录或文件共享给可见范围内的人'
)

const groupOptions = computed(() => groups.groups.filter((g) => g.state !== 'dissolved'))

function openWizard(mode: 'publish' | 'visibility', kind: ShareTargetKind = 'any'): void {
  wizard.value = mode
  publishKind.value = kind
  formBusy.value = false
  formError.value = ''
  formAllowed.value = []
  formGroup.value = ''
  if (mode === 'visibility') {
    const s = selected.value
    if (!s) return
    formPath.value = s.rootPath ?? ''
    formName.value = s.name
    formVis.value = s.visibility.type
    formAllowed.value = [...(s.visibility.allowed ?? [])]
    formGroup.value = s.visibility.type === 'group' ? (s.visibility.allowed?.[0] ?? '') : ''
    return
  }
  formPath.value = ''
  formName.value = ''
  formVis.value = 'users'
  formPathAvailable.value = true
}

function closeWizard(): void {
  if (formBusy.value) return
  wizard.value = ''
}

async function chooseTarget(): Promise<void> {
  const res = await pickShareTarget(publishKind.value)
  if (!res.available) {
    // 浏览器预览里没有原生对话框：降级为手填路径（与聊天区的回形针同一套做法）。
    formPathAvailable.value = false
    return
  }
  if (!res.picked) return
  formPath.value = res.paths[0] ?? ''
  if (!formName.value) {
    formName.value = (formPath.value.split(/[\\/]/).filter(Boolean).pop() ?? '').slice(0, 64)
  }
}

const canSubmit = computed(() => {
  if (wizard.value === 'visibility') {
    if (formVis.value === 'users') return formAllowed.value.length > 0
    if (formVis.value === 'group') return !!formGroup.value
    return true
  }
  if (!formPath.value.trim() || !formName.value.trim()) return false
  if (formVis.value === 'users') return formAllowed.value.length > 0
  if (formVis.value === 'group') return !!formGroup.value
  return true
})

async function submitWizard(): Promise<void> {
  if (!canSubmit.value || formBusy.value) return
  formBusy.value = true
  formError.value = ''
  const allowed = formVis.value === 'group' ? [formGroup.value] : [...formAllowed.value]
  try {
    if (wizard.value === 'visibility') {
      const s = selected.value
      if (!s) return
      const updated = await shares.setVisibility(s.shareId, formVis.value, allowed)
      ui.notify(`「${updated.name}」的可见性已改为「${updated.visibilityText}」`)
    } else {
      const created = await shares.publish(
        formPath.value.trim(),
        formName.value.trim(),
        formVis.value,
        allowed
      )
      ui.notify(`已发布共享「${created.name}」`)
      // 发布后直接选中它：用户能立刻确认清单就是自己要共享的东西。
      selectedId.value = created.shareId
      await loadItems()
    }
    wizard.value = ''
  } catch (e: any) {
    formError.value = String(e?.message ?? e)
  } finally {
    formBusy.value = false
  }
}

// ---------------------------------------------------------------- 启动

onMounted(async () => {
  await shares.refresh()
  ensureSelection()
  if (!groups.groups.length) {
    // 群可见性需要群清单；失败不影响其它功能（只是「仅群组」暂时选不出来）。
    await groups.refresh().catch(() => undefined)
  }
})

// 清单被事件驱动刷新后（发布 / 取消 / 刷新索引），选中态要跟着收敛。
watch(() => shares.shares, ensureSelection)
</script>

<template>
  <Modal title="共享文件" :subtitle="`我发布 ${shares.mine.length} 条 · 可见他人 ${shares.remote.length} 条`" :width="1000"
    :height="680" @close="ui.closePanel()">
    <!-- 搜索照参照页面放在标题右侧；但【输入即过滤】，不另设一个只起装饰作用的
         「查找」按钮 —— 一个点了没反应（或必须点）的按钮是纯负担。 -->
    <template #head>
      <label class="search">
        <Icon name="search" :size="14" />
        <input v-model="query" type="text" placeholder="搜索文件…" spellcheck="false" :disabled="!selected" />
        <button v-if="query" class="icon-btn mini" title="清除" @click="query = ''">
          <Icon name="x" :size="12" />
        </button>
      </label>
    </template>

    <div class="share">
      <!-- 左：两级树 -->
      <div class="tree scroll">
        <div v-if="shares.error" class="banner err">{{ shares.error }}</div>

        <template v-for="g in tree" :key="g.key">
          <button class="group" @click="toggleGroup(g.key)">
            <Icon name="chevron" :size="13" class="group__caret" :class="{ folded: collapsed[g.key] }" />
            <span class="group__label nowrap">{{ g.label }}</span>
            <span v-if="g.online !== null" class="dot dot--static" :class="{ online: g.online }" />
            <span class="group__count mono">({{ g.count }})</span>
          </button>

          <template v-if="!collapsed[g.key]">
            <button v-for="s in g.shares" :key="s.shareId" class="node" :class="{ on: selectedId === s.shareId }"
              @click="selectSharing(s)">
              <Icon name="folder" :size="14" class="node__icon" />
              <span class="node__name nowrap">{{ s.name }}</span>
              <span class="tag">{{ s.visibilityText }}</span>
              <span class="node__meta mono faint">{{ s.fileCount }} 项</span>
            </button>
            <div v-if="!g.shares.length" class="node-empty faint small">
              <template v-if="g.key === 'mine'">还没有发布任何共享，点上方「共享文件 / 共享文件夹」开始。</template>
              <template v-else>这个联系人还没有共享。</template>
            </div>
          </template>
        </template>
      </div>

      <!-- 右：工具栏 + 表格 -->
      <div class="main">
        <!-- 按作用对象分组：发布 / 对选中共享的操作 / 对勾选条目的主操作。
             分组而不是平铺在一行，一是窄窗口下换行时会按组整块换（不会出现
             「共享文件」与「件夹」被拆到两行的中间状态），二是让「谁是主操作」一眼可辨。 -->
        <div class="toolbar">
          <div class="grp">
            <button class="btn btn-soft" @click="openWizard('publish', 'file')">
              <Icon name="file" :size="14" />共享文件
            </button>
            <button class="btn btn-soft" @click="openWizard('publish', 'folder')">
              <Icon name="folder" :size="14" />共享文件夹
            </button>
          </div>

          <div class="grp">
            <button class="btn btn-soft" :disabled="!selected?.mine || busy" title="取消这条共享（仅本机发布的共享）"
              @click="unpublish">
              <Icon name="trash" :size="14" />取消共享
            </button>
            <button class="btn btn-soft" :disabled="!selected?.mine" title="改这条共享的可见范围"
              @click="openWizard('visibility')">
              <Icon name="users" :size="14" />设置可见性
            </button>
            <button class="btn btn-soft" :disabled="!selected || busy" title="我的共享＝重扫磁盘；他人共享＝重拉清单" @click="refreshNow">
              <Icon name="refresh" :size="14" />{{ busy ? '处理中…' : '立即刷新' }}
            </button>
          </div>

          <span class="spacer" />

          <button v-if="selected?.mine" class="btn btn-soft" :disabled="!selected.rootPath"
            title="在文件管理器中定位（勾选文件则定位到它，否则打开共享根目录）" @click="revealLocal">
            <Icon name="folder" :size="14" />打开所在目录
          </button>
          <button v-else class="btn btn-primary" :disabled="busy || !checked.length || !selected?.downloadable"
            :title="selected?.downloadable ? '' : '共享方不在线，暂时无法下载'" @click="downloadSelected">
            <Icon name="download" :size="14" />下载所选 ({{ checked.length }})
          </button>
        </div>

        <div v-if="itemsError" class="banner err">{{ itemsError }}</div>

        <template v-if="!selected">
          <div class="empty">
            <div class="empty-title">从左侧选择一条共享</div>
            <div class="faint small">查看它的文件清单，再勾选下载；发布入口在左上角的两个按钮。</div>
          </div>
        </template>

        <template v-else>
          <div class="thead">
            <span class="c-check">
              <input type="checkbox" :checked="allChecked" :disabled="!visibleItems.length" @change="toggleAll" />
            </span>
            <span>名称</span>
            <span class="c-size">大小</span>
            <span class="c-time">时间</span>
            <span>路径</span>
            <span class="c-vis">可见性</span>
          </div>

          <div class="rows scroll">
            <div v-if="itemsLoading" class="hint faint small">正在读取清单…</div>
            <div v-else-if="!items.length" class="hint faint small">这条共享里没有文件。</div>
            <div v-else-if="!visibleItems.length" class="hint faint small">
              没有匹配「{{ query }}」的文件。
            </div>
            <label v-for="it in visibleItems" :key="it.path" class="row">
              <span class="c-check">
                <input v-model="checked" type="checkbox" :value="it.path" />
              </span>
              <span class="c-name nowrap" :style="{ paddingLeft: pathDepth(it.path) * 14 + 'px' }">
                <Icon :name="it.isDir ? 'folder' : 'file'" :size="14" class="row__icon" />
                <span class="faint mono">{{ pathDir(it.path) }}</span>{{ it.name }}
              </span>
              <span class="c-size mono faint">{{ it.isDir ? '—' : fileSize(it.size) }}</span>
              <span class="c-time faint">{{ mtimeOf(it) }}</span>
              <span class="c-path mono faint nowrap" :title="pathOf(selected, it)">
                {{ pathOf(selected, it) }}
              </span>
              <!-- 可见性是整条共享的属性：每行显示同一个值（照参照页面把密码列放在
                   这一列的位置）。用淡色文字而不是 .tag —— 每一行都挂一个绿色药丸
                   只是噪音。 -->
              <span class="c-vis faint">{{ selected.visibilityText }}</span>
            </label>
          </div>

          <div class="foot faint small">
            <span>{{ items.length }} 项<template v-if="checked.length">，已选 {{ checked.length }} 项 · {{
              fileSize(checkedSize) }}</template></span>
            <span class="spacer" />
            <span v-if="selected.mine">这是你发布的共享，无需下载</span>
            <span v-else-if="!selected.online">共享方离线：清单只能浏览，下载会失败</span>
          </div>
        </template>
      </div>
    </div>

    <!-- 向导：发布与改可见性共用一套字段（同一组取值规则只写一遍） -->
    <Modal v-if="wizard" :title="wizardTitle" :subtitle="wizardSubtitle" :width="520"
      :height="wizard === 'visibility' ? 460 : 580" @close="closeWizard">
      <div class="form">
        <label v-if="wizard === 'publish'" class="field">
          <span>{{ publishKind === 'folder' ? '共享的文件夹' : '共享的文件' }}</span>
          <div class="path-row">
            <input v-model="formPath" class="mono" :readonly="formPathAvailable" placeholder="点击右侧按钮选择，或直接粘贴路径" />
            <button v-if="formPathAvailable" class="btn btn-soft" @click="chooseTarget">选择…</button>
          </div>
          <span v-if="!formPathAvailable" class="faint small">
            当前环境没有原生对话框，请手动填写绝对路径。
          </span>
        </label>

        <label v-if="wizard === 'publish'" class="field">
          <span>共享名（他人列表里显示的名字）</span>
          <input v-model="formName" maxlength="64" placeholder="例如：周报模板" />
        </label>

        <div v-else class="field">
          <span>这条共享</span>
          <div class="readonly mono nowrap" :title="formPath">{{ formName }} · {{ formPath }}</div>
        </div>

        <label class="field">
          <span>可见性</span>
          <select v-model="formVis">
            <option value="users">指定人（默认，最安全）</option>
            <option value="all">所有人（任何发现到你的节点都可见）</option>
            <option value="group">仅群组（仅该群在群成员可见）</option>
          </select>
        </label>

        <div v-if="formVis === 'users'" class="field">
          <span>允许访问的节点（可多选）</span>
          <div v-if="!peers.peers.length" class="faint small">暂无可选节点。</div>
          <div v-else class="picks">
            <label v-for="p in peers.peers" :key="p.nodeId" class="pick">
              <input v-model="formAllowed" type="checkbox" :value="p.nodeId" />
              <span class="nowrap">{{ p.displayName }}</span>
              <span v-if="!p.online && !p.connected" class="tag">离线</span>
            </label>
          </div>
        </div>

        <div v-if="formVis === 'group'" class="field">
          <span>可见的群</span>
          <div v-if="!groupOptions.length" class="faint small">还没有群，先建一个群再选它。</div>
          <select v-else v-model="formGroup">
            <option value="">请选择…</option>
            <option v-for="g in groupOptions" :key="g.groupId" :value="g.groupId">
              {{ g.name }}（{{ g.members.length }} 人）
            </option>
          </select>
        </div>

        <!-- 发布前确认：把「谁可能看到什么」讲到用户能自己判断的程度 -->
        <div v-if="wizard === 'publish'" class="warn">
          <div class="warn__title">
            <Icon name="alert" :size="14" />发布前请确认
          </div>
          <ul>
            <li>
              范围：<span class="mono">{{ formPath || '（未选择）' }}</span> 下的<span>全部内容</span>（递归包含子目录）。
            </li>
            <li>
              可见者：{{
                formVis === 'all'
                  ? '任何发现到你的节点'
                  : formVis === 'group'
                    ? '所选群的在群成员'
                    : `你点名的 ${formAllowed.length} 个节点`
              }}。
            </li>
            <li>共享方离线时对方无法下载（本项目不做服务器托管）；共享内容始终只在你本机。</li>
            <li>发布后可随时改可见性、刷新索引或取消共享，且不会换掉这条共享的标识。</li>
          </ul>
        </div>

        <div v-if="formError" class="err">{{ formError }}</div>
      </div>

      <template #head>
        <button class="btn btn-primary" :disabled="formBusy || !canSubmit" @click="submitWizard">
          {{ formBusy ? '处理中…' : wizard === 'visibility' ? '保存' : '确认发布' }}
        </button>
      </template>
    </Modal>
  </Modal>
</template>

<style scoped>
.share {
  display: grid;
  grid-template-columns: 268px minmax(0, 1fr);
  height: 100%;
  min-height: 0;
}

/* 标题右侧的搜索：与联系人列表的搜索同一套观感 */
.search {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 220px;
  height: 30px;
  padding: 0 8px;
  border: 1px solid var(--divider);
  border-radius: var(--r-sm);
  background: var(--bg-elevated);
  color: var(--text-muted);
}

.search input {
  flex: 1 1 auto;
  min-width: 0;
  border: 0;
  background: transparent;
  font-size: var(--fs-sm);
}

.search input:focus {
  outline: none;
}

.icon-btn.mini {
  width: 20px;
  height: 20px;
}

/* ---------------------------------------------------------------- 左树 */

.tree {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-height: 0;
  padding: 0 8px;
  border-right: 1px solid var(--divider);
}

.group {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  padding: 7px 8px 5px;
  color: var(--text-muted);
  font-size: var(--fs-xs);
  letter-spacing: 0.04em;
}

.group__label {
  flex: 1 1 auto;
  min-width: 0;
  text-align: left;
  text-transform: uppercase;
}

.group__caret {
  transition: transform var(--dur-1) var(--ease);
}

.group__caret.folded {
  transform: rotate(-90deg);
}

.group__count {
  font-size: var(--fs-xs);
}

.node {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  padding: 7px 8px;
  border: 1px solid transparent;
  border-radius: var(--r-md);
  text-align: left;
}

.node:hover {
  background: var(--bg-hover);
}

.node.on {
  border-color: var(--accent);
  background: var(--accent-soft);
}

.node__icon {
  flex: 0 0 auto;
  color: var(--text-secondary);
}

.node__name {
  flex: 1 1 auto;
  min-width: 0;
  font-size: var(--fs-sm);
  font-weight: 500;
}

.node__meta {
  flex: 0 0 auto;
  font-size: var(--fs-xs);
}

.node-empty {
  padding: 2px 10px 8px;
  line-height: 1.6;
}

/* ---------------------------------------------------------------- 右栏 */

.main {
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.toolbar {
  display: flex;
  align-items: center;
  /* 允许换行：窗口压到最窄时（modal 宽 = 视口 - 48px）这一排放不下，
     不允许换行的话按钮会被压缩到文字被裁掉。 */
  flex-wrap: wrap;
  gap: 6px;
  flex: 0 0 auto;
  padding: 12px 16px;
  border-bottom: 1px solid var(--divider);
}

.grp {
  display: flex;
  align-items: center;
  gap: 6px;
  /* 换行时整组一起走，不会把「共享文件」和「共享文件夹」拆到两行 */
  flex: 0 0 auto;
}

.spacer {
  flex: 1 1 0;
}

.banner {
  margin: 10px 16px 0;
  border-radius: var(--r-md);
  border-bottom: 0;
}

.empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 6px;
  flex: 1 1 auto;
  text-align: center;
}

/* 表格：用 grid 让表头与数据行共用同一套列宽（分开写迟早对不齐） */
.thead,
.row {
  display: grid;
  grid-template-columns: 30px minmax(0, 1fr) 84px 92px minmax(0, 1.1fr) 96px;
  align-items: center;
  gap: 8px;
}

.thead {
  flex: 0 0 auto;
  padding: 8px 16px;
  border-bottom: 1px solid var(--divider);
  color: var(--text-muted);
  font-size: var(--fs-xs);
  letter-spacing: 0.04em;
}

.rows {
  flex: 1 1 auto;
  min-height: 0;
}

.row {
  padding: 6px 16px;
  border-bottom: 1px solid var(--divider);
  font-size: var(--fs-sm);
  cursor: default;
}

.row:hover {
  background: var(--bg-hover);
}

.c-check {
  display: flex;
  align-items: center;
  justify-content: center;
}

.c-name {
  display: flex;
  align-items: center;
  gap: 5px;
  min-width: 0;
}

.row__icon {
  flex: 0 0 auto;
  color: var(--text-secondary);
}

.c-size,
.c-time,
.c-vis {
  text-align: right;
}

.c-path {
  min-width: 0;
  direction: rtl;
  text-align: left;
}

.hint {
  padding: 14px 16px;
}

.foot {
  display: flex;
  align-items: center;
  flex: 0 0 auto;
  padding: 8px 16px;
  border-top: 1px solid var(--divider);
}

/* ---------------------------------------------------------------- 向导 */

.form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
  font-size: var(--fs-sm);
}

.field>span {
  color: var(--text-secondary);
}

.readonly {
  padding: 7px 9px;
  border: 1px solid var(--divider);
  border-radius: var(--r-sm);
  background: var(--bg-sunken);
  color: var(--text-secondary);
}

.path-row {
  display: flex;
  gap: 8px;
}

.path-row input {
  flex: 1 1 auto;
  min-width: 0;
}

.picks {
  display: flex;
  flex-direction: column;
  gap: 4px;
  max-height: 150px;
  overflow: auto;
}

.pick {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 6px;
  border-radius: var(--r-sm);
  cursor: pointer;
}

.pick:hover {
  background: var(--bg-hover);
}

.pick .tag {
  margin-left: auto;
}

.warn {
  padding: 10px 12px;
  border: 1px solid var(--warn);
  border-radius: var(--r-md);
  background: color-mix(in oklch, var(--warn) 10%, transparent);
}

.warn__title {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-bottom: 6px;
  font-weight: 600;
  font-size: var(--fs-sm);
}

.warn ul {
  margin: 0;
  padding-left: 18px;
  font-size: var(--fs-xs);
  line-height: 1.75;
  color: var(--text-secondary);
}

.err {
  color: var(--danger);
  font-size: var(--fs-sm);
}

/* 次级文案：`.faint`（全局，上色）+ `.small`（本仓由各组件自己定义字号）。 */
.small {
  font-size: var(--fs-sm);
}
</style>
