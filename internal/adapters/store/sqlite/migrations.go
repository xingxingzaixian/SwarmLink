package sqlite

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// migration 是一次版本化迁移。
type migration struct {
	version int
	name    string
	sql     string
}

// migrations 是全部迁移（架构书第 5 章）。
//
// 规则：已发布的迁移【永不修改】，只追加新的版本。
// checksum 会检测到对历史迁移的意外改动。
var migrations = []migration{
	{
		version: 1,
		name:    "init",
		sql: `
CREATE TABLE IF NOT EXISTS conversations (
    conv_id      TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK(kind IN ('direct','group')),
    title        TEXT NOT NULL,
    peer_addr    TEXT,
    last_msg_id  TEXT,
    last_time    INTEGER,
    unread       INTEGER DEFAULT 0,
    is_online    INTEGER DEFAULT 0,
    pinned       INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    msg_id     TEXT NOT NULL,
    conv_id    TEXT NOT NULL,
    sender_id  TEXT NOT NULL,
    direction  TEXT NOT NULL CHECK(direction IN ('in','out')),
    content    TEXT NOT NULL,
    msg_type   TEXT NOT NULL DEFAULT 'text',
    file_id    TEXT,
    sent_at    INTEGER NOT NULL,
    recv_at    INTEGER,
    state      TEXT NOT NULL DEFAULT 'pending'
               CHECK(state IN ('pending','sent','delivered','failed')),
    FOREIGN KEY (conv_id) REFERENCES conversations(conv_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_msg_id        ON messages(msg_id);
CREATE INDEX        IF NOT EXISTS ix_msg_conv_time ON messages(conv_id, sent_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS outbox (
    msg_id      TEXT PRIMARY KEY,
    conv_id     TEXT NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    next_try_at INTEGER NOT NULL,
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_outbox_next ON outbox(next_try_at);

CREATE TABLE IF NOT EXISTS groups (
    group_id   TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    owner_id   TEXT NOT NULL,
    epoch      INTEGER NOT NULL DEFAULT 0,
    state_sig  TEXT,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS group_members (
    group_id     TEXT NOT NULL,
    node_id      TEXT NOT NULL,
    display_name TEXT,
    role         TEXT NOT NULL DEFAULT 'member',
    state        TEXT NOT NULL DEFAULT 'active',
    joined_at    INTEGER NOT NULL,
    PRIMARY KEY (group_id, node_id)
);

CREATE TABLE IF NOT EXISTS peers (
    node_id      TEXT PRIMARY KEY,
    display_name TEXT,
    pub_key      TEXT,
    last_addr    TEXT,
    caps         INTEGER DEFAULT 0,
    proto_ver    INTEGER DEFAULT 1,
    first_seen   INTEGER,
    last_seen    INTEGER,
    subnet       TEXT,
    state        TEXT DEFAULT 'unknown'
                 CHECK(state IN ('unknown','discovered','online','offline','blocked')),
    source       TEXT DEFAULT 'broadcast'
);
CREATE INDEX IF NOT EXISTS ix_peers_seen ON peers(last_seen DESC);

CREATE TABLE IF NOT EXISTS transfer_jobs (
    job_id        TEXT PRIMARY KEY,
    peer_id       TEXT NOT NULL,
    file_name     TEXT NOT NULL,
    file_size     INTEGER NOT NULL,
    file_hash     TEXT NOT NULL,
    chunk_size    INTEGER NOT NULL DEFAULT 524288,
    total_chunks  INTEGER NOT NULL DEFAULT 0,
    direction     TEXT NOT NULL CHECK(direction IN ('send','recv')),
    local_path    TEXT NOT NULL,
    temp_path     TEXT,
    completed     INTEGER DEFAULT 0,
    chunk_bitmap  BLOB,
    status        TEXT NOT NULL DEFAULT 'active'
                  CHECK(status IN ('queued','active','paused','verifying','done','failed','cancelled')),
    window_size   INTEGER NOT NULL DEFAULT 8,
    retry_count   INTEGER NOT NULL DEFAULT 0,
    error         TEXT,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_transfer_peer_hash ON transfer_jobs(peer_id, file_hash);
CREATE INDEX IF NOT EXISTS ix_transfer_status    ON transfer_jobs(status, updated_at DESC);

CREATE TABLE IF NOT EXISTS seed_nodes (
    addr       TEXT PRIMARY KEY,
    node_id    TEXT,
    subnet     TEXT,
    tcp_port   INTEGER,
    last_seen  INTEGER,
    last_epoch INTEGER,
    is_manual  INTEGER DEFAULT 1,
    fail_cnt   INTEGER DEFAULT 0
);
`,
	},
	{
		version: 2,
		name:    "conv_clears",
		// 会话「清空点」：早于它的消息不再显示（消息本身不删除）。
		//
		// 独立一张表而不是塞进 conversations：清空是「本机视图偏好」，
		// 与 conversations 那张由消息派生的表职责不同，混进去会让
		// 「会话行的写入」与「视图偏好」互相牵制。
		sql: `
CREATE TABLE IF NOT EXISTS conv_clears (
    conv_id    TEXT PRIMARY KEY,
    cleared_at INTEGER NOT NULL
);
`,
	},
	{
		version: 3,
		name:    "transfer_waiting",
		// 「等待对方上线」的两个落库字段。
		//
		// 为什么加列而不是加 status 取值：SQLite 改 CHECK 约束必须重建整表
		// （建新表 → 拷数据 → 换名），而 'queued' 本来就在取值域里、
		// 领域态 StateIdle 本来就叫"还没开始"。加两列是纯追加、零搬迁。
		//
		// 注意迁移是【只追加、永不修改】的：已应用过的 SQL 一旦改动，
		// checksum 校验会直接让应用启动失败。
		sql: `
ALTER TABLE transfer_jobs ADD COLUMN wait_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE transfer_jobs ADD COLUMN next_try_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS ix_transfer_waiting ON transfer_jobs(direction, status, next_try_at);
`,
	},
	{
		version: 4,
		name:    "transfer_batch",
		// 文件夹批次的三列。
		//
		// 为什么仍然只是加列：批次不是状态机里的概念（一批任务里每条的状态
		// 各自独立），把它做成 status 取值只会让状态机与列表查询一起变复杂。
		//
		// is_dir 用 INTEGER 0/1：SQLite 没有布尔类型，并存的 0 同时是
		// 「单文件」与「不是目录」两种含义的默认值，正好是我们想要的。
		sql: `
ALTER TABLE transfer_jobs ADD COLUMN batch_id TEXT    NOT NULL DEFAULT '';
ALTER TABLE transfer_jobs ADD COLUMN rel_path TEXT    NOT NULL DEFAULT '';
ALTER TABLE transfer_jobs ADD COLUMN is_dir   INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS ix_transfer_batch ON transfer_jobs(batch_id);
`,
		// batch_id 的取值约定（不加约束，因为没有能表达它的 CHECK）：
		// 由发送端生成、全局唯一（随机串），不是文件夹名也不是序号；
		// 同一 batch_id 只属于一个对端的一次发送。空串 = 单文件传输。
		// 见 ports.TransferRepo.ListByBatch 的契约说明。
	},
	{
		version: 5,
		name:    "group_state_and_deliveries",
		// 群聊完善（2026-10-11）：
		//   1) groups.state —— 群级状态（active/dissolved），解散靠它而不是删光成员；
		//   2) group_msg_deliveries —— 群消息「按成员」的投递跟踪，替代「任一 ACK 即出队」，
		//      让离线成员上线后能补投，并支持「N 人未送达」。
		//
		// 两张表都用纯追加（ADD COLUMN / 新 CREATE TABLE），零搬迁，沿用 v3/v4 的既定做法。
		sql: `
ALTER TABLE groups ADD COLUMN state TEXT NOT NULL DEFAULT 'active';

CREATE TABLE IF NOT EXISTS group_msg_deliveries (
    msg_id    TEXT NOT NULL,
    group_id  TEXT NOT NULL,
    member_id TEXT NOT NULL,
    acked_at  INTEGER,
    PRIMARY KEY (msg_id, member_id)
);
CREATE INDEX IF NOT EXISTS ix_gmd_pending ON group_msg_deliveries(msg_id, acked_at);
`,
	},
	{
		version: 6,
		name:    "group_member_tokens",
		// 群成员「发票 / 撕票」模型（ADR-012）：
		//   groups.mint_seq          —— 群主的【群级】发票计数器（单调递增）
		//   group_members.admit_seq  —— 「票号」（群主签）
		//   group_members.kick_seq   —— 群主撕票号（群主签）
		//   group_members.left_seq   —— 本人撕票号（自签）
		//   group_members.left_sig   —— 本人对 H(group_id|node_id|left_seq) 的签名
		//
		// 默认值刻意取 admit_seq = mint_seq = 1：存量成员等价于「拿到过 1 号票」，
		// 语义上正是升级前的那次历史准入 —— 存量数据不需要人工修补。
		//
		// 仍然只是加列（沿用 v3–v5 的既定做法）：SQLite 改 CHECK 必须重建整表，
		// 而这些字段都能用 ADD COLUMN + 默认值表达。
		sql: `
ALTER TABLE groups        ADD COLUMN mint_seq INTEGER NOT NULL DEFAULT 1;
ALTER TABLE group_members ADD COLUMN admit_seq INTEGER NOT NULL DEFAULT 1;
ALTER TABLE group_members ADD COLUMN kick_seq  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE group_members ADD COLUMN left_seq  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE group_members ADD COLUMN left_sig  TEXT;
`,
	},
	{
		version: 7,
		name:    "file_share",
		// 文件共享（FR-14，2026-10-14）：
		//   1) shares      —— 本机发布的共享（元数据 + 可见性 + 根路径）；
		//   2) share_items —— 共享的文件清单，按 (share_id, path) 唯一；
		//   3) peers.shares —— 他人共享摘要的落库列（JSON 数组）。
		//
		// 为什么清单要单开一张表而不是给 shares 加一个 JSON 列：清单是这条记录里
		// 唯一会随目录变化的东西，也是唯一可能上万行的东西；塞进 JSON 列之后
		// 「刷新索引」就变成了「读整行、改一个字段、写回整行」；而按 share_id
		// 整份替换清单（DELETE + INSERT 同一事务）在这里是天然免费的，
		// 也正是「刷新后已删除的文件真的消失」所必需的语义。
		//
		// peers.shares 用 TEXT 存 JSON：摘要本身是可选、可丢的展示数据，
		// 为它开三张关联表会让「一条节点条目」的读写变成三次往返，
		// 而它每次 ANNOUNCE / 目录交换本来就要整份覆盖写一遍。
		//
		// 沿用 v3–v6 的既定做法：纯 CREATE TABLE / ADD COLUMN，零重建。
		sql: `
CREATE TABLE IF NOT EXISTS shares (
    share_id    TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    owner_id    TEXT NOT NULL,
    owner_name  TEXT NOT NULL DEFAULT '',
    root_path   TEXT NOT NULL,
    is_dir      INTEGER NOT NULL DEFAULT 1,
    vis_type    TEXT NOT NULL DEFAULT 'users',
    vis_allowed TEXT NOT NULL DEFAULT '',
    total_size  INTEGER NOT NULL DEFAULT 0,
    item_count  INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_shares_created ON shares(created_at);

CREATE TABLE IF NOT EXISTS share_items (
    share_id TEXT NOT NULL,
    path     TEXT NOT NULL,
    name     TEXT NOT NULL,
    size     INTEGER NOT NULL DEFAULT 0,
    hash     TEXT NOT NULL DEFAULT '',
    mtime    INTEGER NOT NULL DEFAULT 0,
    -- is_dir 标记【空目录】占位项（零字节但 size=0 也可能是真实文件，
    -- 因此不能拿 size 反推）：少了它，共享里的空目录在接收端会凭空消失。
    is_dir   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (share_id, path)
);

ALTER TABLE peers ADD COLUMN shares TEXT NOT NULL DEFAULT '';
`,
		// vis_allowed 的取值约定（不加约束，因为没有能表达它的 CHECK）：
		// 逗号分隔的 node_id / group_id 列表，已去重排序（share.Visibility.Normalize）。
		// 空串与「未指定任何人」是同一件事：除 owner 外无人可见。
		//
		// item_count / total_size 是清单的冗余计数：界面列表要在不读
		// share_items 的情况下显示「N 个文件 / M MB」，而 share_items
		// 可能是上万行 —— COUNT/SUM 每次列表刷新跑一遍是不必要的开销。
	},
}

