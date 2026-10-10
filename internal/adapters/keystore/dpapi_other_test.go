//go:build !windows

package keystore

import (
	"bytes"
	"testing"
)

// 非 Windows 侧没有机器级 DPAPI：托管副本就是明文，加解密是恒等变换。
//
// 这个文件在 Windows 上不参与编译，但它钉住的是「两个平台提供同一份契约」——
// 尤其是空载荷必须与 Windows 一样报错（否则 vault.save([]byte{}) 会在 Unix 上
// 写出一个空文件并返回 nil）。
func TestVaultIdentityRoundTripOnNonWindows(t *testing.T) {
	plain := []byte("-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n")

	sealed, err := protectVault(plain)
	if err != nil {
		t.Fatalf("protect: %v", err)
	}
	if !bytes.Equal(sealed, plain) {
		t.Fatal("非 Windows 侧应当是恒等变换（明文存储）")
	}
	if got, err := unprotectVault(sealed); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("unprotect: err=%v", err)
	}

	if _, err := protectVault(nil); err == nil {
		t.Fatal("空载荷必须报错：与 Windows 侧保持一致")
	}
	if _, err := unprotectVault(nil); err == nil {
		t.Fatal("空密文必须报错")
	}
}
