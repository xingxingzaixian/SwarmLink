# 身份托管（vault）与无效节点删除 设计

- 日期：2026-10-10
- 状态：已确认（对话逐节评审通过），待实现
- 范围：两个独立功能，可分别实现、分别验收
  - **A. 身份托管（vault）**：让"换数据目录 / 重装应用"不再换身份
  - **B. 无效节点删除**：能删掉列表里的失效身份（尤其是身份重生留下的幽灵条目）

## 1. 背景

`node.key` 存在数据目录下（`internal/adapters/keystore/keystore.go:17-18`），
首次运行随机生成（`keystore.go:44-47`），`node_id` 是公钥的指纹
（`internal/domain/identity/identity.go:60-69`）。

由此产生的两个问题：

1. **换数据目录 = 换身份**：`README.md:233-238` 已把它列为"必须知道的副作用"。
   重装应用、用 `SWARMLINK_DATA_DIR` 指向新目录、便携模式换路径，都会生成新密钥
   ⇒ 新 `node_id` ⇒ 对端看到一个新节点，历史会话/群成员全部错位。
2. **幽灵条目**：旧 `node_id` 会永远留在别人的 `peers` 表里（主键是 `node_id`、
   `Upsert` 只按 `node_id` 合并、全仓没有任何删除路径，见 §5.1），
   而列表里又看不到任何清理入口。

## 2. 目标与非目标

**目标**

- A：同一台机器上，**删/换数据目录、重装应用**后自动恢复同一个 `node.key`
  ⇒ `node_id`、公钥都不变，对端还认识你。
- B：能把列表里的无效用户**彻底删掉**（从列表消失，且不被目录同步带回来）。

**非目标（明确不做）**

- 跨机器迁移身份、多设备并行用同一身份
- 密钥轮换 / 继任声明
- 系统重装或换硬件后仍保持身份（需 TPM 或设备指纹，见 §6.3）
- 真拉黑（拒绝握手）
- 设备指纹派生密钥（见 §6.1）
- vault 多副本、身份导出/导入恢复码（YAGNI，见 §6.4）
- 启动通知（`Node.Notices`）的 UI 展示面（用户明确不需要，见 §4.6）

## 3. 已确认的决策与代价

| # | 决策 | 代价（已接受） |
|---|---|---|
| D1 | vault 位置用**机器级** `%ProgramData%\SwarmLink`（`SWARMLINK_VAULT_DIR` 可覆盖） | 同机多用户共享身份 |
| D2 | Windows 用 **DPAPI 机器作用域**（`CRYPTPROTECT_LOCAL_MACHINE`）包裹；其他平台明文 + 权限 | 仅 Windows 有机器绑定；同机任何用户都能解包 |
| D3 | **数据副本优先**，vault 只是恢复副本 | 两份不一致时以数据目录为准（并覆盖 vault） |
| D4 | vault 损坏 → **生成新身份 + 覆盖 vault + error 日志**（不阻塞启动） | 告警只落在 `logs/swarmlink.log`，用户可能没察觉 |
| D5 | 便携模式（`portable.mode`）**跳过** vault | 便携=自包含，不做恢复 |
| D6 | 删除 = 把该行 `state` 置为已有的 `'blocked'`（零迁移、零新表）；**不拒绝握手** | 删除只在本机生效，对方列表不受影响 |
| D7 | 被删的 `node_id` 若再次发来**有效签名通告**，**自动恢复** | 活跃用户删不掉（会自动爬回来） |
| D8 | 不做"已移除"列表 UI | 误删只能靠自动恢复 |

## 4. 设计 A：身份托管（vault）

### 4.1 位置解析（新增，独立于 `config.Dir()`）

新增 `config.EnvVaultDir = "SWARMLINK_VAULT_DIR"` 与 `config.VaultDir() (string, error)`，
纯路径解析、不做 I/O，优先级：

