// Package sqlite 是 ports 的持久化实现（纯 Go 驱动，无 CGO）。
//
// 关键点：
//   - WAL + busy_timeout + synchronous=NORMAL + foreign_keys=on
//   - 所有写操作经【单写 goroutine】串行化，消除多连接争锁
//   - 迁移版本化并带 checksum，保证可重入与可审计
package sqlite

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// DB 持有连接池与写队列。
type DB struct {
	db     *sql.DB
	writer *writer
	path   string
}

// DSN 构造连接串。
//
// 逐项说明：
//   - journal_mode(WAL)     并发读写的基础
//   - busy_timeout(5000)    遇锁等待而非立即失败
//   - synchronous(NORMAL)    WAL 下的性能/安全折中
//   - foreign_keys(on)       Go 驱动默认【不开】外键约束，必须显式打开
func DSN(path string) string {
	return "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(on)"
}

// Open 打开数据库、执行迁移并启动写队列。
func Open(path string) (*DB, error) {
	sqldb, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	sqldb.SetMaxOpenConns(5)
	sqldb.SetMaxIdleConns(2)
	sqldb.SetConnMaxLifetime(0)

	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("sqlite: ping: %w", err)
	}

	d := &DB{db: sqldb, path: path}
	if err := d.migrate(); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	d.writer = newWriter(sqldb)
	return d, nil
}

// Close 停止写队列并关闭连接池。
func (d *DB) Close() error {
	if d.writer != nil {
		d.writer.stop()
	}
	return d.db.Close()
}

// Path 返回数据库文件路径。
func (d *DB) Path() string { return d.path }

// Write 通过单写 goroutine 执行一个写事务。
func (d *DB) Write(fn func(*sql.Tx) error) error {
	if d.writer == nil {
		return fmt.Errorf("sqlite: writer not initialised")
	}
	return d.writer.submit(fn)
}

// Query 执行读查询。
func (d *DB) Query(q string, args ...any) (*sql.Rows, error) { return d.db.Query(q, args...) }

// QueryRow 执行单行读查询。
func (d *DB) QueryRow(q string, args ...any) *sql.Row { return d.db.QueryRow(q, args...) }

// ---------------------------------------------------------------------------
// 共用小工具
// ---------------------------------------------------------------------------

type rowScanner interface{ Scan(dest ...any) error }

func msOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func timeOf(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func b64(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

func unb64(s string) []byte {
	if s == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

func nullInt64(v sql.NullInt64) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

func nullString(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}
