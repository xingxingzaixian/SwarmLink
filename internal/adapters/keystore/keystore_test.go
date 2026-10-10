package keystore

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
)

// posixPerms 报告当前平台是否真正落实 POSIX 权限位。
//
// Windows 不落实：os.WriteFile(0600) 只映射到「只读」属性，os.MkdirAll(0700) 同理，
// 于是 os.Stat 返回 0666 / 0777。这是 Go 在 Windows 上的既定语义，不是本适配器的
// 缺陷 —— keystore.go 的包注释与《架构设计书》都把它记为平台限制（私钥在 Windows
// 上不保证对其他用户不可读，只能靠 ACL，而不是权限位）。
//
// 因此权限断言只在落实权限位的平台上成立。做成常量而不是两处各写一个 if：两处
// 用例用的是同一条判据，将来若真的接上 Windows ACL，只需改这一处。
const posixPerms = runtime.GOOS != "windows"

func TestLoadOrCreateIsStableAndPersists(t *testing.T) {
	dir := t.TempDir()

	// 第二参数 "" = 禁用托管：本用例钉住的是「没有托管时与历史版本行为一致」。
	kp1, err := LoadOrCreate(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	kp2, err := LoadOrCreate(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if kp1.KeyPair.NodeID() != kp2.KeyPair.NodeID() {
		t.Fatal("node id not stable across loads")
	}

	fi, err := os.Stat(KeyPath(dir))
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if posixPerms {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("want 0600 got %#o", perm)
		}
	} else if fi.Size() == 0 {
		// Windows 上断言不了权限位，但也不该让这条用例退化成空跑：
		// 至少钉住「私钥确实落了盘且非空」—— 这才是本用例的主题。
		t.Fatal("key file is empty")
	}
}

func TestLoadOrCreateCreatesDirWith0700(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir, ""); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Fatalf("identity 必须是目录，实际 mode = %v", fi.Mode())
	}
	if posixPerms {
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Fatalf("want 0700 got %#o", perm)
		}
	}
}

// 数据副本损坏、且没有可用托管副本 ⇒ 报错。这是本功能【不可动摇】的语义：
// 绝不静默换身份（换身份会让所有已有联系人认不出你）。
func TestLoadOrCreateRejectsCorruptKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/identity", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(KeyPath(dir), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir, ""); err == nil {
		t.Fatal("expected error for corrupt key")
	}
}