1. `SWARMLINK_VAULT_DIR`（部署与测试隔离；与 `EnvDataDir` 对称，见 `config.go:23-27`）
2. Windows：`%ProgramData%\SwarmLink`（`os.Getenv("ProgramData")`，缺失退 `%SystemDrive%\ProgramData`）
3. Linux：`/var/lib/swarmlink`；macOS：`/Library/Application Support/SwarmLink`
4. 便携模式（`config.PortableMarker` 存在，`config.go:29-31`）→ 返回 `""`（禁用）
5. 兜底守卫：`vaultDir == dataDir` → 返回 `""` 并告警
   （macOS 上 `os.UserConfigDir()` 就是 `~/Library/Application Support`，降级路径很容易撞上数据目录；
   一旦相同，"删数据目录"就等于"删身份"，必须拦住）

`""` 表示"本平台/本次运行禁用 vault"，后续全部走现状分支。

### 4.2 存储格式

- Windows：`node.key.dpapi` = `CryptProtectData(pemBytes, entropy="swarmlink-vault-v1", CRYPTPROTECT_LOCAL_MACHINE)`
- 其他平台：明文 PEM（与数据副本同格式），目录 `0700` / 文件 `0600`（尽力而为）
- 平台文件：`dpapi_windows.go`（`//go:build windows`）+ `dpapi_other.go`（`//go:build !windows`），
  风格对齐现有 `fatal_windows.go` / `fatal_other.go`
- Windows 走 `golang.org/x/sys/windows` → `crypt32`。
  `golang.org/x/sys` 已在依赖树（`go.mod:24`，当前是 indirect），
  `go mod tidy` 会提升为 direct，**不新增模块**
- `entropy` 用固定串：它只是"同一台机器上不同用途的 blob 不互通"，不是秘密

### 4.3 启动决策顺序

唯一入口保持 `keystore.LoadOrCreate`，签名改为 `LoadOrCreate(dataDir, vaultDir string)`：

| 数据副本 | vault | 行为 |
|---|---|---|
| 解析成功 | 任意 | 用它；若 vault 内容不同 → **用数据副本覆盖 vault**（权威副本），无告警 |
| 存在但损坏 | 完好 | 从 vault 恢复 → 写回数据副本 → 返回告警 |
| 存在但损坏 | 缺失/也坏 | **返回错误**（保持现状，绝不静默换身份） |
| 缺失 | 完好 | 从 vault 恢复 → 回填数据副本 → 日志 `identity restored from vault` |
| 缺失 | 损坏/解包失败 | **生成新身份** → 覆盖 vault → 返回告警 + error 日志 |
| 缺失 | 缺失 | 生成新身份 → 同时写两份 |
| 任意 | 不可用（`""`/不可写/DPAPI 失败） | 完全降级为现状行为，启动时告警一次，不重试 |

告警通过**返回值**回传（`restored bool` + `notice string`），
keystore 不依赖日志、UI 或 `bootstrap`。

**为什么 vault 损坏不返回错误**：`README.md:240-241` 明确把"启动失败不再静默 / 双击没反应"
当作要避免的现象；为一个坏文件让应用起不来更糟。代价是 D4。

### 4.4 原子写与并发

- vault 写入必须原子：写 `<file>.tmp` → `fsync` → `os.Rename`。
  现有 `keystore.go:55` 是直接 `os.WriteFile`，vault 这层必须加，
  否则断电留下半截密钥 → 下次启动被判"损坏" → 触发换身份
- 同机两个实例（不同数据目录、同一 vault）会并发读写：
  对 `<vaultDir>/identity/.lock` 取独占锁（Windows `LockFileEx` / Posix `flock`）；
  拿不到锁 → 只用数据副本、不碰 vault，**不阻塞启动**

### 4.5 组件划分

- `config.VaultDir()`：位置解析 + 便携/相等守卫
- `bootstrap`：解析 `vaultDir` 后显式传入 keystore
  （不放在 keystore 内部，避免 `adapters/keystore → adapters/config` 的横向依赖，
  且与 `bootstrap.go:92-97` 解析数据目录的既有做法一致）
