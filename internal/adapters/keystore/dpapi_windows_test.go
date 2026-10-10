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

	// 篡改必须报错（实测 DPAPI 返回 "The data is invalid."），不能 panic、也不能返回垃圾
	bad := append([]byte(nil), sealed...)
	bad[len(bad)/2] ^= 0xFF
	if _, err := unprotectVault(bad); err == nil {
		t.Fatal("被篡改的 blob 居然解开了")
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
