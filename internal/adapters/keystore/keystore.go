// Package keystore 负责节点密钥文件的持久化。
//
// 它存在的唯一原因：domain/identity 必须保持零 os 依赖（红线 R1），
// 而密钥读写是文件 I/O，因此下沉到本适配器。
//
// 除了数据目录下的权威副本（<dataDir>/identity/node.key），本包还维护一份
// 【机器级托管副本】（vault，见 vault.go 与 config.VaultDir）：数据副本缺失或
// 损坏时由它恢复身份，使「换数据目录 / 重装应用」不再换节点身份。
package keystore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
)

// KeyPath 返回私钥文件路径：<configDir>/identity/node.key。
func KeyPath(configDir string) string {
	return filepath.Join(configDir, "identity", "node.key")
}

// Result 是一次身份加载的结果。
type Result struct {
	KeyPair *identity.KeyPair

	// Restored 为真表示数据副本缺失（或损坏），本次身份来自机器级托管副本。
	Restored bool

	// Notice 非空表示需要告知用户的事：换数据目录后身份被恢复、数据副本损坏被修复、
	// 托管副本损坏导致换了新身份、托管不可用导致降级。
	// 调用方负责呈现（当前只有日志，见设计文档 §4.6 与「非目标」）。
	Notice string
}

// LoadOrCreate 读取已有密钥；数据副本缺失时尝试机器级托管副本；都没有则生成新身份。
//
// vaultDir 为 "" 表示禁用托管（便携模式 / 与数据目录重合 / 平台没有机器级位置），
// 此时行为与历史版本完全一致。
//
// 决策顺序与每一格的理由见设计文档 §4.3。两条不可动摇的约束：
//   - 数据副本是【权威副本】：两份不一致时以它为准，并用它覆盖托管副本；
//   - 只有在「数据副本损坏 且 没有可用托管副本」时才报错 —— 其余情况绝不静默换身份。
func LoadOrCreate(dataDir, vaultDir string) (Result, error) {
	v := newVault(vaultDir)
	dataPath := KeyPath(dataDir)

	data, err := os.ReadFile(dataPath)
	switch {
	case err == nil:
		kp, perr := parseKeyPair(data)
		if perr == nil {
			// 数据副本可用 ⇒ 让它成为托管副本的真相（幂等同步，失败不影响启动）
			syncVault(v, data)
			return Result{KeyPair: kp}, nil
		}
		// 数据副本损坏 ⇒ 先试托管副本；两边都不可用就保持历史语义：报错，不静默换身份。
		//
		// 这里的 broken 一律忽略（包括 errVaultUnavailable）：数据副本坏 + 托管副本
		// 读不到，结论就是「没有可用托管副本」，与「托管被禁用」同路 —— 报错、不写任何东西。
		if res, ok, _ := v.restore(dataPath); ok {
			res.Notice = "身份文件损坏，已从机器级托管副本恢复：" + dataPath
			return res, nil
		}
		return Result{}, fmt.Errorf("keystore: %w", perr)

	case errors.Is(err, os.ErrNotExist):
		// 首次运行，或换了数据目录：先问托管副本（见下方 restore）

	default:
		return Result{}, fmt.Errorf("keystore: read key: %w", err)
	}

	res, ok, broken := v.restore(dataPath)
	switch {
	case ok:
		res.Notice = "已从机器级托管副本恢复节点身份：" + v.filePath()
		return res, nil

	case errors.Is(broken, errVaultUnavailable):
		// 读不到（权限 / IO / 路径被占）：降级为「只用数据目录」，且【不碰】托管副本 ——
		// 读失败不是损坏的证据，覆盖它等于把一个完好身份换掉。
		// 传 nil vault：本次不尝试写入（写入大概率同样失败，且失败会污染日志）。
		res, gerr := generateAndPersist(nil, dataPath)
		if gerr != nil {
			return Result{}, gerr
		}
		res.Notice = "无法读取机器级托管副本，已降级为仅使用数据目录：" + broken.Error()
		return res, nil

	case broken != nil:
		// 真损坏（解包失败 / payload 非法）：生成新身份并覆盖它，否则每次启动都刷同一条告警
		res, gerr := generateAndPersist(v, dataPath)
		if gerr != nil {
			return Result{}, gerr
		}
		res.Notice = "机器级托管副本损坏，已生成新的节点身份：" + broken.Error()
		return res, nil
	}

	return generateAndPersist(v, dataPath)
}