- `keystore`：`keystore.go`（编排）+ `vault.go`（读/写/原子替换/锁）+ `dpapi_*.go`（平台封装）
- `test/harness/harness.go:260` 的 `LoadOrCreateIdentity` 必须同步改签名

### 4.6 安全语义、代价与告警边界

- **机器级 + DPAPI 机器作用域 ⇒ 同机任何用户都能解包**：
  "同机其他用户可继承你的身份"是 D1+D2 的必然结果，需写进 README 与 ADR
- DACL 仍尽力收紧到 `当前用户 + Administrators + SYSTEM`，
  用途是防非本机访问、防被别的进程覆盖；**ACL 设置失败不影响启动**
- DPAPI 在这里真正挡住的是"blob 被拷到别的机器"（ACL 挡不住同机其他用户）
- 告警只在 `logs/swarmlink.log`（D4 + §2 非目标）：
  `bootstrap.Node.Notices` 目前**只被写入、没有任何消费方**（全仓 grep 仅 `bootstrap.go` 自己在 append），
  因此本设计不依赖它；将来补 UI 展示面时，vault 告警可一并接入

## 5. 设计 B：无效节点删除

### 5.1 为什么不能只删行（三条复活路径）

| # | 路径 | 代码位置 | 现状 |
|---|---|---|---|
| 1 | 收到广播/种子通告 | `app/peer_app.go:59-135` | 无条件 `Upsert` |
| 2 | 连接建立事件 | `app/peer_app.go:143-167` | 无条件 `state=online` + `Upsert` |
| 3 | 目录交换（别人送回来的） | `app/discovery_app.go:183-219` | 对每条 `Upsert` 成 `discovered` |

第 3 条是关键：幽灵此刻还在别人与种子的目录里，且 `entries()` 只过滤 `blocked`
（`discovery_app.go:238`）、不过滤 `offline`，所以**全量目录同步（ADR-010）会持续把它送回来**。

同时确认：`ports.PeerDirectory` 没有任何删除方法（`domain/ports/ports.go:65-73`），
`peers` 表也没有 `DELETE` 语句——当前实现下这些行永远删不掉。

结论：**删除必须落成一个持久标记**（否则第 3 条路径会把条目送回来），
而 `peers.state` 里已经有现成的 `'blocked'`：CHECK 允许它（`migrations.go:96-97`），
`DiscoveryApp.entries()` 也已经过滤它（`discovery_app.go:238`）。
**因此既不需要新表，也不需要新列。**

### 5.2 数据模型：复用 `state='blocked'`，零迁移

**删除 = 把该行的 `state` 置为 `'blocked'`。**

- 行保留（`display_name` / `pub_key` 一并保留，便于诊断与将来做"已移除"列表），
  但不再出现在任何用户可见的列表里（靠 §5.3 ② 的读取过滤）
- `blocked` 在本设计里的语义定为"**本地已移除（抑制目录条目）**"。
  将来做真拉黑（拒绝握手）时在此基础上加"不解抑制 + 拒绝握手"，
  必要时补一个 `blocked_reason` 字段区分，而不是现在就把语义拆成两个标记（见 §6.5）
- **不新增 `is_delete` 列**：它会与 `state` 形成两处真相（`is_delete=1` 但 `state=online` 该算什么？），
  而且照样要一次迁移；复用 `state` 是零迁移 + 单一真相
- **不新增 `removed_peers` 表**（本文档初稿的方案）：它唯一的收益是"读取路径不用过滤"，
  代价是多一张表 + 一个端口方法 + 四处实现（`sqlite.Peers`、`mem.Peers`、
  `wails/services_test.go:146` 的 `emptyDir`、`:176` 的 `capDir`）。
  把过滤下沉到仓储 `List()` 之后（§5.3 ②），复用 `state` 的读取路径同样"天然正确"

### 5.3 行为规则

**① 抑制：下沉为写入侧不变量（0 个显式抑制点）**

