// Package keystore 负责节点密钥文件的持久化。
//
// 它存在的唯一原因：domain/identity 必须保持零 os 依赖（红线 R1），
// 而密钥读写是文件 I/O，因此下沉到本适配器。
package keystore

import (
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

// LoadOrCreate 读取已有密钥；不存在则生成并落盘。
// 目录权限 0700，文件权限 0600（Windows 不遵守 POSIX 权限位，属平台限制）。
func LoadOrCreate(configDir string) (*identity.KeyPair, error) {
	keyPath := KeyPath(configDir)

	data, err := os.ReadFile(keyPath)
	switch {
	case err == nil:
		priv, perr := identity.ParsePrivateKeyPEM(data)
		if perr != nil {
			return nil, fmt.Errorf("keystore: %w", perr)
		}
		kp, perr := identity.NewKeyPair(priv)
		if perr != nil {
			return nil, fmt.Errorf("keystore: %w", perr)
		}
		return kp, nil
	case errors.Is(err, os.ErrNotExist):
		// 首次运行，继续生成。
	default:
		return nil, fmt.Errorf("keystore: read key: %w", err)
	}

	kp, err := identity.GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	pemBytes, err := identity.MarshalPrivateKeyPEM(kp.PrivateKey())
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return nil, fmt.Errorf("keystore: mkdir: %w", err)
	}
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("keystore: write key: %w", err)
	}
	return kp, nil
}