// 核心用例：数据副本缺失时，身份必须能从托管副本恢复成【同一个】node_id。
func TestLoadOrCreateRestoresFromVault(t *testing.T) {
	dataDir, vaultDir := t.TempDir(), t.TempDir()

	first, err := LoadOrCreate(dataDir, vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟「换数据目录 / 删掉数据目录」：只清数据副本，托管副本还在
	if err := os.Remove(KeyPath(dataDir)); err != nil {
		t.Fatal(err)
	}

	second, err := LoadOrCreate(dataDir, vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	if first.KeyPair.NodeID() != second.KeyPair.NodeID() {
		t.Fatalf("身份没恢复：%s -> %s", first.KeyPair.NodeID(), second.KeyPair.NodeID())
	}
	if !second.Restored {
		t.Fatal("Restored 必须为真，否则调用方无法提示用户")
	}
	if _, err := os.Stat(KeyPath(dataDir)); err != nil {
		t.Fatalf("数据副本必须被回填：%v", err)
	}
}

// 数据副本损坏 + 托管副本完好 ⇒ 从托管副本恢复，而不是报错（比现状更强）。
func TestLoadOrCreateRepairsCorruptDataCopy(t *testing.T) {
	dataDir, vaultDir := t.TempDir(), t.TempDir()
	if _, err := LoadOrCreate(dataDir, vaultDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(KeyPath(dataDir), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := LoadOrCreate(dataDir, vaultDir)
	if err != nil {
		t.Fatalf("有可用托管副本时不该报错：%v", err)
	}
	if res.Notice == "" {
		t.Fatal("恢复了损坏的数据副本，必须给出提示")
	}
}

// 数据副本损坏 + 没有托管副本 ⇒ 仍然报错（保持既有语义，绝不静默换身份）。
func TestLoadOrCreateCorruptWithoutVaultStillFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/identity", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(KeyPath(dir), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir, ""); err == nil {
		t.Fatal("期望报错")
	}
}

// 托管副本损坏 ⇒ 生成新身份并覆盖它（否则每次启动都刷告警），且必须提示。
func TestLoadOrCreateReplacesBrokenVault(t *testing.T) {
	dataDir, vaultDir := t.TempDir(), t.TempDir()
	if _, err := LoadOrCreate(dataDir, vaultDir); err != nil {
		t.Fatal(err)
	}
	v := newVault(vaultDir)
	if err := os.WriteFile(v.filePath(), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(KeyPath(dataDir)); err != nil {
		t.Fatal(err)
	}

	res, err := LoadOrCreate(dataDir, vaultDir)
	if err != nil {
		t.Fatalf("坏掉的托管副本不该阻塞启动：%v", err)
	}
	if res.Notice == "" {
		t.Fatal("换了新身份必须提示")
	}
	// 覆盖后再读一次必须能正常解开
	if _, err := v.load(); err != nil {
		t.Fatalf("坏掉的托管副本必须被覆盖：%v", err)
	}
}

// 数据副本与托管副本不同 ⇒ 以数据副本为准（权威副本），并让两份重新一致。
func TestLoadOrCreateDataCopyWins(t *testing.T) {
	dataDir, vaultDir := t.TempDir(), t.TempDir()
	first, err := LoadOrCreate(dataDir, vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	// 换一个数据目录生成第二个身份，再把它的密钥放回第一个数据目录：
	// 模拟「用户手工把备份的旧密钥恢复进数据目录」
	other := t.TempDir()
	second, err := LoadOrCreate(other, "")
	if err != nil {
		t.Fatal(err)
	}
	pem, err := identity.MarshalPrivateKeyPEM(second.KeyPair.PrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(KeyPath(dataDir), pem, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadOrCreate(dataDir, vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyPair.NodeID() != second.KeyPair.NodeID() {
		t.Fatal("必须使用数据副本里的身份")
	}
	if got.KeyPair.NodeID() == first.KeyPair.NodeID() {
		t.Fatal("前置条件不成立")
	}
	// 托管副本必须被同步成数据副本那一份
	res, err := LoadOrCreate(t.TempDir(), vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.KeyPair.NodeID() != second.KeyPair.NodeID() {
		t.Fatal("托管副本没有被数据副本覆盖")
	}
}

// 托管目录不可写 ⇒ 降级为现状行为，绝不阻塞启动。
func TestLoadOrCreateDegradesWhenVaultUnwritable(t *testing.T) {
	dataDir := t.TempDir()
	vaultDir := filepath.Join(t.TempDir(), "nope")
	// 用一个【文件】占住 vault 目录名，让 MkdirAll 必然失败
	if err := os.WriteFile(vaultDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := LoadOrCreate(dataDir, vaultDir)
	if err != nil {
		t.Fatalf("托管不可用不能影响启动：%v", err)
	}
	if res.KeyPair == nil {
		t.Fatal("必须仍然拿到身份")
	}
	if res.Notice == "" {
		t.Fatal("降级必须有提示")
	}
}

// 「读不到」不等于「损坏」：读失败时降级，且【绝不覆盖】那份暂时读不到的副本。
//
// 用【目录】占住副本路径来构造一次确定性的读失败 —— 既不返回 ErrNotExist（不是首次运行），
// 也不是解密失败（不是损坏）。若实现把它当损坏去「生成新身份并覆盖」，这个目录会被
// 换成普通文件，本用例就会红。
func TestLoadOrCreateDoesNotOverwriteUnreadableVault(t *testing.T) {
	dataDir, vaultDir := t.TempDir(), t.TempDir()
	v := newVault(vaultDir)
	if err := os.MkdirAll(filepath.Dir(v.filePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(v.filePath(), 0o700); err != nil {
		t.Fatal(err)
	}

	res, err := LoadOrCreate(dataDir, vaultDir)
	if err != nil {
		t.Fatalf("托管读不到不能影响启动：%v", err)
	}
	if res.KeyPair == nil {
		t.Fatal("必须仍然拿到身份")
	}
	if res.Notice == "" {
		t.Fatal("降级必须有提示")
	}

	fi, err := os.Stat(v.filePath())
	if err != nil {
		t.Fatalf("读不到的托管副本被删掉了：%v", err)
	}
	if !fi.IsDir() {
		t.Fatal("读不到的托管副本被当损坏覆盖了 —— 权限/IO 抖动不该换掉一个完好身份")
	}
}
