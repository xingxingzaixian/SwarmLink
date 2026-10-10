package keystore

import (
	"fmt"
	"os"
	"path/filepath"
)

// vault 是身份的【机器级托管副本】。
//
// 它只负责三件事：定位、原子写、以及把编码/加密委托给平台文件（dpapi_*.go）。
// 因此本文件跨平台可编译、可测试，且不需要任何 runtime.GOOS 判断。
//
// dir 为空表示禁用（便携模式，或解析结果与数据目录重合 —— 见 config.VaultDir）。
//
// 为什么必须原子写：断电留下半截 blob 会被下次启动判成「损坏」，
// 而「损坏」的处置是「换新身份」—— 那正是本功能要消灭的东西。
type vault struct{ dir string }

func newVault(dir string) *vault { return &vault{dir: dir} }

// enabled 报告本次运行是否启用了托管。
func (v *vault) enabled() bool { return v != nil && v.dir != "" }

// filePath 返回托管副本的完整路径（文件名由平台文件决定）。
func (v *vault) filePath() string {
	return filepath.Join(v.dir, "identity", vaultFileName)
}

// load 读取并解开托管副本。
//
// 文件不存在时返回的 error 必须能被 errors.Is(err, os.ErrNotExist) 识别 ——
// 调用方（LoadOrCreate）靠它区分「没有托管副本」（首次运行）与「托管副本坏了」
// （要覆盖并告警）。因此这里不包装 ErrNotExist，直接原样返回。
func (v *vault) load() ([]byte, error) {
	if !v.enabled() {
		return nil, os.ErrNotExist
	}
	blob, err := os.ReadFile(v.filePath())
	if err != nil {
		return nil, err
	}
	pemBytes, err := unprotectVault(blob)
	if err != nil {
		return nil, fmt.Errorf("keystore: %w", err)
	}
	return pemBytes, nil
}

// save 原子写入托管副本：写 .tmp → fsync → rename。
//
// 三步缺一不可：少了 fsync，rename 之后的内容可能还在页缓存里，
// 断电后正式文件是空的；少了 rename，写到一半就是半个文件。
func (v *vault) save(pem []byte) error {
	if !v.enabled() {
		return nil
	}
	target := v.filePath()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("keystore: vault mkdir: %w", err)
	}
	blob, err := protectVault(pem)
	if err != nil {
		return fmt.Errorf("keystore: %w", err)
	}

	tmp := target + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("keystore: vault open tmp: %w", err)
	}
	if _, err := f.Write(blob); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("keystore: vault write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("keystore: vault sync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("keystore: vault close tmp: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("keystore: vault rename: %w", err)
	}
	return nil
}
