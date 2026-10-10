//go:build windows

package keystore

import (
	"bytes"
	"testing"
)

// DPAPI 机器作用域必须真的加密、能原样解回，并且对被篡改的 blob 报错 ——
// 最后一条是「托管副本损坏」分支的判据：它不能退化成静默返回垃圾。
func TestVaultRoundTripAndTamperDetection(t *testing.T) {
	plain := []byte("-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n")

	sealed, err := protectVault(plain)
	if err != nil {
		t.Fatalf("protect: %v", err)
	}
	if bytes.Contains(sealed, []byte("PRIVATE KEY")) {
		t.Fatal("密文里仍能看到明文特征：DPAPI 没有真的加密")
	}

	got, err := unprotectVault(sealed)
	if err != nil {
		t.Fatalf("unprotect: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("往返不一致：got %q", got)
	}

	// 篡改必须报错（实测 DPAPI 返回 "The data is invalid."），不能 panic、也不能返回垃圾。
	// 翻转最后一个字节：它必然落在密文/认证区，比翻中间字节更不依赖 blob 长度。
	bad := append([]byte(nil), sealed...)
	bad[len(bad)-1] ^= 0xFF
	if _, err := unprotectVault(bad); err == nil {
		t.Fatal("被篡改的 blob 居然解开了")
	}
}

// 熵必须真的参与运算：换一个熵就解不开同一个 blob。
//
// 这是 D2 安全语义里唯一能被测试钉住的部分（LOCAL_MACHINE 在单用户环境下无法端到端验证，
// 已作为残余缺口记入 ADR-012）。没有这条断言，把 entropy 传成 nil 也不会有任何用例变红。
func TestVaultEntropyIsActuallyUsed(t *testing.T) {
	plain := []byte("-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n")
	const right = "swarmlink-vault-v1"

	sealed, err := protectVaultWith(plain, []byte(right))
	if err != nil {
		t.Fatalf("protect: %v", err)
	}
	if _, err := unprotectVaultWith(sealed, []byte(right+"-x")); err == nil {
		t.Fatal("换了熵仍能解开 ⇒ entropy 没有被传给 DPAPI")
	}
	// 反向断言：同一个熵必须能解开 —— 否则上一条可能只是「什么都解不开」
	if got, err := unprotectVaultWith(sealed, []byte(right)); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("相同熵应当能解开：err=%v", err)
	}
}

// 空载荷必须是错误：让调用方早失败，而不是写一个能解开但内容是空的副本。
func TestVaultRejectsEmptyPayload(t *testing.T) {
	if _, err := protectVault(nil); err == nil {
		t.Fatal("空载荷必须报错")
	}
	if _, err := unprotectVault(nil); err == nil {
		t.Fatal("空密文必须报错")
	}
}
