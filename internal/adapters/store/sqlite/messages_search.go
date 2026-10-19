package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// 本文件实现 ports.MessageRepo 的【搜索】与【跳转定位】。
//
// 性能纪律（实测数据见 ADR-013，勿改）：
//
//	搜索 = LIKE 子串 + `ORDER BY id DESC LIMIT n` 的尾优先倒扫。
//	20 万条消息 / 20 个会话下首屏 0.55ms，查询计划里【没有 TEMP B-TREE】——
//	SQLite 从 rowid 尾部倒着扫，凑够 n 条就停。
//	一旦出现 count(*) 或 GROUP BY conv_id，同样的数据量下分别是 213ms 与 1.9s。
//
// 因此搜索只做「倒扫 + LIMIT」，不排序、不聚合、不计数；
// 窗口（Window）只用 `(sent_at, id)` 的行值范围取两侧。
// `TestQueryPlansArePinned` 把这几条纪律钉在查询计划上（且在无统计信息的有
// 与无两种状态下各断言一次）。

// messageColumnsM 是带表别名的列清单。
//
// 搜索与定位都要 JOIN conv_clears（清空点在另一张表），因此不能复用
// messages.go 里那份不带别名的 messageColumns —— 两份列序必须一致，
// 因为它们共用 scanMessage。
const messageColumnsM = `m.msg_id, m.conv_id, m.sender_id, m.direction, m.content,
                        m.msg_type, m.file_id, m.sent_at, m.recv_at, m.state`

// 关于搜索语句里的 `NOT INDEXED`（窗口的两侧则用 `INDEXED BY`，见下方窗口常量）：
// 这不是可有可无的优化提示，而是【正确性级别的必需项】。
//
// 实测（20 万条消息 / 20 个会话，见 ADR-013）：
//
//	有统计信息（ANALYZE 过）→ SCAN messages                      → 0.3ms
//	无统计信息（本应用的常态）→ SEARCH USING ix_msg_conv_time
//	                              + USE TEMP B-TREE FOR ORDER BY  → 543ms
//
// 后者是规划器在【没有统计信息】时对 `conv_id IN (20 个值)` 的乐观估计：
// 它以为按 conv_id 走索引很划算，于是每个会话都扫一遍索引，再为
// `ORDER BY id DESC` 建临时表排序 —— 而 LIMIT 在排序之后才生效，
// 也就是说它必须把所有命中都读出来。写测试时如果先用 ANALYZE 建库，
// 就永远看不到这个计划（第一版就踩了这个坑）。
//
// 本应用从不 ANALYZE（全仓只有测试里出现过这个词），因此生产上跑的是
// 【无统计信息】那条路径。显式 `NOT INDEXED` 让规划器只能用 rowid 倒扫，
// 两种状态下都是同一个快计划，也不再依赖任何统计信息是否新鲜。
//
// 注意：它只作用于本 FROM 子句（`m`），不会影响游标子查询里那次
// `ux_msg_id` 点查；`rowid<?` / `rowid>?` 用的是表本身而不是索引，同样不受影响。

// visibleClause 是「会话清空点」的可见性判据。
//
// 必须与前端 stores/chat.ts 的 activeMessages 逐字对齐（sent_at 严格大于
// cleared_at 才可见）：两边差一个等号，用户就会看到「界面上没有这条消息，
// 但搜索能搜出来」—— 那是隐私回退，不是显示瑕疵。
//
// 为什么过滤放在 SQL 里而不是取回后再过滤：后过滤会让每页条数不定，
// 用户会翻到一串空白页（「到底还有没有」也无法回答）。
const visibleClause = `(c.cleared_at IS NULL OR m.sent_at > c.cleared_at)`

// Search 实现 ports.MessageRepo.Search（语义见端口注释）。
func (m *Messages) Search(q ports.MessageSearchQuery) ([]message.Message, error) {
	sqlText, args, ok := searchSQL(q)
	if !ok {
		return []message.Message{}, nil
	}
	return m.queryMessages(sqlText, args...)
}

