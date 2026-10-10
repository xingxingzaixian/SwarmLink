//go:build !windows

package keystore

// vaultFileName：非 Windows 没有机器级 DPAPI，托管副本就是明文 PEM
// （与数据副本同一保护等级），保护来自文件权限与「它不在用户目录里」。
const vaultFileName = "node.key"

// protectVault / unprotectVault 在非 Windows 上是恒等变换。
//
// 保留这两个函数而不是在各调用点写 if runtime.GOOS：调用方（vault.go）不需要知道平台差异，
// 而 Windows 那侧的 DPAPI 语义也因此被完全封装在一个文件里。
func protectVault(pem []byte) ([]byte, error) { return pem, nil }

func unprotectVault(blob []byte) ([]byte, error) { return blob, nil }
