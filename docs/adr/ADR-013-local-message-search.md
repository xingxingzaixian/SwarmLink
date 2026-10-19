# ADR-013：本机消息历史搜索用 LIKE 尾优先倒扫，不用 FTS5

**Status**：Accepted（P1 后端 / P2 搜索界面 / P3 跳转定位 均已落地）
**来源**：新需求（架构书未覆盖）；实测数据由本机探针与 `BenchmarkSearch200k` 取得

## Context

需求：在本机消息历史里全局搜索（左栏按会话筛选命中，点一条结果 → 打开会话 +
滚动到那一条 + 短暂高亮）。**明确不做共享文件搜索**。

数据规模基准：20 个会话 / 20 万条消息 / 38~44MB 库，SQLite 3.53.4 +
纯 Go 驱动 v1.59.0 + 生产 DSN（WAL / busy_timeout / synchronous=NORMAL / 5 连接）。

三条实测事实决定了方案：

1. **FTS5 不能用**。驱动确实支持 FTS5 与 trigram 分词器，但
   `trigram` 要求查询词 **≥ 3 个字符**：中文两字词（`会议` / `照片` / `周报`）
   会**静默返回 0 命中，不报错**；`unicode61` 对 CJK 完全无效（整段当一个 token）。
2. **LIKE 全扫的代价取决于分页方式**：`ORDER BY id DESC LIMIT 50` 的计划是
   `SCAN messages`（**无 TEMP B-TREE**）——从 rowid 尾部倒扫，凑够 50 条即停。
3. **凡是需要「扫完」的操作都贵**：`count(*)` 213ms、罕见词/零命中 175~226ms、
   `GROUP BY conv_id` 每会话计数 **1.9s**。

## Decision

### 1. 语料与匹配：LIKE 子串，不用 FTS5

`content LIKE '%kw%' ESCAPE '\'`。天然支持任意长度子串、两字中文；
ASCII 默认忽略大小写（SQLite 只对 ASCII 折叠，这一点被当作契约，
内存实现用自写的 `likeContains` 而不是 `strings.ToLower` 来对齐）。

LIKE 元字符（`\` `%` `_`）**必须转义**：漏掉时搜一个 `%` 会命中全库。

### 2. 分页：尾优先倒扫 + 消息 ID 游标，且**禁止计数**

```sql
SELECT ... FROM messages m NOT INDEXED
LEFT JOIN conv_clears c ON c.conv_id = m.conv_id
WHERE m.content LIKE ? ESCAPE '\'
  AND (c.cleared_at IS NULL OR m.sent_at > c.cleared_at)
  AND m.id < (SELECT id FROM messages WHERE msg_id = ?)   -- 仅非首页
  AND m.conv_id IN (?,...)