// searchSQL 组装搜索语句；ok=false 表示本次查询不必访问数据库
// （空关键词 / 空白名单）。
//
// 抽成独立函数的理由不是复用，而是【可断言】：查询计划是本方案的性能地基，
// 测试必须拿同一份 SQL 去 EXPLAIN（见 TestSearchUsesTailFirstPlan）——
// 在测试里另抄一份 SQL 就等于测试一份谁也不执行的字符串。
func searchSQL(q ports.MessageSearchQuery) (sqlText string, args []any, ok bool) {
	if q.Text == "" || len(q.ConvIDs) == 0 {
		return "", nil, false
	}
	limit := q.Limit
	if limit <= 0 {
		limit = ports.DefaultSearchLimit
	}

	var sb strings.Builder
	sb.WriteString(`SELECT ` + messageColumnsM + ` FROM messages m NOT INDEXED
LEFT JOIN conv_clears c ON c.conv_id = m.conv_id
WHERE m.content LIKE ? ESCAPE '\'
  AND ` + visibleClause)

	args = make([]any, 0, len(q.ConvIDs)+3)
	args = append(args, likePattern(q.Text))

	if q.BeforeMsgID != "" {
		// 游标换算：一次 ux_msg_id 覆盖索引的点查，把「上一页最后一条」定位到
		// rowid，于是下面的 ORDER BY id DESC 仍然走 INTEGER PRIMARY KEY 的范围
		// 扫描（实测计划：SEARCH m USING INTEGER PRIMARY KEY (rowid<?)）。
		//
		// 用子查询而不是「先查一次 id 再拼进 SQL」，是为了让游标只在一条语句里
		// 生效 —— 两次往返之间插进一条新消息会让分页边界漂移。
		sb.WriteString("\n  AND m.id < (SELECT id FROM messages WHERE msg_id = ?)")
		args = append(args, q.BeforeMsgID)
	}

	sb.WriteString("\n  AND m.conv_id IN (")
	sb.WriteString(placeholders(len(q.ConvIDs)))
	sb.WriteString(")\nORDER BY m.id DESC LIMIT ?")
	for _, id := range q.ConvIDs {
		args = append(args, id)
	}
	args = append(args, limit)
	return sb.String(), args, true
}

// Window 实现 ports.MessageRepo.Window（语义见端口注释）。
func (m *Messages) Window(convID, msgID string, before, after int) ([]message.Message, error) {
	if convID == "" || msgID == "" {
		return []message.Message{}, nil
	}
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}

	// 锚点定位：一次点查同时确认三件事 —— 存在、属于 convID、未被清空点隐藏，
	// 并取回窗口边界 `(sent_at, id)`。
	//
	// 为什么必须先判锚点本身：两侧的范围扫描同样带清空点判据，若不先判，
	// 「锚点已被清空」会静默退化成「只返回它更旧的那些消息」——界面上表现为
	// 跳到了错误的位置，而不是回退到最新（那才是调用方期望的兜底）。
	var (
		anchorSentAt int64
		anchorID     int64
		anchorConv   string
	)
	err := m.db.QueryRow(`SELECT m.sent_at, m.id, m.conv_id FROM messages m
LEFT JOIN conv_clears c ON c.conv_id = m.conv_id
WHERE m.msg_id = ?
  AND `+visibleClause, msgID).Scan(&anchorSentAt, &anchorID, &anchorConv)
	if errors.Is(err, sql.ErrNoRows) {
		return []message.Message{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: locate message %s: %w", msgID, err)
	}
	if anchorConv != convID {
		// 调用方给的会话与锚点实际所属会话不符：宁可返回空（前端回退到最新），
		// 也不要把另一个会话的消息放进当前会话的窗口。
		return []message.Message{}, nil
	}

	out := make([]message.Message, 0, before+after+1)

	if after > 0 {
		// 新侧必须用 ASC + LIMIT：DESC + LIMIT 取到的是「整段尾部最新的 n 条」，
		// 而不是「紧邻锚点之后的 n 条」—— 后者才是窗口语义。
		newer, err := m.queryMessages(windowNewerSQL, convID, anchorSentAt, anchorID, after)
		if err != nil {
			return nil, err
		}
		// 翻成新→旧接到前面（与 Latest 的方向一致，前端 loadHistory 就按这个方向 reverse）
		for i := len(newer) - 1; i >= 0; i-- {
			out = append(out, newer[i])
		}
	}

	// 旧侧：DESC + LIMIT 取「紧邻锚点之前的 before 条」加锚点本身。
	older, err := m.queryMessages(windowOlderSQL, convID, anchorSentAt, anchorID, before+1)
	if err != nil {
		return nil, err
	}
	return append(out, older...), nil
}

