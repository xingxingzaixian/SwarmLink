package app

import (
	"errors"
	"fmt"
	"testing"
)

// 分类规则表：主判据是「对端可达性」，错误文本只做兜底。
//
// 反例比正例更重要 —— 把真失败误判成"等对方上线"会让用户永远等一个
// 不可能成功的任务，比当场报错糟糕得多。
func TestShouldWaitForPeer(t *testing.T) {
	dialErr := errors.New(`tcp: dial 192.168.1.9:41641: connectex: No connection could be made`)

	cases := []struct {
		name        string
		reachable   bool
		sessionLive bool
		err         error
		want        bool
	}{
		{"对端不在目录里 → 等", false, false, errors.New("transfer: no known address for peer abc"), true},
		{"对端不在目录里，哪怕错误看不懂 → 等", false, false, errors.New("some brand new error"), true},
		{"会话没了（对方掉线） → 等", true, false, errors.New("write tcp: connection reset by peer"), true},
		{"看起来可达但发不出去（拨号失败） → 等", true, true, dialErr, true},
		{"等 FILE_META_ACK 超时 → 等", true, true, errors.New("transfer: timeout waiting for FILE_META_ACK"), true},
		{"等 FILE_DONE_ACK 超时 → 等", true, true, errors.New("transfer: timeout waiting for FILE_DONE_ACK"), true},

		{"文件不存在 → 真失败", true, true, errors.New("transfer: open /tmp/gone.bin: no such file or directory"), false},
		{"校验失败 → 真失败", true, true, errors.New("transfer: peer verify failed: bad chunk 3"), false},
		{"元数据非法 → 真失败", true, true, errors.New("transfer: invalid meta (total_chunks=0)"), false},
		{"空文件 → 真失败", true, true, errors.New("transfer: empty file"), false},
		{"ctx 取消 → 真失败", true, true, fmt.Errorf("transfer: hash /tmp/a.bin: %w", errors.New("context canceled")), false},
		// 本地读文件读到一半文件被截断：看起来像 EOF，但绝不能当成"等对方上线"
		{"读文件时 unexpected EOF → 真失败", true, true, errors.New("read /tmp/a.bin: unexpected EOF"), false},
		{"nil 错误 → 不算等待（调用方只在 err != nil 时问）", false, false, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldWaitForPeer(failureEvidence{
				PeerReachable: tc.reachable,
				SessionAlive:  tc.sessionLive,
				Err:           tc.err,
			})
			if got != tc.want {
				t.Errorf("shouldWaitForPeer = %v, want %v", got, tc.want)
			}
		})
	}
}