把 `blocked` 做成**粘性状态**：已存在的行若为 `blocked`，任何写入都不得把它改成别的状态
（sqlite 侧在 `Upsert` 的 `ON CONFLICT DO UPDATE SET` 里加一个 `CASE`，`mem.Peers.Upsert` 加一行判断）。

放在仓储的唯一写入点，是为了避免"每新增一条写入路径都要记得过滤"的漏点：
今天有 3 条路径会写 peers（`OnAnnouncement`、`TopicPeerOnline`、`HandlePeerListResp`），
以后只会更多。

**② 读取：`List()` 默认排除 `blocked`**

`PeerFilter` 增加 `IncludeBlocked bool`（默认 `false`；诊断与测试可显式打开），
在 `sqlite.Peers.List`（`sqlite/peers.go:69-110`）与 `mem.Peers.List`（`mem/mem.go:504-517`）两处实现。
这样 `PeerService.List()`（`wails/services.go:848-863`）、
会话列表（由 peers 派生，`services.go:578-590`）、`DiscoveryApp.entries()` 全部自动正确，
将来新增的遍历也不会漏。

**③ 恢复：两个显式恢复点（D7）**

- `OnAnnouncement`：UDP 层已验签 + `node_id == Fingerprint(pub)` 自洽校验
  （`udp/broadcast.go:262-272`、`udp/seed.go:80-88`）⇒ 有效签名通告 ⇒ 恢复
- `TopicPeerOnline`：TCP 三段握手已认证（`tcp/handshake.go:140-143`、`:222-224`）⇒ 恢复
- `HandlePeerListResp`：**不恢复**（第三方转述、无签名）——靠 ① 的粘性自动被挡

恢复动作 = `SetSuppressed(id, false)` + **走现有的状态计算，不直接写 `online`**：

| 恢复触发 | 恢复后的 `state` | 依据 |
|---|---|---|
| `OnAnnouncement` | `discovered`（若会话仍在则 `online`） | `peer_app.go:87-90` |
| `TopicPeerOnline` | `online` | `peer_app.go:156` |

这条区分不能省：`PeerDTO.Online` 只认 `StateOnline`（`wails/dto.go:95`），
把一条 announce 直接写成 `online` 等于违反 ADR-009（"在线状态与连接可用解耦"），
UI 上会出现"显示在线但发不出消息"。

> 实现注释必须钉住这条假设：`OnAnnouncement` 的可信度**来自调用方已验签**。
> 将来若有未验签路径调用它，D7 就会变成"任何人都能让被删的节点复活"。

**④ 端口与实现（本功能唯一新增的接口方法）**

```
// SetSuppressed 设置/清除"本地已移除"标记（抑制目录条目、不进列表）。
SetSuppressed(id identity.NodeID, suppressed bool, at time.Time) error
```

实现四处：`sqlite.Peers`、`mem.Peers`，以及 `wails/services_test.go` 的 `emptyDir`、`capDir` 两个桩
（`MarkOffline` 是现成的同类方法，照抄它的形状即可）。

选择显式方法而不是"`Get` 后改字段再 `Upsert` 回去"，是为了让"删除"在领域端口上是一个明确动作，
而不是一次读改写的副作用。

**⑤ UI 动作 `PeerService.Remove(nodeID)`**

`SetSuppressed(id, true)` → 发 `peer:removed` 事件让前端即时刷新
（事件桥已有 `EventPeerUpdated` 的先例，见 `wails/eventbridge.go:100-107`）。

**不做的事**

- 不拒绝握手（`tcp/manager.go:201-210` 不加钩子）
- 不发任何网络通告（P2P 无权威；别人的列表不受影响，UI 文案需写明）
- 不删历史消息（会话列表由 peers 派生，该行不进列表后会话自然消失；恢复后历史回来）

### 5.4 UI

- `frontend/src/components/ContactCard.vue` / `ContactPanel.vue`：加"删除"入口 + 二次确认
  （文案说明：只在本机生效；该节点若再次上线会自动恢复）
