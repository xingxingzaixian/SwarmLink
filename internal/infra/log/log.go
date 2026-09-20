// Package log 封装 log/slog，提供 P2P 排障所需的三级上下文注入：
// node_id（进程级）· conn_id（连接级）· job_id（传输级）。
package log

import (
	"log/slog"
	"os"
)

// New 构造 JSON 结构化日志器。
func New(level slog.Level, nodeID string) *slog.Logger {
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	l := slog.New(h)
	if nodeID != "" {
		l = l.With(slog.String("node_id", nodeID))
	}
	return l
}

// WithConn 注入连接上下文。
func WithConn(l *slog.Logger, connID string) *slog.Logger {
	return l.With(slog.String("conn_id", connID))
}

// WithJob 注入传输任务上下文，使一次传输的因果链可从日志还原。
func WithJob(l *slog.Logger, jobID string) *slog.Logger {
	return l.With(slog.String("job_id", jobID))
}

// WithPeer 注入对端上下文。
func WithPeer(l *slog.Logger, peerID string) *slog.Logger {
	return l.With(slog.String("peer_id", peerID))
}
