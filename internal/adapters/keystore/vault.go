package keystore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// errVaultUnavailable 表示托管副本【存在，但读不出来】（权限不足、IO 错误、路径被目录占住）。
//
// 必须与「副本损坏」严格区分：**读不到不是损坏的证据**，绝不能据此覆盖它 ——
// 否则一次权限问题就会把一个完好但读不到的权威副本换成新身份。
//
// LoadOrCreate 的三分支判定就靠它：
//   - errors.Is(err, os.ErrNotExist) → 没有托管副本（首次运行，正常的）
//   - errors.Is(err, errVaultUnavailable) → 托管不可用 → 降级、不覆盖
//   - 其它（解包失败、payload 非法）→ 损坏 → 可以覆盖
var errVaultUnavailable = errors.New("keystore: vault unreadable")

// vault 是身份的【机器级托管副本】。
//
// 它只负责三件事：定位、原子写、以及把编码/加密委托给平台文件（dpapi_*.go）。
// 因此本文件跨平台可编译、可测试，且不需要任何 runtime.GOOS 判断。
//
// dir 为空表示禁用（便携模式，或解析结果与数据目录重合 —— 见 config.VaultDir）。
//
// 所有方法都接受 nil 接收者（等价于禁用），调用方不必先判空。
//
// 为什么必须原子写：断电留下半截 blob 会被下次启动判成「损坏」，
// 而「损坏」的处置是「换新身份」—— 那正是本功能要消灭的东西。
type vault struct{ dir string }

func newVault(dir string) *vault { return &vault{dir: dir} }

// enabled 报告本次运行是否启用了托管。
func (v *vault) enabled() bool { return v != nil && v.dir != "" }

// filePath 返回托管副本的完整路径（文件名由平台文件决定）。
func (v *vault) filePath() string {
	if v == nil {
		return ""
	}
	return filepath.Join(v.dir, "identity", vaultFileName)
}

// load 读取并解开托管副本。
//
// 错误分类见 errVaultUnavailable：调用方靠它决定「降级」还是「覆盖」，不要在这里合并。
func (v *vault) load() ([]byte, error) {
	if !v.enabled() {
		return nil, os.ErrNotExist
	}
	blob, err := os.ReadFile(v.filePath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		// 原样返回：调用方用 errors.Is 判断「没有副本」
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%w: %w", errVaultUnavailable, err)
	}

	pemBytes, err := unprotectVault(blob)
	if err != nil {
		return nil, fmt.Errorf("keystore: vault unprotect %s: %w", v.filePath(), err)
	}
	return pemBytes, nil
}

// save 原子写入托管副本：写 .tmp → fsync → rename。
//
// 三步缺一不可：少了 fsync，rename 之后的内容可能还在页缓存里，断电后正式文件是空的；
// 少了 rename，写到一半就是半个文件。（严格说 fsync 保证的是【内容】落盘，
// 「target 指向新 inode」这条目录项并不由它保证 —— 那需要再 fsync 父目录，本项目不做。）
func (v *vault) save(pem []byte) error {
	if !v.enabled() {
		return nil
	}
	// 先编码再建目录：payload 非法时不该留下任何痕迹
	blob, err := protectVault(pem)
	if err != nil {
		return fmt.Errorf("keystore: %w", err)
	}

	target := v.filePath()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("keystore: vault mkdir: %w", err)
	}

	// 临时名带 pid：两个实例共用同一个 vault 时，固定名会让后启动者的 O_TRUNC
	// 把先启动者刚写完的 .tmp 截成 0 字节，随后被 rename 成正式文件 ——
	// 恰好造出本节要消灭的「半个文件」形态。
	tmp := fmt.Sprintf("%s.tmp.%d", target, os.Getpid())
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
	// Close 的错误必须单独检查：defer 会把「写完但没落盘」吞掉，而那正是「报成功却丢了数据」的来源
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