- 服务方法：`PeerService.Remove(nodeID string) error`（`wails/services.go`，`Deps` 已有 `PeerDir`）
- 改完服务导出方法后执行 `make bindings` 重新生成提交入库的 `frontend/bindings/`
  （见 `docs/adr/README.md:54`）

## 6. 未采纳的替代

### 6.1 用设备信息派生私钥 / 用机器标识做 `node_id`

**否决**。实测与代码依据：

- 机器标识不是秘密：本机 `HKLM\SOFTWARE\Microsoft\Cryptography` 的默认 ACL 里
  `BUILTIN\Users` 与 `ALL APPLICATION PACKAGES` 都有 `ReadKey`；
  `Win32_ComputerSystemProduct` 也无需提权即可查询。
  派生后攻击门槛从"读 0600 密钥文件"降到"读一个全局可读的值" ⇒ 私钥不再保密，
  而 `identity.Verify` 是唯一信任根
- `node_id` 必须由公钥派生：`node_id == Fingerprint(pub)` 有 **5 处硬校验**
  （`udp/broadcast.go:270`、`udp/seed.go:86`、`tcp/handshake.go:141`、`:222`，
  外加 `announce_identity_test.go:135` 专门钉住），它是防冒充的唯一防线；
  改 `node_id` 生成方式就要删掉这 5 处 ⇒ 任何人都能声称别人的 `node_id`
- 稳定性是相对的：`MachineGuid` 在 sysprep/系统克隆时重生，`SMBIOS UUID` 在编辑 VM 定义/重建虚拟机时变；
  Linux 的 `/sys/class/dmi/id/product_uuid` 多为 `0400 root:root`，普通用户读不到 ⇒ 需要回退链
  ⇒ 字段集合一变就身份漂移（正是本设计要修的 bug）
- 克隆/模板化的虚拟化环境（本仓库的部署场景就是 VDI）会让多台机器共享同一标识
  ⇒ **两个不同的人拿到同一个 `node_id`**，比幽灵条目严重得多
- 只保留其**诊断用途**（例如将来在列表里显示机器短码、或检测"身份文件来自别的机器"），
  不参与任何密钥、签名或 `node_id` 计算

### 6.2 用 MD5 生成 `node_id`

**否决**。MD5 已被实用化碰撞攻破，而 `node_id` 是 `peers` 主键、是目录与协议的索引，
碰撞=身份混淆。现用 `SHA-256(域分隔串 ‖ pubkey)[:8]`，且域分隔有断言钉住
（`identity_test.go:32-43`）。

### 6.3 系统重装后仍保持身份

**否决（超出本次需求）**。系统盘与注册表被清后，机器级文件与机器级 DPAPI 都失效，
只剩 TPM 或设备指纹两条路，二者都引入 §6.1 的问题或平台特性。用户已确认不需要。

### 6.4 vault 多副本 / 身份导出导入

**暂缓（YAGNI）**。二者是"所有副本都被删"的兜底，而本次需求（删/换数据目录、重装应用）
一份 vault 足够。真出问题再补，届时"导出/导入恢复码"是唯一能覆盖换硬件的方案。

### 6.5 真拉黑（拒绝握手）

**本次不做**。用户明确选择"直接删除，主要针对无效用户"，所以 `blocked` 本次只承载
"本地已移除（抑制目录条目 + 不进列表 + 收到有效通告自动恢复）"这一层语义。

将来的真拉黑需要它再加一层：**不解抑制 + 拒绝握手**（在 `tcp/manager.go:201-210` 注入预检）。
届时建议给 `peers` 补一个 `blocked_reason` 之类的字段来区分两种程度，
而不是引入第二个布尔标记（避免与 `state` 形成两处真相）。
它对应 `docs/plans/2026-10-09-后续开发计划.md:108` 里 S2 的"密钥轮换 + 拉黑"项。

## 7. 测试计划

### 7.1 vault（`internal/adapters/keystore`，全部用 `t.TempDir()` 传 `vaultDir`）