// parseKeyPair 解析 PEM 私钥并构造密钥对。
func parseKeyPair(pemBytes []byte) (*identity.KeyPair, error) {
	priv, err := identity.ParsePrivateKeyPEM(pemBytes)
	if err != nil {
		return nil, err
	}
	return identity.NewKeyPair(priv)
}

// generateAndPersist 生成新身份并写入数据副本（+ 尽力写托管副本）。
//
// 目录权限 0700，文件权限 0600（Windows 不遵守 POSIX 权限位，属平台限制）。
// v 传 nil 表示本次不写托管副本（降级路径）。
func generateAndPersist(v *vault, dataPath string) (Result, error) {
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		return Result{}, err
	}
	pemBytes, err := identity.MarshalPrivateKeyPEM(kp.PrivateKey())
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dataPath), 0o700); err != nil {
		return Result{}, fmt.Errorf("keystore: mkdir: %w", err)
	}
	if err := os.WriteFile(dataPath, pemBytes, 0o600); err != nil {
		return Result{}, fmt.Errorf("keystore: write key: %w", err)
	}

	res := Result{KeyPair: kp, Notice: "已生成新的节点身份：" + kp.NodeID().String()}
	if v.enabled() {
		if err := v.save(pemBytes); err != nil {
			// 托管失败只降级，不阻塞：用户仍然能正常用，只是换数据目录后会换身份
			res.Notice += "（机器级托管副本写入失败，换数据目录后会变成新身份：" + err.Error() + "）"
		}
	}
	return res, nil
}

// syncVault 让托管副本与数据副本保持一致（只在内容不同时写）。
// 失败只影响「下次能否恢复」，因此忽略错误：恢复是尽力而为，不该阻塞启动。
func syncVault(v *vault, data []byte) {
	if !v.enabled() {
		return
	}
	if cur, err := v.load(); err == nil && bytes.Equal(cur, data) {
		return
	}
	_ = v.save(data)
}

// restore 从托管副本恢复身份，并把 PEM 回填到数据副本。
//
// 三个返回值：
//   - ok=true        ：恢复成功（res.KeyPair 可用）
//   - broken != nil  ：托管副本存在但不可用，需调用方再分两类：
//     errors.Is(broken, errVaultUnavailable) 只是【读不到】，降级、不覆盖；
//     其余是【真损坏】，可以生成新身份并覆盖它。
//   - 两者都为零值   ：没有托管副本（首次运行，或托管被禁用）
func (v *vault) restore(dataPath string) (res Result, ok bool, broken error) {
	if !v.enabled() {
		return Result{}, false, nil
	}
	pemBytes, err := v.load()
	if errors.Is(err, os.ErrNotExist) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	// 注意：非 Windows 侧 unprotectVault 是恒等变换、永不失败，
	// 因此「payload 坏了」只能在这里靠解析判定，不能指望 load 报错。
	kp, err := parseKeyPair(pemBytes)
	if err != nil {
		return Result{}, false, fmt.Errorf("keystore: vault payload invalid: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dataPath), 0o700); err != nil {
		return Result{}, false, fmt.Errorf("keystore: mkdir: %w", err)
	}
	if err := os.WriteFile(dataPath, pemBytes, 0o600); err != nil {
		return Result{}, false, fmt.Errorf("keystore: write key: %w", err)
	}
	return Result{KeyPair: kp, Restored: true}, true, nil
}