ORDER BY m.id DESC LIMIT ?
```

- **游标是 `msg_id` 而不是时间戳**：同一毫秒写入多条是常态（连发、群发、
  入站消息沿用发送方时间戳），按 `sent_at` 翻页会把边界上同毫秒的其它命中整页丢掉。
  实现内部把 `msg_id` 换算成写入序（sqlite 的 rowid / mem 的写入序号 `seq`），
  因此游标不泄漏存储细节，两个实现也不会分叉。
- **纪律：不查命中总数，不做每会话计数。** 于是左栏改为「按已加载结果派生
  最近命中时间」，不显示数字（Slack 行为）。

#### 2.1 `NOT INDEXED` 是必需项，不是调优提示（重要）

`FROM messages m NOT INDEXED` 看起来像多余的优化提示，实际上它是一条**正确性级别的
约束**：它把计划钉在「rowid 倒扫」上，因为规划器在**没有统计信息**时会选错。

实测（20 万条 / 20 个会话，同一句 SQL）：

| 规划器输入 | 计划 | 首屏耗时 |
|---|---|---|
| **无统计信息（本应用的常态）** | `SEARCH m USING INDEX ix_msg_conv_time` + `USE TEMP B-TREE FOR ORDER BY` | **543ms** |
| 有统计信息（手动 `ANALYZE` 过） | `SCAN messages` | 0.3ms |

为什么会这样：无统计信息时规划器对 `conv_id IN (20 个值)` 做了乐观估计，以为按
`conv_id` 走索引划算 —— 于是每个会话都扫一遍索引，再为 `ORDER BY id DESC`
建临时表排序。而 `LIMIT` 在排序之后才生效，等于必须把所有命中都读出来（20 万条
里约 1.2 万条）。

**本应用从不 `ANALYZE`**（全仓只有测试里出现过这个词），所以生产上跑的就是那条
543ms 的路。`NOT INDEXED` 让两种输入下都是同一个快计划，不再依赖统计信息是否新鲜。

> **教训（写测试的方式）**：第一版计划守卫先 `ANALYZE` 再断言，于是完整地漏掉了
> 这个缺陷 —— 而探针当初也是 ANALYZE 之后测的，才得出「1ms」的结论。
> **测性能必须在与生产相同的规划器输入下测**；`internal/adapters/store/sqlite`
> 的 `TestQueryPlansArePinned` 现在会分别在「无统计信息」与「ANALYZE 后」两个
> 状态下各断言一次。

### 3. 清空点（`conv_clears`）必须在 SQL 里过滤

判据与前端 `stores/chat.ts` 的 `activeMessages` **逐字一致**：
`sent_at > cleared_at` 才可见（严格大于）。取回后再过滤会让每页条数不定、
翻出空白页；而差一个等号就是「界面上没有、搜索能搜到」的隐私回退。
实测这道 JOIN **零成本**（1.1ms vs 1.0ms）。

### 4. 端口：`Search` + `Window`

```go
Search(MessageSearchQuery) ([]message.Message, error)
Window(convID, msgID string, before, after int) ([]message.Message, error)
```

`Window` 是跳转定位（命中 → 停在那一条）的唯一入口，它两侧的边界是
**行值 `(sent_at, id)`**，并显式 `INDEXED BY ix_msg_conv_time`：

```sql
-- 旧侧（含锚点）
WHERE m.conv_id = ? AND (m.sent_at, m.id) <= (?, ?)
ORDER BY m.sent_at DESC, m.id DESC LIMIT ?
-- 新侧
WHERE m.conv_id = ? AND (m.sent_at, m.id) >  (?, ?)
ORDER BY m.sent_at ASC,  m.id ASC  LIMIT ?
```

三条理由，逐条都有实测撑着：

1. **为什么边界要带 `id`（而不是只用 `sent_at`）**：同一毫秒里可能有多条（群发、
   连发），只按 `sent_at` 分界时锚点与邻居互踩，**命中可能被挤出窗口** —— 那不是
   显示瑕疵，是「点了搜索结果却跳不到那条消息」。带 `id` 后边界唯一，两侧互补。
2. **为什么不用 rowid 当边界**（曾经这么写）：实测在「某个会话的消息在物理上连成
   一块」的数据下（一个重度用户连着聊两小时就会出现），按 rowid 回溯必须扫过相邻
   会话的整块消息才能凑够 `before` 条。20 万条时，跳到该会话头部要 **65ms**，且
   代价是 O(全表)。改用 `(sent_at,id)` 后两侧都变成索引范围，**与全表大小无关**
   （实测：头部/中部/尾部 <1ms / 1ms / <1ms）。
3. **为什么 `ORDER BY` 与 `Latest` 一致**：`(sent_at, id)` 就是本应用的消息显示序
   （`domain.SortInPlace`、`Latest` 的 `ORDER BY sent_at DESC, id DESC`、前端渲染），
   且恰好是 `ix_msg_conv_time(conv_id, sent_at DESC, id DESC)` 的索引序 ——
   不建临时表。

其余契约：白名单 `ConvIDs` 为空时返回空结果（**不退化成全库**）；锚点不存在 /
不属于该会话 / 已被清空点时 `Window` 返回空切片且不报错（调用方据此回退到最新）。

### 5. 会话白名单由入站适配器派生

`wails.SearchService` 从「节点目录（默认排除 blocked）+ 群列表」派生，
与 `Conversations()` 同源。**已解散的群仍然计入**：会话列表里它还在、
历史也还打得开，从搜索里抹掉只会变成「我记得那条消息但找不回来」。

### 6. 不新增索引、不新增迁移

`LIKE '%kw%'` 用不上任何索引；分页用 `INTEGER PRIMARY KEY`；窗口与清空点都走
既有索引。迁移只追加且带 checksum，本需求不需要动它。

### 7. 内存实现必须显式维护「写入序」

`mem` 侧新增 `seq`（单调递增）与 `likeContains`（ASCII-only 折叠），
否则它无法与 rowid / `LIKE` 的语义对等。`internal/adapters/store/parity` 用同一套
场景对拍两个实现（新包，因为 R2 红线不允许适配器互相 import）。

### 8. 前端：搜索是「整窗两栏弹层」，不是右栏，也不是新页面

落点：`stores/ui.ts` 的 `PanelKind` 加一项 `'search'`，界面是
`views/SearchPanel.vue`（复用既有 `Modal`，960×640，两栏）。

为什么不是右栏：搜索是**跨会话**的能力，而右栏的语义一直是「**当前**会话的对方资料 +
正在传的文件」（ADR-009 那条「在线 ≠ 连接」的展示就挂在它上面）。把一个跨会话的
列表塞进「当前会话」的容器里，本身就是语义错位；顺带还会把传输进度挤没。

为什么不是新页面/路由：本应用是单窗口三栏（左栏会话 / 中栏聊天 / 右栏资料），
没有路由层。全局功能（设置、共享中心、调试、传输）一律走 `ui.panel` 这套弹层机制，
搜索加入它是最短的路，也让「`Esc` 关闭 / 点遮罩关闭 / 同一时刻只开一个弹层」
这些行为自动一致。

必须守住的四条界面纪律：

1. **不显示命中总数**（§2 的直接推论：后端没有便宜的来源）。界面只能说
   「还有更早的」：`hasMore = 本页条数 >= PAGE(50)`，这是在没有总数的前提下
   唯一不骗人的判据。
2. **左栏由已加载结果派生**，不再向后端要一次「命中了哪些会话」：按 `convId`
   分组取各组最大 `sentAt` 当作「最近命中时间」倒序。代价是它随滚动增长 ——
   这正是诚实的地方：它回答的是「你翻到的结果里有哪些会话」，而不是一个
   需要 `GROUP BY` 的答案（1.9s，已否决）。
3. **只高亮首处命中**，左右各留约 30 字符上下文；片段取自 `previewText(message)`
   而不是 `content` —— 图片/文件/文件夹消息的 `content` 是 JSON 信封，直接截断
   会把一段裸 JSON 糊到界面上（这个项目已经因此出过事故）。
4. **前端的大小写规则必须与 SQLite `LIKE` 逐字一致**（仅 ASCII 折叠）。
   后端只回原文、高亮的那一处由前端定位，两边规则一分叉就会出现
   「搜到了但一个字都没高亮」。规则单独放在 `frontend/src/utils/search.ts`，
   由两处共用：`mock.ts` 的 LIKE 模拟、`SearchPanel.vue` 的结果高亮定位 ——
   写两份就一定会分叉。

两条并发纪律（Wails 调用不可取消，这两条是唯一的保护）：

- **250ms 防抖**：输入连打收敛成一次查询；
- **请求序号守卫**：每次查询自增序号，响应回来对不上就丢弃。只在成功分支验号
  不够 —— 失败分支同样会把旧错误写到界面上；`loading` 的复位也必须验号，
  否则后发的那次查询会被标成「不在加载中」。

入口三个：左栏搜索框**回车**（带上当前关键词）、`Ctrl/⌘+K`（全局）、本人菜单
（从头找）。关键词跨组件传递走 `ui.searchSeed` —— 弹层是 `Teleport` 到 `body`
的，与左栏没有父子关系。

`mock.ts` 必须实现同一套语义（`seqByMsg` 写入序 = sqlite 的 rowid / mem 的 `seq`）。
它存在的理由不是「让预览能跑」，而是让 `npm run dev`（没有 Go 后端）时
**同一套界面能被真实点击**；语义一旦分叉，预览就会开始骗人。

### 9. 前端：跳转定位 = 「窗口 + 锚点」，锚点由视图消费

`chat.openAt(conv, msgId)`：

1. `SearchService.JumpContext(convID, msgID, 40, 20)` 取回以该条为中心的一页
   （向上多取、向下少取：锚点会被滚到视口中央，上方那 40 条是前情提要）；
2. 用这个窗口**替换** `messagesByConv[convId]`（不是追加 —— 消息可能在几千条之前，
   追加会得到一个空洞）；
3. 置 `anchorMsgId` 这份「意图」，由 `ChatView` 在渲染后消费：滚到
   `block:'center'` + 2s 描边高亮（`prefers-reduced-motion` 下改成静态描边），
   然后由视图调 `clearAnchor()`。

**为什么 store 不自己 `querySelector` + `scrollIntoView`**（原始设计与实现不同之处）：

- **时机**：窗口要等下一次渲染才进 DOM，`openAt` 里同步查必然拿到 `null`，
  表现是「点了搜索结果不滚」——一个只在真机上才看得见的静默失败；
- **职责**：store 不该碰 DOM 与定时器，而滚动容器在 `ChatView` 手里。

代价是多一个字段 + 一次 watcher；换来的是「谁消费谁清理」。视图侧有两个必须显式
处理的时序问题，都会被写成「偶发、难复现」的 bug：

- 会话历史那个「条数变化 → 贴底时自动滚到底」的 watcher 会毁掉刚定位好的位置，
  所以先把 `atBottom` 压成 `false`（并且要压两次：`toBottom` 的赋值会异步派发
  scroll 事件、由 `onScroll` 把它置回 true）；
- 滚动前必须让出一个宏任务 —— `toBottom` 走的是 `nextTick` 的微任务链，
  不让位就会出现「先滚到锚点、再被拉回底部」。

DOM 定位用「扫一遍 `[data-msg-id]` 再比对」，不用 `[data-msg-id="…"]` 属性选择器：
`msg_id` 是外部数据，拼进选择器要先做 CSS 转义（引号、反斜杠都会把语法写坏），
而一屏最多百来条，扫一遍的代价可以忽略。

**边界（空窗口）不是错误**：`JumpContext` 返回空数组表示消息不存在 / 不属于该会话 /
已被清空点隐藏 —— 后者是正常的用户路径（用户在另一台设备或另一个窗口清空过）。
此时回退到最新一页并给**提示条**，不静默失败：静默的表现是「点了结果，聊天区一片空白」，
用户会以为自己点错了。

提示条用独立的 `notice` 字段而不是复用 `error`：`error` 会在下一次 `loadHistory`
开头被清空，而这条提示恰恰是在 `loadHistory` **之后**才成立的，塞进同一个字段必然互相踩。

**关于「跳转后向上翻页的 `before` 游标」**（原 §4.6.5）：当前代码里**没有向上加载更多
历史**这个功能（`History(limit, before)` 后端支持，界面固定取最新 100 条），因此没有
游标可重算。今天的自愈方式是：再次打开该会话时 `open()` 重新取最新 100 条。
等真接上向上翻页时，游标取 `messagesByConv[convId][0].msgId` 即可。

## Consequences

**更好**

- 20 万条消息下常见词首屏 **~0.5ms**；游标页 <1ms；跳转窗口 <1ms。
  两字中文、任意长度子串、ASCII 大小写全部开箱可用。
- 无需维护 FTS 索引（及它的重建/一致性/体积）。
- 搜索的可见性规则与聊天区同一条（清空点），不会出现「界面与搜索不一致」。
- 计划被 `NOT INDEXED` / `INDEXED BY` 钉死，**不依赖统计信息**，
  因此开发机与用户机器上的行为一致。
- 跳转是**一次往返 + 一个窗口**：滚到几千条之前的那条消息，与滚到相邻消息同价
  （<1ms），不需要「先拉全部历史再找」。
- 搜索不占右栏、不影响会话状态；关掉弹层对聊天区零副作用（唯一的例外是
  点过结果 —— 那是用户明确要求「跳过去」）。

**更差**

- 搜索的代价是「扫到够 50 条或扫完全表」：罕见词/零命中在 20 万条下 **175~226ms**，
  50 万条下约 1~1.5s。可接受的原因：它只发生在**没有结果**的那一次。
- **不能显示命中总数**。左栏是「筛选维度」而不是「统计面板」。
- 跨表语义要手抄一遍到内存实现（`seq` / `likeContains` / 行值边界），
  这是为「mem 与 sqlite 互为替身」的测试策略付的成本。
- 计划被硬编码在 SQL 里：若将来删改 `ix_msg_conv_time`，
  `INDEXED BY` 会让查询**直接报错**（而不是默默变慢） —— 这是刻意的，
  由 `TestQueryPlansArePinned` 与窗口用例守住。
- **整窗弹层挡住了聊天区**：不能「边看当前会话边搜」。这是两栏换来的空间，
  代价是搜索期间看不见聊天。可接受：搜索是一次短交互，关掉即回。
- **跳转后聊天区只剩那个窗口**（40+20 条）：滚到窗口边界就没有更多了，
  想看最新要么重新点一次会话，要么等一条新消息把视图带下去。
  这是「不做向上翻页」的直接后果，也是它的边界。
- 前端的关键词折叠规则必须与 SQLite `LIKE` 手工对齐（`utils/search.ts`）。
  这是「高亮在前端做」的成本；漏了它只会表现为「搜到了但不高亮」——
  属于「看起来能用」的那类缺陷，所以两侧规则共用一份实现而不是各写一遍。

## 未采纳的替代

| 替代 | 否决原因 |
|---|---|
| FTS5 + trigram | 中文两字词静默返回 0 命中（最要命的静默失败） |
| FTS5 + unicode61 | 对 CJK 无效（整段一个 token） |
| 自建倒排表 / 分词器 | 新依赖 + 迁移 + 一致性维护，收益在 20 万条内体现不出来 |
| `count(*)` 命中总数 | 20 万条 213ms；左栏不显示计数后它没有消费者 |
| `GROUP BY conv_id` 每会话计数 | 20 万条 **1.9s** |
| `OFFSET` 翻页 | 第 2 页 2ms（游标 1ms），但深页会线性变慢且并发插入会错位 |
| `sent_at` 作搜索游标 | 同毫秒/乱序时间戳会漏消息 |
| 窗口按 rowid 回溯 | 块状布局下 O(全表)（实测 65ms 起） |
| 启动时 `ANALYZE` 一次 | 统计信息会过期，行为随时可变；`NOT INDEXED` 零维护、零不确定性 |
| 客户端拉全部历史后本地过滤 | 百万级消息不可能全量进内存；且会话清空点在后端 |

前端侧：

| 替代 | 否决原因 |
|---|---|
| 搜索面板放右栏（与资料卡并列） | 右栏语义是「当前会话的对方资料 + 传输」；搜索是跨会话的，语义错位，还会挤掉传输进度 |
| 新开一个路由页 | 本应用没有路由层（单窗口三栏），全局功能一律走 `ui.panel` 弹层 |
| 显示命中总数 / 每会话计数 | 后端 213ms / **1.9s**，且直接违反 §2 的「不计数」纪律 |
| 按相关度排序 | `LIKE` 没有词频与位置信息，打分只能自造；时间倒序才是「聊天记录」的心理模型 |
| 高亮全部出现位置 | 一条消息里同词出现十次就要包十个 `mark`，既慢又乱；用户只需知道「这条为什么命中」 |
| 每次按键立即查询（不做防抖） | 一次输入就是十几次 IPC + 查询；250ms 防抖 + 序号守卫足够 |
| 在 `chat.ts` 的 `openAt` 里直接 `querySelector` + 滚动 | 窗口要等下一次渲染才进 DOM（同步查必为空）；且滚动是视图职责 |
| 跳转后自动「加载更多」把窗口补满 | 会把一次定位变成一次全量读，而用户要的是那一条 |
| 高亮做成常驻状态（不自动撤） | 会让「我刚才是从搜索跳过来的」变成一个永久标记，看不出哪个是当前定位 |

## 验证

### 后端（P1，自动化）

- `internal/adapters/store/sqlite/messages_search_test.go`：LIKE 转义、中文两字词、
  ASCII-only 折叠、清空点（含边界那一毫秒）、白名单必填、同毫秒游标分页不重不漏、
  未知游标返回空、搜索按写入序、`Window` 的四种边界 + 锚点不可用 +
  **同毫秒窗口**；以及 `TestQueryPlansArePinned` —— 在【无统计信息】与
  【ANALYZE 后】两种输入下各断言一次四条查询的计划（无 TEMP B-TREE /
  `INTEGER PRIMARY KEY` / 走 `ix_msg_conv_time`）。
- `internal/adapters/store/mem/mem_search_test.go`：同构副本（内存实现单独漂移同样致命）。
  与 sqlite 版差一处：多一条 `TestWindowOrdersBySentAtThenSeq` —— 内存实现有一份
  显式的写入序，窗口排序值得单独钉住；CJK 与 ASCII-only 折叠由下面的对拍覆盖。
- `internal/adapters/store/parity/parity_test.go`：同一套场景（含同毫秒窗口、
  写入序与时间戳不一致的窗口）对拍两个实现，把「对等」变成机械可验证的断言。
- `internal/adapters/wails/search_test.go`：白名单派生（被移除的对端不得出现）、
  已解散的群仍可搜、空输入短路、游标分页与跳转窗口的参数传递。
- 大样本复核：`go test ./internal/adapters/store/sqlite/ -run XXX \
  -bench BenchmarkSearch200k -benchtime 20x`（20 万条，真实代码路径）。
- 红线与格式：`go test ./...`（含 `internal/archtest`）与 `gofmt -l`。

### 前端（P2/P3）

本仓约定**前端不写自动化测试**（`AGENTS.md`），因此这里的「可执行验收物」是
**类型与构建门禁 + 手工清单**，而不是测试文件：

```
make ci        # = build + lint-arch + test + frontend-typecheck + frontend-build
```

验收清单如下。带 ✅ 的是 P2/P3 落地时**在真浏览器里（降级 mock 模式）走查过**的，
其余条目写明「靠什么兜住」—— 不写「已验收」这三个字，是因为没走过的就是没走过：

| # | 验收项 | 状态 |
|---|---|---|
| 1 | `Ctrl/⌘+K` 打开弹层；左栏搜索框回车**带着当前关键词**打开 | ✅ 走过 |
| 2 | 中文两字词有命中；英文大小写不敏感；`%` / `_` 不通配 | ✅ 走过中文两字词；ASCII 折叠与元字符转义由 `parity` 对拍 + mock 侧断言兜住 |
| 3 | 滚动到底 →「加载更早的命中」翻页，两页不重复 | mock 语料造不出 >50 条命中；由 `limit=1` 的两页断言兜住（不重不漏） |
| 4 | 点左栏某会话 → 结果收窄且显示「最近命中时间」；点「全部会话」恢复 | ✅ 收窄走过；「全部会话」恢复是同一条 watcher 的另一个入参 |
| 5 | 点结果 → 弹层关闭、聊天区**停在该条**（不是最新一页）且该气泡有 2s 描边；2s 后描边消失、位置不回弹 | ✅ 走过（小窗使会话历史溢出，滚动位置可判读） |
| 6 | 清空某会话后搜同一个词搜不到；撤销清空后又能搜到且能跳过去 | store 级断言走过（`openAt` 的清空/撤销两条路径）；界面完整往返未单独走 |
| 7 | `Esc` 关弹层；点遮罩也关；关掉后聊天区不受影响 | ✅ `Esc` 走过；遮罩只核对了 `Modal` 的 `@click.self` 实现 |
| 8 | 跳转后再次打开同一会话 → 回到最新 100 条（窗口自愈） | store 级断言走过 |

### 分期与落点

| 阶段 | 落点 | 验收 |
|---|---|---|
| P1 后端 | `ports.MessageRepo.Search/Window`、`sqlite/messages_search.go`、`mem`（`seq`）、`wails.SearchService`、`store/parity` | 上面四条 Go 测试 + 计划守卫 |
| P2 界面 | `stores/ui.ts`（`PanelKind='search'`）、`views/SearchPanel.vue`、`utils/search.ts`、`api/{types,bindings,mock}.ts`、`App.vue`、`ContactPanel.vue` | `make ci` + 清单 1–4、6、7 |
| P3 跳转 | `stores/chat.ts`（`openAt` / `anchorMsgId` / `notice`）、`ChatView.vue`（`data-msg-id` + 锚点消费）、`ChatBubble.vue`（`anchor` 高亮）、`SearchPanel.vue`（改走 `openAt`） | `make ci` + 清单 5、6、8 |
