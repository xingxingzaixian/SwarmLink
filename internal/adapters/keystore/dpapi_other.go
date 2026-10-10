//go:build !windows

package keystore

import "errors"

// vaultFileName：非 Windows 没有机器级 DPAPI，托管副本就是明文 PEM
// （与数据副本同一保护等级），保护来自文件权限与「它不在用户目录里」。
//
// 注意与 Windows 侧的一处不对称：这里的文件名与数据副本同名（都是 node.key），
// 因此「vaultDir 与 dataDir 重合」这件事全靠 config.VaultDir 的守卫挡住，
// 而该守卫不解析符号链接（设计 §4.1 已记录）。Windows 侧的 .dpapi 后缀是
// 独立于守卫的第二道防线，这里没有。
const vaultFileName = "node.key"

// vaultEntropy 在非 Windows 上不参与运算（没有机器级 DPAPI）。保留同名符号是为了让两个
// 平台文件提供同一组标识符，vault.go 因此不需要任何平台判断。
var vaultEntropy = []byte("swarmlink-vault-v1")

func protectVault(pem []byte) ([]byte, error) { return protectVaultWith(pem, vaultEntropy) }

func unprotectVault(blob []byte) ([]byte, error) { return unprotectVaultWith(blob, vaultEntropy) }

// protectVaultWith / unprotectVaultWith 在非 Windows 上是恒等变换。
//
// 空载荷与 Windows 侧一样报错：若这里放行，`vault.save([]byte{})` 会在 Unix 上
// 写出一个空文件并返回 nil，而同一个调用在 Windows 上会走「写入失败 → 降级告警」
// —— 同一个逻辑事件在两个平台分流，是抽象的漏洞。
func protectVaultWith(pem, _ []byte) ([]byte, error) {
	if len(pem) == 0 {
		return nil, errors.New("protect: empty payload")
	}
	return pem, nil
}

func unprotectVaultWith(blob, _ []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("unprotect: empty payload")
	}
	return blob, nil
}