func migrationChecksum(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

// migrate 在启动时把未应用的迁移按版本顺序执行（每个迁移一个事务）。
func (d *DB) migrate() error {
	if _, err := d.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
        version    INTEGER PRIMARY KEY,
        applied_at INTEGER NOT NULL,
        checksum   TEXT NOT NULL
    )`); err != nil {
		return fmt.Errorf("sqlite: create schema_migrations: %w", err)
	}

	applied := make(map[int]string)
	rows, err := d.db.Query(`SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("sqlite: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			_ = rows.Close()
			return err
		}
		applied[v] = sum
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, m := range migrations {
		sum := migrationChecksum(m.sql)
		if prev, ok := applied[m.version]; ok {
			if prev != sum {
				return fmt.Errorf("sqlite: migration %d (%s) checksum mismatch: applied=%s current=%s",
					m.version, m.name, prev, sum)
			}
			continue // 幂等：已应用则跳过
		}

		tx, err := d.db.Begin()
		if err != nil {
			return err
		}
		if err := execScript(tx, m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlite: apply migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations(version, applied_at, checksum) VALUES(?,?,?)`,
			m.version, time.Now().UnixMilli(), sum,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// execScript 逐条执行 SQL 脚本。
//
// 不依赖驱动对多语句 Exec 的支持：拆开执行行为更可预期，出错也能定位到语句。
// 本项目的 schema 中不存在「字符串字面量内含分号」的情况。
func execScript(tx *sql.Tx, script string) error {
	for _, stmt := range strings.Split(script, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%w (near: %.60s)", err, stmt)
		}
	}
	return nil
}
