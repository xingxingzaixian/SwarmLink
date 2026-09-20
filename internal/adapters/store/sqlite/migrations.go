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