// 窗口两侧的语句（提到包级常量是为了让测试能 EXPLAIN 到【与代码逐字相同】的 SQL，
// 见 TestWindowUsesIndexRangePlan）。
//
// 边界是**行值** `(sent_at, id)`，不是单独的 id，也不是单独的 sent_at：
//
//   - 排序键是 `(sent_at, id)`（与 Latest、domain.SortInPlace、前端渲染完全一致），
//     用它做边界才能让锚点唯一。只按 sent_at 分界会让同一毫秒内的其它消息与锚点
//     互踩 —— 那种情况下「锚点已被挤出窗口」不是显示瑕疵，而是跳转失败。
//   - 行值比较能直接吃 `ix_msg_conv_time(conv_id, sent_at DESC, id DESC)` 的索引
//     范围：旧侧是索引正序的前 before+1 条，新侧是索引逆序的前 after 条。两侧的
//     代价都只与窗口大小有关，**与该会话有多少消息、全表有多少消息无关**。
//
// 为什么这里不用 rowid 当边界（曾经这么写过）：实测在「某个会话的消息在物理上
// 连成一块」的数据下（一个重度用户连着聊了两小时就会出现），按 rowid 回溯必须
// 扫过前一个会话的整块消息才能凑够 before 条，20 万条时实测 65ms；而按
// (sent_at,id) 走索引恒为 0ms 量级。细节见 ADR-013。
const windowNewerSQL = `SELECT ` + messageColumnsM + ` FROM messages m INDEXED BY ix_msg_conv_time
LEFT JOIN conv_clears c ON c.conv_id = m.conv_id
WHERE m.conv_id = ?
  AND (m.sent_at, m.id) > (?, ?)
  AND ` + visibleClause + `
ORDER BY m.sent_at ASC, m.id ASC LIMIT ?`

const windowOlderSQL = `SELECT ` + messageColumnsM + ` FROM messages m INDEXED BY ix_msg_conv_time
LEFT JOIN conv_clears c ON c.conv_id = m.conv_id
WHERE m.conv_id = ?
  AND (m.sent_at, m.id) <= (?, ?)
  AND ` + visibleClause + `
ORDER BY m.sent_at DESC, m.id DESC LIMIT ?`

// likePattern 把用户输入变成 LIKE 模式串：转义元字符后两侧补 %。
//
// 转义字符是反斜杆，SQL 侧以 `ESCAPE '\'` 声明（写在原样字符串里，编译期
// 不会把 `\` 当成转义序列）。
//
// 漏掉这一步的后果是灾难性的：搜一个 `%` 会命中全库，搜 `_` 会命中任意单字，
// 而这两种输入在中文用户里都很常见（全文搜索的直觉用法）。
func likePattern(text string) string {
	var sb strings.Builder
	sb.Grow(len(text) + 2)
	sb.WriteByte('%')
	for i := 0; i < len(text); i++ {
		// 逐字节处理即可：三个元字符都是 ASCII，UTF-8 的续字节（0x80~0xBF）
		// 不可能是它们，因此中文不会被误伤。
		switch text[i] {
		case '\\', '%', '_':
			sb.WriteByte('\\')
		}
		sb.WriteByte(text[i])
	}
	sb.WriteByte('%')
	return sb.String()
}
