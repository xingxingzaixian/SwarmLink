package sqlite

import (
	"database/sql"
	"errors"
	"sync"
)

// ErrClosed 表示写队列已停止。
var ErrClosed = errors.New("sqlite: writer closed")

type writeTask struct {
	fn   func(*sql.Tx) error
	done chan error
}

// writer 是单写 goroutine。
//
// 为什么需要它：SQLite 是单写者模型，即使 SetMaxOpenConns(5)，
// 同一时刻也只有一个连接能写。多连接并发写会退化为「争锁 + busy_timeout 等待」。
// 用一个 goroutine 串行化所有写事务，既消除争锁，也让事务边界清晰可推理。
type writer struct {
	mu     sync.Mutex
	closed bool
	db     *sql.DB
	ch     chan *writeTask
	quit   chan struct{}
	wg     sync.WaitGroup
}

func newWriter(db *sql.DB) *writer {
	w := &writer{
		db:   db,
		ch:   make(chan *writeTask, 256),
		quit: make(chan struct{}),
	}
	w.wg.Add(1)
	go w.loop()
	return w
}

func (w *writer) loop() {
	defer w.wg.Done()
	for {
		select {
		case <-w.quit:
			// 退出前把队列里剩下的任务做完，避免调用方永久阻塞。
			for {
				select {
				case t := <-w.ch:
					t.done <- w.exec(t.fn)
				default:
					return
				}
			}
		case t := <-w.ch:
			t.done <- w.exec(t.fn)
		}
	}
}

func (w *writer) exec(fn func(*sql.Tx) error) error {
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// submit 提交一个写事务并等待完成。
//
// 持锁发送：stop() 也在锁内置 closed，因此不存在「向已停止的队列发送」的窗口。
func (w *writer) submit(fn func(*sql.Tx) error) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrClosed
	}
	t := &writeTask{fn: fn, done: make(chan error, 1)}
	w.ch <- t
	w.mu.Unlock()
	return <-t.done
}

func (w *writer) stop() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.quit)
	w.mu.Unlock()
	w.wg.Wait()
}