1. 两份都缺 → 生成，两份内容一致、`node_id` 相同
2. 只有数据副本 → 用它，且 vault 被同步为同一份
3. **只有 vault → 恢复同一 `node_id`，且数据副本被重建（核心用例）**
4. 两份不同 → 以数据副本为准覆盖 vault，无告警
5. 数据副本损坏 + vault 完好 → 从 vault 恢复 + 告警
6. 数据副本损坏 + vault 缺失 → **返回错误**（保持 `keystore_test.go:72-80` 现状）
7. vault 损坏 → 生成新身份 + 覆盖 vault + 告警，不 panic、不返回 error
8. `vaultDir == ""`、目录不可写 → 降级为现状行为，不返回 error
9. 原子写：中断只留 `.tmp`，不污染正式文件
10. 并发：两 goroutine（不同 dataDir、同一 vault）同时加载 → `node_id` 一致、vault 无损坏
11. Windows 专用：DPAPI 往返得到同一私钥；blob 被篡改 → 报错而非 panic

### 7.2 删除

1. 删除后 `List()` 不含该节点（默认排除 `blocked`）；`Conversations()` 也不含
2. **粘性**：删除后再从目录交换收到该节点条目 → 仍是 `blocked`、仍不显示（不复活）
3. 再次收到该节点的**有效签名通告** → 自动恢复（列表里回来）
4. `TopicPeerOnline`（握手成功）→ 自动恢复
5. `IncludeBlocked=true` 时能看到被移除的行（诊断用）；`entries()` 不含它（不向外传播）
6. 迁移：**不新增迁移** —— 断言 `len(migrations)` 不变（`sqlite_test.go:58`），
   且写入 `state='blocked'` 仍满足 CHECK 约束（`migrations.go:96-97`）
7. `sqlite.Peers` 与 `mem.Peers` 两个实现的 `List` 过滤行为一致（同一组用例跑两遍）
8. 端到端（`test/harness`）：A 删掉 B 的旧身份 → B 以旧身份重新广播 → A 列表恢复为一条

### 7.3 回归

- `go test ./...`
- `make lint-arch`（R1/R3 红线不受影响：改动集中在 `adapters`、`bootstrap`、`app`）
- `test/scale`（目录条数断言：无人被删时应与现状一致）

## 8. 文档与兼容性

- `README.md:233-238`：第 1 条副作用改写（"换数据目录 = 换节点身份"不再成立），
  并补"同机其他用户可继承身份"与"删除只在本机生效"
- `设计书.md:158-170`（§4.1）：补 vault 一节（位置、格式、恢复优先级）
- 新增 `docs/adr/ADR-012-identity-vault.md`，并在 `docs/adr/README.md:18` 索引表补一行
  （验收测试指向 §7.1 第 3、5 条）
- 升级兼容：数据副本仍是第一优先，vault 只在缺失/损坏时参与；
  首次启动只是把现有 `node.key` 同步进 vault，**已有用户 `node_id` 不变，无迁移动作**
- 降级兼容：旧版本只读数据副本，会忽略 vault 文件，不报错
- 删除功能**不新增迁移**（复用既有 `state='blocked'`）。
  兼容性注意：旧版本读到 `state='blocked'` 会把它当普通条目显示出来（不崩、不丢数据），
  因此建议与 vault 一起升级，或升级前不使用删除功能

## 9. 已知代价与风险（实现了也依然存在）

1. 同机多用户共享身份（D1+D2）
2. vault 损坏时的告警只有日志（D4，非目标里已排除 UI 展示面）
3. 活跃用户删除后会自动恢复（D7）
4. 删除某个节点后，指向它的**等待中的传输任务**仍会在后台重试
   （`app/transfer_waiting.go`，退避到上限 5 分钟）；对端已死则一直失败，
   用户可在传输列表里取消。本次不做联动清理
5. 删除不发网络通告：别人列表里的你/它依然存在，这是 P2P 无权威的固有结果
