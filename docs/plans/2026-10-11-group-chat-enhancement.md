# 群聊完善 · 实施计划

**日期**：2026-10-11
**依据**：`docs/plans/2026-10-11-group-chat-enhancement-design.md`
**每个 Task 一次提交（可回滚锚点）**；新协议动作已在本设计内确定，无新增 ADR。

---

## T1 领域与迁移 ✅

- [x] `domain/group/group.go`：`GroupState`（active/dissolved）+ `State` 字段 + `IsDissolved()`；
      `SigningBytes()` 纳入 state（`id|epoch|state|members`）；`Validate()` 校验状态取值。
- [x] `adapters/store/sqlite/migrations.go`：追加迁移 v5（groups.state + group_msg_deliveries）。
- [x] `adapters/store/sqlite/groups.go`：读写 state。
- [x] `adapters/store/sqlite/group_deliveries.go`（新）：实现 `ports.GroupDeliveryRepo`。
- [x] `adapters/store/mem/mem.go`：Groups 读写 state；新增投递实现（与 sqlite 对等）。
- [x] 测试：迁移可重入；state 往返；投递 Ensure/Ack/Pending/Stats。

## T2 端口与应用层 ✅

- [x] `domain/ports/ports.go`：新增 `GroupDeliveryRepo` 与 `DeliveryStat`。
- [x] `domain/protocol/wire.go`：`GroupMeta` 增加 `state`。
- [x] `app/group_app.go`：
      - `Rename` / `RemoveMember` / `Dissolve`（均仅 owner）。
      - `send`：登记投递；`fanout`：只发未送达 ∩ 在线。
      - `HandleGroupAck`：标记送达，全员完成则 `Deliver`。
      - `SweepOutbox`：补投 + 完成出队。
      - `encode/decodeGroupMeta`：带上 state。
- [x] `app/chat_app.go`：`HandleChatAck` 对群消息委派 `GroupAck`（新增注入点，nil 安全）。
- [x] `app/bootstrap/bootstrap.go`：装配投递仓储 + `ChatApp.GroupAck`。
- [x] 测试：改名/移除/解散 epoch 递增；离线成员上线后补投；单聊 ACK 回归。

## T3 服务层与 DTO ✅

- [x] `adapters/wails/dto.go`：`GroupDTO.state`；`MessageDTO.pendingCount/memberCount`。
- [x] `adapters/wails/services.go`：`GroupService.Rename/RemoveMember/Dissolve/SendFolder`；
      `History`/发送返回为群消息填充投递统计。
- [x] `adapters/wails/eventbridge.go`：`group:updated` 推完整摘要（含 state）。
- [x] `TransferApp.SendFolderTo` + `TransferService.SendFolderTo`（群发文件夹共享 batch_id）。

## T4 前端 ✅

- [x] `api/types.ts` + `api/bindings.ts` + `api/mock.ts`：新方法/DTO 字段（绑定已重新生成）。
- [x] `stores/group.ts`：`rename / removeMember / dissolve / byId / isDissolved`。
- [x] `components/GroupInfoPanel.vue`（新）：成员列表 + owner 操作（加/移/改名/解散）。
- [x] `views/ChatView.vue`：标题可点开面板；解散横幅 + 禁用输入；@提及下拉；引用插入。
- [x] `composables/useFileSend.ts`：群会话逐个在线成员发起传输（文件与文件夹）。
- [x] `components/ChatBubble.vue`：群消息「N 人未送达」；右键复制/引用；@提及高亮 + 引用块渲染。
- [x] `events/index.ts`：`group:updated` 刷新 groups；拖放支持群文件。
- [x] `core components`：`Modal.vue` 增加 `#footer` 插槽；`Icon.vue` 增加 `edit`。

## T5 回归 ✅

- [x] `go test -tags wails ./...` 全绿。
- [x] 前端 `npm run typecheck` / `npm run test`(48) / `npm run build` 通过。

---

## 备注

- **退出群**按决策「本次不做」，只提供群主侧移除。
- `SigningBytes` 布局变更会使**升级前的旧群签名失效**（未发布，接受）；两端都升级后新建的群不受影响。
  若开发库里有旧群，重建该群即可。
- 群消息的「未送达」计数只在**全员送达**时清零（后端不逐成员推送 ACK 事件），
  这是刻意取舍：不做逐成员进度事件，避免为一条消息打出 N 条事件。
