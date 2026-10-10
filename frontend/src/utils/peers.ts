/**
 * 节点列表的纯逻辑工具。
 *
 * 抽成纯函数是为了能测：本仓前端没有 jsdom / 组件测试能力（`npm run test` 是
 * node --test，只跑纯逻辑），把「移除后列表长什么样」这条规则留在 store 里就等于没测。
 */

/**
 * 返回去掉指定节点的列表副本。
 *
 * 用严格相等比较而不是前缀匹配：node_id 是 16 位 hex，前 8 位相同很常见，
 * 前缀匹配会一次误删多个节点。
 */
export function withoutPeer<T extends { nodeId: string }>(list: T[], nodeId: string): T[] {
  return list.filter((p) => p.nodeId !== nodeId)
}
