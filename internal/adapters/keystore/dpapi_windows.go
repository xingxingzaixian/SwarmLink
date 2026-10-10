//go:build windows

package keystore

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// vaultFileName 是托管副本在 <vaultDir>/identity/ 下的文件名。
//
// Windows 上它总是 DPAPI 密文，因此带 .dpapi 后缀 —— 避免被误当成私钥明文备份
// （用户手工拷贝、或按名字去读它时，后缀是唯一的提示）。
const vaultFileName = "node.key.dpapi"

// vaultEntropy 是 DPAPI 的附加熵。
//
// 它不是秘密（写死在二进制里），只做用途隔离：同一台机器上别的应用拿不走、
// 也换不到本应用的 blob。
var vaultEntropy = []byte("swarmlink-vault-v1")

// protectVault / unprotectVault 用固定熵包裹/解开托管副本（语义见 protectVaultWith）。
func protectVault(pem []byte) ([]byte, error) { return protectVaultWith(pem, vaultEntropy) }

func unprotectVault(blob []byte) ([]byte, error) { return unprotectVaultWith(blob, vaultEntropy) }

// protectVaultWith / unprotectVaultWith 用 DPAPI 机器作用域
// （CRYPTPROTECT_LOCAL_MACHINE）包裹/解开托管副本。
//
// 熵做成参数而不是直接读包级变量，是为了让「熵真的参与了运算」这条属性可被测试
// （换一个熵必须解不开）—— 否则把 entropy 传成 nil 也不会有任何用例变红。
//
// 语义（设计文档 D2）：本机任何用户都能解开 ⇒ 同机其他用户可继承本机身份，
// 这是「机器级托管」的既定取舍；真正被挡住的是「把 blob 拷到别的机器」。
//
// 注意 DPAPI 的输出内存由 LocalAlloc 分配，必须 LocalFree —— 漏掉就是每次启动泄漏一块，
// 而这类泄漏任何 Go 层的检测都抓不到，只能靠这行注释与人工评审。
func protectVaultWith(pem, entropy []byte) ([]byte, error) {
	if len(pem) == 0 {
		return nil, errors.New("protect: empty payload")
	}
	in := windows.DataBlob{Size: uint32(len(pem)), Data: &pem[0]}
	var ent *windows.DataBlob
	if len(entropy) > 0 {
		ent = &windows.DataBlob{Size: uint32(len(entropy)), Data: &entropy[0]}
	}

	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, ent, 0, nil, windows.CRYPTPROTECT_LOCAL_MACHINE, &out); err != nil {
		return nil, fmt.Errorf("protect: %w", err)
	}
	defer func() { _, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data))) }()

	buf := make([]byte, out.Size)
	copy(buf, unsafe.Slice(out.Data, out.Size))
	return buf, nil
}

// unprotectVaultWith 解开托管副本。
//
// blob 被篡改、或来自别的机器/别的主密钥时返回错误，调用方据此走
// 「托管副本损坏」分支 —— 绝不能把返回值当密钥用。
func unprotectVaultWith(blob, entropy []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("unprotect: empty payload")
	}
	in := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	var ent *windows.DataBlob
	if len(entropy) > 0 {
		ent = &windows.DataBlob{Size: uint32(len(entropy)), Data: &entropy[0]}
	}

	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, ent, 0, nil, windows.CRYPTPROTECT_LOCAL_MACHINE, &out); err != nil {
		return nil, fmt.Errorf("unprotect: %w", err)
	}
	defer func() { _, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data))) }()

	buf := make([]byte, out.Size)
	copy(buf, unsafe.Slice(out.Data, out.Size))
	return buf, nil
}
