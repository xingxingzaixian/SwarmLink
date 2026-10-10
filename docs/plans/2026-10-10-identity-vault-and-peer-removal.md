# 身份托管（vault）与无效节点删除 实施计划

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 让「换数据目录 / 重装应用」不再换节点身份（身份托管副本），并让用户能把列表里的失效身份彻底删掉（本地移除 + 目录抑制 + 再次上线自动恢复）。

**Architecture:** A（vault）在 `adapters/keystore` 里加一层「机器级托管副本」：数据副本仍是权威，托管副本只在数据副本缺失/损坏时参与恢复；Windows 用 DPAPI 机器作用域包裹，其它平台明文。B（删除）复用 `peers.state` 里已存在的 `'blocked'` 作为「本地已移除」标记，把抑制做成仓储 `Upsert` 的**粘性不变量**、把过滤下沉到 `List()`，因此零迁移、零新表。

**Tech Stack:** Go 1.26、`golang.org/x/sys/windows`（DPAPI；已在依赖树，`go.mod:24`）、进程内事件总线、Wails3 beta.28、Vue 3 + Pinia + TypeScript。

**设计与依据：** `docs/plans/2026-10-10-identity-vault-and-peer-removal-design.md`

---

## 前置约定

- 所有命令在仓库根目录 `d:\code\OpenSource\swarm-link` 执行（前端命令先 `cd frontend`）。
- 提交信息沿用仓库风格：`feat(keystore): 中文描述`、`feat(app): …`、`docs: …`。**每个 Task 结束都提交**（分任务提交是回滚锚点）。
- **提交纪律（重要）**：所有 `git add` 都写**显式文件列表**，不要 `git add <目录>`。工作区里有不属于本功能的未跟踪文件：`scripts/build.bat`、`scripts/build_nsis.bat`、`swarmlink-1.png`、`swarmlink-2.png`。
- **既有失败基线：全绿。** 2026-10-10 实测 `go test -tags wails ./... -count=1` 共 24 个包全部 `ok`（`docs/plans/2026-10-08-folder-transfer.md` 记录的那 4 个既有失败已经修掉了）。**因此任何失败都是本次改动引入的**，不要放过。
- **验证必须跑全仓**：`PeerDirectory` 加一个方法会波及【所有】实现者，包括散落在测试文件里的替身（`internal/adapters/wails/services_test.go:146` 的 `emptyDir`、`:176` 的 `capDir`）。只跑「受影响的包」会得到 `go build` 通过而 `go test ./...` 全红。固定命令：

  ```
  go test -tags wails ./... -count=1
  ```

  开发中的快速反馈才用 `-run` 缩小范围。
- **架构红线与 gofmt 是 `go test` 的一部分**（`internal/archtest`）：R1（domain 零 I/O）、R2（适配器之间禁止互引）、R3（app/domain 不得 import 适配器）、`TestSourcesAreGofmtClean`（gofmt + LF 行尾）。新增 .go 文件必须 gofmt 干净。
  - 本计划刻意**不**让 `adapters/keystore` import `adapters/config`（会踩 R2）：vault 目录由 `bootstrap` 解析后作为参数传入。
- 前端：`npm run typecheck`（`vue-tsc --noEmit`）必须保持干净；测试用 `npm run test`（Node 内置 test runner，`node --test "tests/**/*.test.ts"`，**不是** vitest，**没有** jsdom/组件测试能力）。
- 绑定产物 `frontend/bindings/` 已入库；重新生成的命令（`-ts` 与 `-i` 不能省）：

  ```
  wails3 generate bindings -f "-tags wails" -clean=true -ts -i .
  ```

  生成后 `git status --short -- frontend/bindings` 应当只出现本次新增的方法。
- 测试里**不要新造构造脚手架**：沿用各文件既有写法（keystore 测试用 `t.TempDir()`；peers 相关用 `mem.NewPeers(fc)` / `sqlite_test.go` 顶部的建库辅助）。
- 任务顺序：Part A 与 Part B 相互独立，可任选先做。Part A 内部 A1→A2→A3→A4→A5 有依赖；Part B 内部 B1→B2→B3→B4→B5 有依赖。

---

# Part A：身份托管（vault）

## Task A1: `config.VaultDir()` —— 托管目录解析

**Files:**
- Modify: `internal/adapters/config/config.go`
- Test: `internal/adapters/config/config_test.go`

**Step 1: 写失败的测试**

在 `config_test.go` 里追加（沿用本文件既有的 `t.Setenv` + `os.UserConfigDir` 写法）：

```go
// 托管目录必须与数据目录【解耦】：它跟着环境变量走，不受数据目录影响。
func TestVaultDirEnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "vault")
	t.Setenv(EnvVaultDir, want)

	if got := VaultDir(t.TempDir()); got != want {
		t.Fatalf("VaultDir = %q，期望 %q", got, want)
	}
}

// 空白值不算设置：空的 SWARMLINK_VAULT_DIR 会变成「当前目录」，比不设更糟。
func TestVaultDirEnvOverrideIgnoresBlank(t *testing.T) {
	t.Setenv(EnvVaultDir, "   ")

	base := machineDataDir()
	if base == "" {
		t.Skip("本平台没有机器级目录基座")
	}
	if got, want := VaultDir(t.TempDir()), filepath.Join(base, AppName); got != want {
		t.Fatalf("VaultDir = %q，期望回到平台默认 %q", got, want)
	}
}

// 解析结果与数据目录重合时必须禁用：一旦相同，「删数据目录」就等于「删身份」。
func TestVaultDirRejectsDataDirOverlap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvVaultDir, dir)

	if got := VaultDir(dir); got != "" {
		t.Fatalf("与数据目录重合时必须返回空串（禁用），得到 %q", got)
	}
	// 尾部分隔符与大小写不该骗过守卫
	if got := VaultDir(dir + string(filepath.Separator)); got != "" {
		t.Fatalf("尾部斜杠应视为同一目录，得到 %q", got)
	}
}
```

**Step 2: 运行测试确认失败**

Run: `go test ./internal/adapters/config/ -run 'TestVaultDir' -count=1`
Expected: 编译失败 —— `undefined: VaultDir` / `undefined: EnvVaultDir` / `undefined: machineDataDir`

**Step 3: 实现**

在 `config.go` 里 `EnvDataDir` 附近追加常量，在文件末尾附近追加三个函数（`runtime` 若未导入则补进 import 块）：

```go
// EnvVaultDir 覆盖身份托管目录（vault）：值是「其下含 identity 子目录的那个目录」。
// 与 EnvDataDir 对称，且优先级最高 —— 测试与受限部署都需要一个不依赖机器的确定位置。
const EnvVaultDir = "SWARMLINK_VAULT_DIR"

// VaultDir 返回身份托管副本所在目录（与数据目录无关的机器级固定位置）。
//
// 返回 "" 表示本次运行【禁用】托管，调用方据此退回「只用数据目录」的现状行为：
//   - 便携模式（portable.mode 存在）：便携 = 自包含。否则把 U 盘插到别的机器上会
//     恢复原机器的身份，同时污染那台机器的托管目录；
//   - 解析结果与数据目录重合：一旦相同，「删数据目录」就等于「删身份」，必须拦住。
//
// 本函数只做路径解析，不创建目录、不做任何 I/O（与 Dir 的约定一致）。
func VaultDir(dataDir string) string {
	if d := strings.TrimSpace(os.Getenv(EnvVaultDir)); d != "" {
		if sameDir(d, dataDir) {
			return ""
		}
		return d
	}
	if _, ok := portableDir(); ok {
		return ""
	}
	base := machineDataDir()
	if base == "" {
		return ""
	}
	d := filepath.Join(base, AppName)
	if sameDir(d, dataDir) {
		return ""
	}
	return d
}

// machineDataDir 返回「与用户无关的机器级数据目录」基座。
// 各平台回退到此为止：拿不到就禁用托管（而不是猜一个可写但没意义的位置）。
func machineDataDir() string {
	switch runtime.GOOS {
	case "windows":
		if pd := strings.TrimSpace(os.Getenv("ProgramData")); pd != "" {
			return pd
		}
		if sd := strings.TrimSpace(os.Getenv("SystemDrive")); sd != "" {
			return filepath.Join(sd+string(filepath.Separator), "ProgramData")
		}
		return ""
	case "darwin":
		return "/Library/Application Support"
	default:
		return "/var/lib"
	}
}

// sameDir 判断两个路径是否指向同一目录。Windows 上大小写不敏感。
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}
```

**Step 4: 运行测试确认通过**

Run: `go test ./internal/adapters/config/ -count=1`
Expected: PASS（含既有用例）

**Step 5: 提交**

```
git add internal/adapters/config/config.go internal/adapters/config/config_test.go
git commit -m "feat(config): 新增身份托管目录解析（含便携与数据目录重合守卫）"
```

## Task A2: DPAPI 平台层

**Files:**
- Create: `internal/adapters/keystore/dpapi_windows.go`
- Create: `internal/adapters/keystore/dpapi_other.go`
- Test: `internal/adapters/keystore/dpapi_windows_test.go`

**Step 1: 写失败的测试**

`dpapi_windows_test.go`（文件名带 `_windows` ⇒ 只在 Windows 上编译）：

```go
//go:build windows

package keystore

import (
	"bytes"
	"testing"
)

func TestVaultRoundTripAndTamperDetection(t *testing.T) {
	plain := []byte("-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n")

	sealed, err := protectVault(plain)
	if err != nil {
		t.Fatalf("protect: %v", err)
	}
	// 机器级 DPAPI 必须真的加密：明文特征不能出现在密文里
	if bytes.Contains(sealed, []byte("PRIVATE KEY")) {
		t.Fatal("密文里仍能看到明文特征")
	}

	got, err := unprotectVault(sealed)
	if err != nil {
		t.Fatalf("unprotect: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("往返不一致：got %q", got)
	}

	// 篡改必须报错 —— 这是「vault 损坏」分支的判据，不能退化成静默返回垃圾
	bad := append([]byte(nil), sealed...)
	bad[len(bad)/2] ^= 0xFF
	if _, err := unprotectVault(bad); err == nil {
		t.Fatal("被篡改的 blob 居然解开了")
	}
}
```

**Step 2: 运行测试确认失败**

Run: `go test ./internal/adapters/keystore/ -run TestVaultRoundTrip -count=1`
Expected: 编译失败 —— `undefined: protectVault`

**Step 3: 实现**

`dpapi_windows.go`：

```go
//go:build windows

package keystore

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// vaultFileName 是托管副本在 <vaultDir>/identity/ 下的文件名。
// Windows 上它总是 DPAPI 密文，因此带 .dpapi 后缀，避免被误当成私钥明文备份。
const vaultFileName = "node.key.dpapi"

// vaultEntropy 是 DPAPI 的附加熵。它不是秘密（写死在二进制里），只是用途隔离：
// 同一台机器上别的应用拿不走、也换不到本应用的 blob。
var vaultEntropy = []byte("swarmlink-vault-v1")

// protectVault / unprotectVault 用 DPAPI 机器作用域（CRYPTPROTECT_LOCAL_MACHINE）包裹托管副本。
//
// 语义（见设计文档 D2）：本机任何用户都能解开 ⇒ 同机其他用户可继承本机身份，
// 这是「机器级托管」的既定取舍；真正被挡住的是「把 blob 拷到别的机器」。
//
// 注意 DPAPI 的输出内存由 LocalAlloc 分配，必须 LocalFree —— 漏掉就是每次启动泄漏一块。
func protectVault(pem []byte) ([]byte, error) {
	if len(pem) == 0 {
		return nil, fmt.Errorf("keystore: vault protect: empty payload")
	}
	in := windows.DataBlob{Size: uint32(len(pem)), Data: &pem[0]}
	entropy := windows.DataBlob{Size: uint32(len(vaultEntropy)), Data: &vaultEntropy[0]}

	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, &entropy, 0, nil, windows.CRYPTPROTECT_LOCAL_MACHINE, &out); err != nil {
		return nil, fmt.Errorf("keystore: dpapi protect: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))

	buf := make([]byte, out.Size)
	copy(buf, unsafe.Slice(out.Data, out.Size))
	return buf, nil
}

// unprotectVault 解开托管副本。blob 被篡改或来自别的机器/别的 DPAPI 主密钥时返回错误，
// 调用方据此走「托管副本损坏」分支（不能用返回值当密钥）。
func unprotectVault(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("keystore: vault unprotect: empty payload")
	}
	in := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	entropy := windows.DataBlob{Size: uint32(len(vaultEntropy)), Data: &vaultEntropy[0]}

	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, &entropy, 0, nil, windows.CRYPTPROTECT_LOCAL_MACHINE, &out); err != nil {
		return nil, fmt.Errorf("keystore: dpapi unprotect: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))

	buf := make([]byte, out.Size)
	copy(buf, unsafe.Slice(out.Data, out.Size))
	return buf, nil
}
```

`dpapi_other.go`：

```go
//go:build !windows

package keystore

// vaultFileName：非 Windows 没有机器级 DPAPI，托管副本就是明文 PEM
// （与数据副本同一保护等级），靠文件权限 + 「不在用户目录里」提供保护。
const vaultFileName = "node.key"

// protectVault / unprotectVault 在非 Windows 上是恒等变换。
// 保留这两个函数而不是在各处 if runtime.GOOS：调用点不需要知道平台差异。
func protectVault(pem []byte) ([]byte, error) { return pem, nil }

func unprotectVault(blob []byte) ([]byte, error) { return blob, nil }
```

**Step 4: 运行测试确认通过**

Run: `go test ./internal/adapters/keystore/ -run TestVaultRoundTrip -count=1 -v`
Expected: PASS（本机 2026-10-10 已用一次性探针验证过：`sealed 278B / 明文 60B`、往返一致、篡改返回 `The data is invalid.`）

同时确认 `go.mod` 把 x/sys 提升为直接依赖：

```
go mod tidy
git status --short -- go.mod go.sum
```

Expected: `go.mod` 里 `golang.org/x/sys` 从 indirect 段移到直接 require 段（版本不变、不新增模块）。

**Step 5: 提交**

```
git add internal/adapters/keystore/dpapi_windows.go internal/adapters/keystore/dpapi_other.go internal/adapters/keystore/dpapi_windows_test.go go.mod go.sum
git commit -m "feat(keystore): 托管副本的平台加密层（Windows DPAPI 机器作用域）"
```

## Task A3: vault 读写层

**Files:**
- Create: `internal/adapters/keystore/vault.go`
- Test: `internal/adapters/keystore/vault_test.go`

**Step 1: 写失败的测试**

```go
// 原子写：只留 .tmp 说明上一次写到一半被打断，正式文件必须仍是上一份完整内容。
func TestVaultSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	v := newVault(dir)

	if err := v.save([]byte("first")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := os.WriteFile(v.filePath()+".tmp", []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := v.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("残留 .tmp 污染了正式文件：got %q", got)
	}
}

// 禁用状态（vaultDir == ""）下所有操作都必须是无害的：这是便携模式与守卫命中时的路径。
func TestVaultDisabledIsInert(t *testing.T) {
	v := newVault("")

	if v.enabled() {
		t.Fatal("空目录必须表示禁用")
	}
	if err := v.save([]byte("x")); err != nil {
		t.Fatalf("禁用时 save 不应报错：%v", err)
	}
	if _, err := v.load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("禁用时 load 应返回 ErrNotExist，得到 %v", err)
	}
}

// 只读目录：save 必须报错（调用方据此降级 + 告警），load 的 ErrNotExist 必须可被 errors.Is 识别。
func TestVaultMissingFileIsErrNotExist(t *testing.T) {
	v := newVault(t.TempDir())
	if _, err := v.load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺失时必须返回 ErrNotExist，得到 %v", err)
	}
}
```

（`os` / `errors` 已在 `keystore_test.go` 里导入过，两个测试文件同包，注意别重复声明。）

**Step 2: 运行测试确认失败**

Run: `go test ./internal/adapters/keystore/ -run 'TestVault' -count=1`
Expected: 编译失败 —— `undefined: newVault`

**Step 3: 实现**

`vault.go`：

```go
package keystore

import (
	"fmt"
	"os"
	"path/filepath"
)

// vault 是身份的【机器级托管副本】。
//
// 它只负责定位、原子写、加锁无关的三件事：定位、原子写、编码委托给平台文件
// （dpapi_*.go），因此本文件跨平台可编译、可测试。dir 为空表示禁用
// （便携模式，或解析结果与数据目录重合 —— 见 config.VaultDir）。
//
// 为什么需要原子写：断电留下半截 blob 会被下次启动判成「损坏」，
// 而「损坏」的处置是「换新身份」—— 那正是本功能要消灭的东西。
type vault struct{ dir string }

func newVault(dir string) *vault { return &vault{dir: dir} }

func (v *vault) enabled() bool { return v != nil && v.dir != "" }

func (v *vault) filePath() string {
	return filepath.Join(v.dir, "identity", vaultFileName)
}

// load 读取并解开托管副本。文件不存在时返回可被 errors.Is(err, os.ErrNotExist) 识别的错误。
func (v *vault) load() ([]byte, error) {
	if !v.enabled() {
		return nil, os.ErrNotExist
	}
	blob, err := os.ReadFile(v.filePath())
	if err != nil {
		return nil, err
	}
	pem, err := unprotectVault(blob)
	if err != nil {
		return nil, fmt.Errorf("keystore: vault unprotect %s: %w", v.filePath(), err)
	}
	return pem, nil
}

// save 原子写入托管副本：写 .tmp → fsync → rename。
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
		return err
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

```

> 注意 `load` 的「不存在」语义必须是 `os.ErrNotExist`（调用方用 `errors.Is` 判断）——
> `restore` 靠它区分「没有托管副本」（首次运行）与「托管副本坏了」（要覆盖并告警）。

**Step 4: 运行测试确认通过**

Run: `go test ./internal/adapters/keystore/ -count=1`
Expected: PASS（含既有 3 个用例）

**Step 5: 提交**

```
git add internal/adapters/keystore/vault.go internal/adapters/keystore/vault_test.go
git commit -m "feat(keystore): 托管副本读写层（原子写 + 禁用语义）"
```

## Task A4: `keystore.LoadOrCreate` 编排

**Files:**
- Modify: `internal/adapters/keystore/keystore.go`
- Modify: `internal/adapters/keystore/keystore_test.go`（既有 3 个用例的调用点）
- Test: `internal/adapters/keystore/keystore_test.go`

**Step 1: 写失败的测试**

先改既有 3 个用例的调用（`LoadOrCreate(dir)` → `LoadOrCreate(dir, "")`），它们钉住的是「禁用托管时行为与现状一致」。然后追加：

```go
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
```

需要补 `"github.com/swarmlink/swarmlink/internal/domain/identity"` 到测试文件 import。

**Step 2: 运行测试确认失败**

Run: `go test ./internal/adapters/keystore/ -count=1`
Expected: 编译失败 —— `LoadOrCreate` 参数个数不匹配、`Result` 未定义

**Step 3: 实现**

`keystore.go` 改成（保留 `KeyPath` 与包注释不变）：

```go
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
		// 数据副本损坏 ⇒ 先试托管副本；两边都不可用就保持历史语义：报错，不静默换身份
		if res, ok, _ := v.restore(dataPath); ok {
			res.Notice = "身份文件损坏，已从机器级托管副本恢复：" + dataPath
			return res, nil
		}
		return Result{}, fmt.Errorf("keystore: %w", perr)

	case errors.Is(err, os.ErrNotExist):
		// 首次运行或换了数据目录：先问托管副本

	default:
		return Result{}, fmt.Errorf("keystore: read key: %w", err)
	}

	if res, ok, broken := v.restore(dataPath); ok {
		res.Notice = "已从机器级托管副本恢复节点身份：" + v.filePath()
		return res, nil
	} else if broken != nil {
		// 托管副本存在但解不开 ⇒ 生成新身份并覆盖它，否则每次启动都刷同一条告警
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
// 失败只影响「下次能否恢复」，因此由调用方忽略错误、只记日志。
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
//   - broken != nil  ：托管副本存在但不可用（损坏 / DPAPI 解不开 / 另有来源）
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
```

> `restore` 的 `broken` 与「没有托管副本」必须区分：前者要生成新身份并**覆盖**坏文件
> （否则每次启动都刷同一条告警），后者是正常的首次运行。这个区分靠 `os.ErrNotExist`，见 `vault.go`。

**Step 4: 运行测试确认通过**

Run: `go test ./internal/adapters/keystore/ -count=1`
Expected: PASS（既有 3 个 + 新增 7 个）

**Step 5: 提交**

```
git add internal/adapters/keystore/keystore.go internal/adapters/keystore/keystore_test.go
git commit -m "feat(keystore): 身份加载支持机器级托管副本（数据副本优先，绝不静默换身份）"
```

## Task A5: bootstrap 接线

**Files:**
- Modify: `internal/bootstrap/bootstrap.go:92-130`
- Modify: `test/harness/harness.go:260`
- Test: `internal/bootstrap`（若有既有测试）或 `test/e2e`（见 Step 1 说明）

**Step 1: 写失败的测试**

`bootstrap.Start` 依赖真实配置目录与网络，没有轻量单测脚手架。**不新造脚手架**，改在既有 e2e 里钉住端到端行为：在 `test/e2e` 里找既有的「启动节点」用例，追加

```go
// 换数据目录后身份必须保持不变（本计划的用户可见承诺）。
func TestIdentitySurvivesDataDirChange(t *testing.T) {
	// 若 test/e2e 已有「起两个节点互通」的辅助，直接沿用它：
	//   1) 以 dataDir=A、vaultDir=V 启动 → 记下 node_id
	//   2) 只清掉 A/identity/，再以 A、V 启动 → node_id 必须相同
	//   3) 断开后换 dataDir=B、同一个 V → node_id 仍然相同
	// 若 e2e 的启动辅助不支持注入 vaultDir，则本用例放到 Task A6 之后、
	// 由 test/harness 暴露 LoadOrCreateIdentity(dir, vaultDir) 后补（见 Step 3）。
}
```

> 这条用例的价值在于「产品承诺可执行」。如果你在 e2e 里发现启动辅助无法注入 `vaultDir`（很可能，因为它走 `SWARMLINK_DATA_DIR`），就**用环境变量**：`t.Setenv("SWARMLINK_VAULT_DIR", v)` + `t.Setenv("SWARMLINK_DATA_DIR", a)` —— 这条路径同时也验证了仓库自己的部署入口。

**Step 2: 运行测试确认失败**

Run: `go test ./test/e2e/ -run TestIdentitySurvivesDataDirChange -count=1 -v`
Expected: FAIL（当前换数据目录必然换身份）

**Step 3: 实现**

`bootstrap.go` 在数据目录解析之后接上托管目录：

```go
	dir := opts.DataDir
	if dir == "" {
		d, err := config.Dir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	// 身份托管目录与数据目录【无关】；返回空串表示禁用（便携模式 / 两者重合 / 平台无机器级位置）。
	// 为什么要单独解析而不是让 keystore 自己去问 config：adapters 之间禁止互引（R2）。
	vaultDir := config.VaultDir(dir)
```

把身份加载改成：

```go
	res, err := keystore.LoadOrCreate(dir, vaultDir)
	if err != nil {
		return nil, err
	}
	kp := res.KeyPair
	lg := newLogger(opts.Level, kp.NodeID().String())
	if res.Notice != "" {
		// 当前只有日志这一个呈现面（设计文档「非目标」里排除了启动通知 UI）。
		// 这条日志是「用户突然换了身份」时唯一能查到原因的地方，措辞要能自解释。
		lg.Warn("identity notice", "notice", res.Notice, "restored", res.Restored)
	}
```

`test/harness/harness.go:260` 同步改签名：

```go
// LoadOrCreateIdentity 暴露 keystore，便于需要持久身份的测试。
func LoadOrCreateIdentity(dir, vaultDir string) (*identity.KeyPair, error) {
	res, err := keystore.LoadOrCreate(dir, vaultDir)
	if err != nil {
		return nil, err
	}
	return res.KeyPair, nil
}
```

**Step 4: 运行测试确认通过**

Run: `go test -tags wails ./... -count=1`
Expected: 全部 PASS（注意 Task A4 改了 `LoadOrCreate` 的签名，全仓任何遗漏的调用点都会在这里暴露）

**Step 5: 提交**

```
git add internal/bootstrap/bootstrap.go test/harness/harness.go test/e2e/<改动的测试文件>
git commit -m "feat(bootstrap): 接入身份托管目录，并把身份恢复/降级写进日志"
```

## Task A6: 文档对齐（含三处与实现不一致的修正）

**Files:**
- Modify: `README.md:233-238`
- Modify: `设计书.md:158-170`
- Create: `docs/adr/ADR-012-identity-vault.md`
- Modify: `docs/adr/README.md:18`
- Modify: `docs/plans/2026-10-10-identity-vault-and-peer-removal-design.md`（三处修正）

**Step 1: 修正设计文档里与实现不一致的三处**

1. **DACL 收紧（§4.6）**：删掉「DACL 仍尽力收紧到 当前用户 + Administrators + SYSTEM」。
   理由写清楚：收紧到只有当前用户可读，同机其他用户就读不到托管副本，也就无法「继承身份」——
   与 D1/D2 的机器级共享语义**互相矛盾**。因此**不额外收紧**，只做 `MkdirAll(0700)`（Windows 忽略权限位，Posix 生效）。
   并把 DPAPI 的价值收敛为一句事实：**它挡住的是「blob 被拷到别的机器」**。
2. **并发锁（§4.4）**：删掉 `LockFileEx` / `flock` 那段。理由：原子写（tmp+fsync+rename）已经保证不会写出半个文件；
   锁只能串行化，却解决不了真正的问题 —— 两个数据目录并存时托管副本会来回抖动（后启动者胜）。
   把这条挪到 §9「已知代价」：**同一台机器上长期并用两个数据目录，托管副本会在两者之间抖动**；
   若真要根治，需要给托管副本记「来源数据目录」并拒绝跨目录覆盖（当前不做）。
3. **Linux 路径**：`/var/lib/swarmlink` → `/var/lib/SwarmLink`（实现是 `filepath.Join(base, AppName)`，与 Windows/macOS 一致）。

**Step 2: 改 README**

`README.md:233-238` 的第 1 条副作用改写为：

```markdown
> 1. **身份不再跟着数据目录跑**。`identity/node.key` 是权威副本，另有一份机器级托管副本
>    （Windows `%ProgramData%\SwarmLink\identity\`，可用 `SWARMLINK_VAULT_DIR` 覆盖；
>    便携模式下不启用）。删掉/换掉数据目录、重装应用之后，启动时会自动从托管副本恢复同一身份。
>    两条要知道的边界：**换机器 = 换身份**（托管是机器级的，且 Windows 上 blob 被 DPAPI 绑在本机）；
>    **同一台机器上的其他用户也能继承这个身份**（机器级托管的既定语义）。
```

**Step 3: 改设计书与 ADR**

- `设计书.md:158-170` 的「存储位置」段落补一段 vault 说明（位置优先级、Windows 用 DPAPI 机器作用域、数据副本优先、托管不可用时降级）。
- 新建 `docs/adr/ADR-012-identity-vault.md`，沿用既有 ADR 的结构（决策 / 背景 / 后果 / 实现注记 / 验证），验证一栏指向
  `keystore.TestLoadOrCreateRestoresFromVault`、`TestLoadOrCreateRepairsCorruptDataCopy`、`TestLoadOrCreateDataCopyWins`。
  必须写进去的两条「代价」：机器级共享身份、DPAPI 只防跨机拷贝。
- `docs/adr/README.md:18` 的索引表补一行 ADR-012。

**Step 4: 验证**

Run: `go test -tags wails ./... -count=1`
Expected: 全部 PASS（文档改动不影响测试，这一步是为了确认前面几个 Task 没有留下半成品）

**Step 5: 提交**

```
git add README.md 设计书.md docs/adr/ADR-012-identity-vault.md docs/adr/README.md docs/plans/2026-10-10-identity-vault-and-peer-removal-design.md
git commit -m "docs(identity): 补身份托管的位置与边界，并修正设计文档三处与实现不一致"
```

---

# Part B：无效节点删除

## Task B1: `ports.SetSuppressed` 与三个实现

**Files:**
- Modify: `internal/domain/ports/ports.go:65-73`（`PeerDirectory`）
- Modify: `internal/adapters/store/sqlite/peers.go`
- Modify: `internal/adapters/store/mem/mem.go`
- Modify: `internal/adapters/wails/services_test.go:146,176`（两个测试桩）
- Test: `internal/adapters/store/sqlite/sqlite_test.go`、`internal/adapters/store/mem/mem_test.go`

**Step 1: 写失败的测试**

`sqlite_test.go`（沿用本文件既有的建库辅助与 `mkID`）：

```go
// 本地移除标记必须落库：它是「删得掉」的唯一依据（设计文档 §5.2）。
func TestSetSuppressedRoundTrip(t *testing.T) {
	db := newTestDB(t) // 沿用本文件既有构造
	p := NewPeers(db, clock.New())

	id := mkID(7)
	if err := p.Upsert(peer.Peer{NodeID: id, DisplayName: "ghost", State: peer.StateOffline}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetSuppressed(id, true, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	got, ok := p.Get(id)
	if !ok || got.State != peer.StateBlocked {
		t.Fatalf("期望 blocked，得到 %+v", got)
	}
	if err := p.SetSuppressed(id, false, time.Unix(2000, 0)); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Get(id); got.State == peer.StateBlocked {
		t.Fatal("解除后不该还是 blocked")
	}
}

// 对不存在的行做标记必须是无害的空操作（幂等）。
func TestSetSuppressedOnMissingRowIsNoop(t *testing.T) {
	db := newTestDB(t)
	p := NewPeers(db, clock.New())
	if err := p.SetSuppressed(mkID(9), true, time.Unix(1, 0)); err != nil {
		t.Fatalf("不该报错：%v", err)
	}
}
```

`mem_test.go` 追加同名语义的用例（沿用本文件既有 `mem.NewPeers(fc)` 与 `mkID` 写法）。

**Step 2: 运行测试确认失败**

Run: `go test ./internal/adapters/store/... -run TestSetSuppressed -count=1`
Expected: 编译失败 —— `p.SetSuppressed undefined`

**Step 3: 实现**

`ports.go` 在 `MarkOffline` 旁边加（注意 `time` 已在该文件导入）：

```go
	// SetSuppressed 设置/清除「本地已移除」标记。
	//
	// suppressed=true：该节点默认不再出现在 List、也不再被目录交换传播，
	// 直到它再次被验证到（有效签名通告或握手成功 —— 见 app.PeerApp）。
	//
	// 语义与全部约束（包括为什么每一条写入路径都不能覆盖这个标记）见
	// docs/plans/2026-10-10-identity-vault-and-peer-removal-design.md §5。
	SetSuppressed(id identity.NodeID, suppressed bool, at time.Time) error
```

`sqlite/peers.go`（放在 `MarkOffline` 之后）：

```go
// SetSuppressed 设置/清除「本地已移除」标记：复用 state='blocked'（设计文档 §5.2）。
//
// 只更新已存在的行：删除的前提是「它出现在我的列表里」；没有行时什么都不做（幂等）。
// at 记进 last_seen，让「什么时候删的」在诊断里可见。
func (p *Peers) SetSuppressed(id identity.NodeID, suppressed bool, at time.Time) error {
	state := peer.StateDiscovered
	if suppressed {
		state = peer.StateBlocked
	}
	return p.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE peers SET state = ?, last_seen = ? WHERE node_id = ?`,
			string(state), msOf(at), id.String(),
		)
		return err
	})
}
```

`mem/mem.go`（放在 `MarkOffline` 之后）：

```go
// SetSuppressed 设置/清除「本地已移除」标记（语义见 ports.PeerDirectory）。
func (s *Peers) SetSuppressed(id identity.NodeID, suppressed bool, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[id]
	if !ok {
		return nil
	}
	if suppressed {
		p.State = peer.StateBlocked
	} else if p.State == peer.StateBlocked {
		p.State = peer.StateDiscovered
	}
	p.LastSeen = at
	s.items[id] = p
	return nil
}
```

两个测试桩（`wails/services_test.go:146`、`:176`）各加一行：

```go
func (emptyDir) SetSuppressed(identity.NodeID, bool, time.Time) error { return nil }
func (d capDir) SetSuppressed(identity.NodeID, bool, time.Time) error { return nil }
```

**Step 4: 运行测试确认通过**

Run: `go test -tags wails ./... -count=1`
Expected: 全部 PASS。**这一步必须跑全仓** —— 少改一个测试桩，`wails` 包会编译失败。

**Step 5: 提交**

```
git add internal/domain/ports/ports.go internal/adapters/store/sqlite/peers.go internal/adapters/store/mem/mem.go internal/adapters/store/sqlite/sqlite_test.go internal/adapters/store/mem/mem_test.go internal/adapters/wails/services_test.go
git commit -m "feat(peers): 目录新增「本地已移除」标记与两个仓储实现"
```

## Task B2: 粘性不变量与读取过滤

**Files:**
- Modify: `internal/adapters/store/sqlite/peers.go:32-56`（`Upsert`）、`:112-121`（`MarkOffline`）、`:69-110`（`List`）
- Modify: `internal/adapters/store/mem/mem.go:479-493`（`Upsert`）、`:528-543`（`MarkOffline`）、`:504-517`（`List`）
- Modify: `internal/domain/ports/ports.go:57-63`（`PeerFilter`）
- Modify: `internal/app/discovery_app.go:229-255`（`entries()` 里那行冗余过滤）
- Test: `internal/adapters/store/sqlite/sqlite_test.go`、`internal/adapters/store/mem/mem_test.go`

**Step 1: 写失败的测试**

```go
// 粘性：被移除的节点不能被任何写入路径「复活」——这是删除能删干净的前提。
func TestBlockedStateIsStickyAcrossUpsert(t *testing.T) {
	db := newTestDB(t)
	p := NewPeers(db, clock.New())

	id := mkID(3)
	if err := p.Upsert(peer.Peer{NodeID: id, State: peer.StateOffline}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetSuppressed(id, true, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}

	// 目录交换会把条目 Upsert 成 discovered；这一发不能生效
	if err := p.Upsert(peer.Peer{NodeID: id, DisplayName: "ghost", State: peer.StateDiscovered}); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Get(id); got.State != peer.StateBlocked {
		t.Fatalf("Upsert 覆盖了移除标记：%+v", got)
	}
	// 但名字这类信息允许被更新（诊断仍要能看出是谁）
	if got, _ := p.Get(id); got.DisplayName != "ghost" {
		t.Fatalf("非状态字段应当照常更新：%+v", got)
	}

	// 离线事件也不能把它变回可见
	if err := p.MarkOffline(id, time.Unix(1200, 0)); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Get(id); got.State != peer.StateBlocked {
		t.Fatalf("MarkOffline 覆盖了移除标记：%+v", got)
	}
}

// 默认 List 不返回被移除的节点；显式 IncludeBlocked 或显式 State 查询仍能看到（诊断用）。
func TestListExcludesBlockedByDefault(t *testing.T) {
	db := newTestDB(t)
	p := NewPeers(db, clock.New())

	kept, gone := mkID(4), mkID(5)
	for _, id := range []identity.NodeID{kept, gone} {
		if err := p.Upsert(peer.Peer{NodeID: id, State: peer.StateOffline}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.SetSuppressed(gone, true, time.Now()); err != nil {
		t.Fatal(err)
	}

	all := p.List(ports.PeerFilter{})
	if len(all) != 1 || all[0].NodeID != kept {
		t.Fatalf("默认 List 应只剩 1 条，得到 %+v", all)
	}
	if with := p.List(ports.PeerFilter{IncludeBlocked: true}); len(with) != 2 {
		t.Fatalf("IncludeBlocked 应返回 2 条，得到 %d", len(with))
	}
	if only := p.List(ports.PeerFilter{State: peer.StateBlocked}); len(only) != 1 {
		t.Fatalf("显式按 blocked 查询应返回 1 条，得到 %d", len(only))
	}
}
```

`mem_test.go` 与 sqlite 同样的两条用例（两个实现的行为必须一致）。

**Step 2: 运行测试确认失败**

Run: `go test ./internal/adapters/store/... -run 'TestBlockedStateIsSticky|TestListExcludesBlocked' -count=1`
Expected: FAIL —— 被 Upsert 覆盖成 discovered / 默认 List 返回 2 条

**Step 3: 实现**

1. `ports.PeerFilter` 加字段：

```go
// PeerFilter 是目录查询过滤条件。
type PeerFilter struct {
	State   peer.State
	Source  string
	// OnlineOnly 只返回 online/discovered。
	OnlineOnly bool
	// IncludeBlocked 让「被本地移除」的条目也出现在结果里（诊断用；默认不返回）。
	// 注意：State 显式指定为 blocked 时，本字段无意义 —— 显式查询优先。
	IncludeBlocked bool
	Limit          int
}
```

2. `sqlite/peers.go` 的 `Upsert`：把 `state        = excluded.state,` 一行换成

```sql
  state        = CASE WHEN peers.state = 'blocked' THEN peers.state ELSE excluded.state END,
```

  并在 `Upsert` 的注释里补一句：**`blocked` 是粘性状态，任何写入都不得覆盖它**（否则每新增一条写入路径都是一次复活风险）。

3. `sqlite/peers.go` 的 `MarkOffline`：加守卫，让离线事件也不能把被移除的条目变回可见：

```go
		_, err := tx.Exec(
			`UPDATE peers SET state = 'offline', last_seen = ?
			  WHERE node_id = ? AND state <> 'blocked'`,
			msOf(at), id.String(),
		)
```

4. `sqlite/peers.go` 的 `List`：在 `State` 过滤之后加

```go
	if filter.State != "" {
		where = append(where, "state = ?")
		args = append(args, string(filter.State))
	} else if !filter.IncludeBlocked {
		where = append(where, "state <> 'blocked'")
	}
```

5. `mem/mem.go` 三处同步：
   - `Upsert`：`if old, ok := s.items[p.NodeID]; ok && old.State == peer.StateBlocked { p.State = peer.StateBlocked }`
   - `MarkOffline`：若当前是 `blocked` 则**直接提前返回**（连 `LastSeen` 也不动）——
     必须与 sqlite 的 `AND state <> 'blocked'` 一致；只更新 `LastSeen` 会引入一条新的跨实现分歧
     （这是实现阶段修正计划文本的一处，原计划写的是"只更新 LastSeen"）
   - `List`：`if filter.State != "" {...} else if !filter.IncludeBlocked && p.State == peer.StateBlocked { continue }`

6. `discovery_app.go` 的 `entries()` 删掉那行 `if p.State == peer.StateBlocked { continue }`，
   换成一句注释：「List 默认已排除 blocked（ports.PeerFilter），此处不再重复过滤」。
   （重复过滤会让「过滤规则」出现两处真相；由 §7.2 的用例钉住这条不变量。）

**Step 4: 运行测试确认通过**

Run: `go test -tags wails ./... -count=1`
Expected: 全部 PASS

**Step 5: 提交**

```
git add internal/domain/ports/ports.go internal/adapters/store/sqlite/peers.go internal/adapters/store/mem/mem.go internal/adapters/store/sqlite/sqlite_test.go internal/adapters/store/mem/mem_test.go internal/app/discovery_app.go
git commit -m "feat(peers): 移除标记做成写入侧粘性不变量，并让 List 默认排除"
```

## Task B3: 两个显式恢复点

**Files:**
- Modify: `internal/app/peer_app.go:59-135`（`OnAnnouncement`）、`:143-167`（`TopicPeerOnline`）
- Test: `internal/app/peer_app_test.go`、`internal/app/discovery_app_test.go`

**Step 1: 写失败的测试**

```go
// 被移除的节点再次发来【有效签名通告】⇒ 自动恢复（设计 D7）。
//
// 这条规则与「目录交换不恢复」配对存在：携带该身份的有效签名 = 它没死；
// 第三方转述的目录条目不具备这个性质。
func TestAnnouncementRestoresSuppressedPeer(t *testing.T) {
	id := mustNodeID(t, testPeerHex)
	dir := mem.NewPeers(clock.New())
	if err := dir.Upsert(peer.Peer{NodeID: id, State: peer.StateBlocked}); err != nil {
		t.Fatal(err)
	}
	a := NewPeerApp(identity.NodeID{}, dir, eventbus.New(), clock.New(), time.Minute, nil)

	if err := a.OnAnnouncement(peer.Announcement{NodeID: id, DisplayName: "back"}); err != nil {
		t.Fatal(err)
	}
	got, _ := dir.Get(id)
	if got.State == peer.StateBlocked {
		t.Fatal("有效通告必须解除移除标记")
	}
}

// 握手成功（更强的在世证明）同样恢复。
func TestConnectionRestoresSuppressedPeer(t *testing.T) {
	// 沿用本文件既有的 bus + PeerApp 构造，发 TopicPeerOnline 事件，
	// 断言 state 不再是 blocked（应当是 online）。
}

// 目录交换【不】恢复：它没有签名，任何人都能转述任意 node_id。
func TestPeerListRespDoesNotRestoreSuppressedPeer(t *testing.T) {
	// 沿用 discovery_app_test.go 既有构造，造一条含被移除 node_id 的 PeerListResp，
	// 断言该行仍是 blocked（且名字等非状态字段可以更新）。
}
```

**Step 2: 运行测试确认失败**

Run: `go test ./internal/app/ -run 'Suppressed' -count=1`
Expected: FAIL —— 通告后仍是 blocked

**Step 3: 实现**

`OnAnnouncement` 在 `now := a.clk.Now()` 之后、`if an.Leaving` 之前插入：

```go
	// 本地已移除的节点再次被【有效签名通告】验证到 ⇒ 它没死，自动恢复（设计 D7）。
	//
	// 能走到这里就说明 UDP 层已经验签，并校验了 node_id == Fingerprint(pub)
	// （udp/broadcast.go 的 decodeAnnounce / udp/seed.go）。
	// 携带该 node_id 的第三方转述（目录交换）不具备这个性质 ——
	// HandlePeerListResp 那条路径必须继续抑制，不要在这里找对称性。
	if prev, ok := a.dir.Get(an.NodeID); ok && prev.State == peer.StateBlocked {
		if err := a.dir.SetSuppressed(an.NodeID, false, now); err != nil {
			return err
		}
		if a.lg != nil {
			a.lg.Info("peer un-suppressed by signed announce", "peer_id", an.NodeID.String())
		}
	}
```

`TopicPeerOnline` 的处理里，`p, exists := a.dir.Get(id)` 之后插入：

```go
		// 三段 Ed25519 握手已通过 ⇒ 该身份确实在世，清除移除标记（设计 D7）。
		if exists && p.State == peer.StateBlocked {
			if err := a.dir.SetSuppressed(id, false, a.clk.Now()); err != nil {
				return
			}
		}
```

**Step 4: 运行测试确认通过**

Run: `go test ./internal/app/ -count=1`，然后再跑一次全仓
Expected: 全部 PASS

**Step 5: 提交**

```
git add internal/app/peer_app.go internal/app/peer_app_test.go internal/app/discovery_app_test.go
git commit -m "feat(peers): 有效通告与握手成功自动解除本地移除"
```

## Task B4: `PeerService.Remove`

**Files:**
- Modify: `internal/adapters/wails/services.go`（`PeerService`，约 `:834-863`）
- Test: `internal/adapters/wails/services_test.go`

**Step 1: 写失败的测试**

```go
// Remove 必须落到目录上；非法 nodeID 必须报错而不是静默成功。
func TestPeerServiceRemove(t *testing.T) {
	// 沿用本文件既有 Deps 构造（capDir 需提供真实实现，见下）
}
```

> `services_test.go` 里的 `capDir` 是一个「记录调用的能力桩」。本用例应把它扩成能记录 `SetSuppressed` 的调用，
> 断言 `Remove("…合法 id…")` 调用了 `SetSuppressed(id, true, …)`，并且 `Remove("不是 hex")` 返回错误。
> 不要为它引入 sqlite 依赖 —— 该包刻意只依赖 app/domain/config。

**Step 2: 运行测试确认失败**

Run: `go test ./internal/adapters/wails/ -run TestPeerServiceRemove -count=1`
Expected: 编译失败 —— `s.d.PeerDir.Remove undefined`（或被删方法不存在）

**Step 3: 实现**

`services.go` 的 `PeerService` 里追加（该文件的 import 已含 `identity` 与 `time`）：

```go
// Remove 从本机列表里移除一个节点（设计文档 §5）。
//
// 三条必须写进注释的边界（会直接影响用户的预期）：
//   - 只在本机生效：不发任何网络通告，对方与别人的列表都不受影响；
//   - 不拒绝握手：这不是拉黑，将来做真拉黑要另加一层语义；
//   - 并非永久：该节点若再次被验证到（有效签名通告或握手成功）会自动回到列表。
//
// 幂等：列表里没有这个 node_id 时返回 nil，而不是报错 —— 用户的意图（「列表里别再有它」）
// 已经满足了，报错只会让 UI 弹一个没有意义的失败。
func (s *PeerService) Remove(nodeID string) error {
	id, err := identity.ParseNodeID(nodeID)
	if err != nil {
		return fmt.Errorf("peer: invalid node id %q: %w", nodeID, err)
	}
	if _, ok := s.d.PeerDir.Get(id); !ok {
		return nil
	}
	return s.d.PeerDir.SetSuppressed(id, true, time.Now())
}
```

**Step 4: 跑测试 + 重新生成绑定**

```
go test ./internal/adapters/wails/ -count=1
wails3 generate bindings -f "-tags wails" -clean=true -ts -i .
git status --short -- frontend/bindings
```

Expected: 测试 PASS；`git status` 里只出现 `PeerService` 的 `Remove` 相关新增（不应有其它方法被改动）。

**Step 5: 提交**

```
git add internal/adapters/wails/services.go internal/adapters/wails/services_test.go frontend/bindings
git commit -m "feat(peer): 新增 PeerService.Remove（本地移除节点）并重新生成绑定"
```

## Task B5: 前端删除入口

**Files:**
- Create: `frontend/src/utils/peers.ts`
- Test: `frontend/tests/peers.test.ts`
- Modify: `frontend/src/api/types.ts`（`Api` 接口 + 若需要 `EV`）
- Modify: `frontend/src/api/bindings.ts:118-120`
- Modify: `frontend/src/api/mock.ts`（`peerList` 附近）
- Modify: `frontend/src/stores/peers.ts`
- Modify: `frontend/src/components/ContactPanel.vue:225-272`

> **不做新事件**。前端已有 3 秒兜底轮询（`App.vue:54`），加上本地立即移除，用户感知是「点完就消失」；
> 为它新增事件主题 + `Deps` 字段 + 事件桥接线属于 YAGNI。若将来发现轮询不可靠再补。

**Step 1: 写失败的测试**

`frontend/tests/peers.test.ts`（沿用 `format.test.ts` 等的 `node:test` + `node:assert/strict` 写法）：

```ts
import test from 'node:test'
import assert from 'node:assert/strict'

import { withoutPeer } from '../src/utils/peers.ts'

test('withoutPeer 只移除目标节点，其余顺序不变', () => {
  const list = [{ nodeId: 'a' }, { nodeId: 'b' }, { nodeId: 'c' }]
  assert.deepEqual(
    withoutPeer(list, 'b').map((p) => p.nodeId),
    ['a', 'c']
  )
})

test('withoutPeer 对不存在的节点是原样返回', () => {
  const list = [{ nodeId: 'a' }]
  assert.deepEqual(withoutPeer(list, 'zz'), [{ nodeId: 'a' }])
  assert.equal(withoutPeer(list, 'a').length, 0)
})
```

**Step 2: 运行测试确认失败**

Run: `cd frontend && npm run test`
Expected: FAIL —— 无法解析 `../src/utils/peers.ts`

**Step 3: 实现**

`frontend/src/utils/peers.ts`：

```ts
/**
 * 返回去掉指定节点的列表副本。
 *
 * 抽成纯函数是为了能测：本仓前端没有 jsdom / 组件测试能力（`npm run test` 是
 * node --test，只跑纯逻辑），把「移除后列表长什么样」这条规则留在 store 里就等于没测。
 */
export function withoutPeer<T extends { nodeId: string }>(list: T[], nodeId: string): T[] {
  return list.filter((p) => p.nodeId !== nodeId)
}
```

`frontend/src/api/types.ts`：`Api` 接口的「节点与诊断」段加

```ts
  /** 从本机列表移除一个节点（只在本机生效；该节点再次上线会自动回来）。 */
  removePeer(nodeId: string): Promise<void>
```

`frontend/src/api/bindings.ts`：`peerList` 之后加

```ts
  removePeer: async (nodeId) => PeerService.Remove(nodeId),
```

`frontend/src/api/mock.ts`：在 mock 的 `peerList` 附近加

```ts
    async removePeer(nodeId) {
      const i = peers.findIndex((p) => p.nodeId === nodeId)
      if (i >= 0) peers.splice(i, 1)
    },
```

> mock 体里 `peers` 是模块级数组（`mkPeer` 造的那批）。若它在别处被复制过，改完后要确认
> `peerList()` 返回的不再包含该节点 —— mock 是本仓「无后端也能跑 UI」的入口，行为要与真实实现一致。

`frontend/src/stores/peers.ts`：加一个动作（沿用本文件既有的「先调 API、再改本地状态」写法）

```ts
  /** 从本机列表移除一个节点。失败时把错误抛给调用方，由 UI 呈现。 */
  async function remove(nodeId: string): Promise<void> {
    const api = await getApi()
    await api.removePeer(nodeId)
    peers.value = withoutPeer(peers.value, nodeId)
  }
```

并在 `return { ... }` 里导出 `remove`（同时补 import）。

`frontend/src/components/ContactPanel.vue`：两张列表（在线 `:225-250`、离线 `:256-271`）的 `.row-meta` 里加一个删除按钮，
点击时 `@click.stop`（不触发 `openPeer`），并用**已经导入**的 `Modal.vue` 做二次确认：

```vue
<span class="row-time">{{ when(peerConvId(p), p.lastSeen) }}</span>
<button class="row-del" title="从列表移除" @click.stop="askRemove(p)">×</button>
```

```ts
const removing = ref<Peer | null>(null)

function askRemove(p: Peer): void {
  removing.value = p
}

async function confirmRemove(): Promise<void> {
  const p = removing.value
  if (!p) return
  try {
    await peers.remove(p.nodeId)
    removing.value = null
  } catch (e: any) {
    removeError.value = String(e?.message ?? e)
  }
}
```

确认弹窗文案必须把边界说清楚（否则用户会以为「删了就彻底断了」）：

```vue
<Modal v-if="removing" title="从列表移除节点" @close="removing = null">
  <p>将把「{{ removing?.displayName }}」从本机列表中移除。</p>
  <p class="faint small">
    只影响本机：对方不会收到任何通知，别人的列表也不变。该节点若再次上线，会自动回到列表。
  </p>
  <p v-if="removeError" class="warn">{{ removeError }}</p>
  <div class="modal-actions">
    <button class="btn btn-soft" @click="removing = null">取消</button>
    <button class="btn" @click="confirmRemove">移除</button>
  </div>
</Modal>
```

> 样式（`.row-del`）沿用本文件既有的按钮变量与 hover 规则，不要新造一套配色。

**Step 4: 验证**

```
cd frontend
npm run test
npm run typecheck
```

Expected: 测试 PASS；typecheck 干净（`Api` 接口新增方法后，`bindings.ts` 与 `mock.ts` 两处实现都必须补上，否则 typecheck 会失败 —— 这正是它该有的作用）。

**Step 5: 提交**

```
git add frontend/src/utils/peers.ts frontend/tests/peers.test.ts frontend/src/api/types.ts frontend/src/api/bindings.ts frontend/src/api/mock.ts frontend/src/stores/peers.ts frontend/src/components/ContactPanel.vue
git commit -m "feat(ui): 联系人列表支持移除节点（含边界说明与二次确认）"
```

## Task B6: 文档（README）

**Files:**
- Modify: `README.md`（节点列表/发现那一段）

**Step 1: 补一段说明**

写清楚三件事，位置放在讲「节点目录」的地方：

- 删除**只在本机生效**：p2p 没有权威，别人的列表不会因为你的操作而变化；
- 删除**不是拉黑**：不拒绝握手、不阻止对方看见你，将来若需要真的拉黑会另加一层；
- 删除**会被自动撤销**：该节点再次上线（有效签名通告或握手成功）就回到列表 ——
  所以它适合清理「已经死掉的旧身份」，不适合屏蔽一个活跃节点。

**Step 2: 验证**

Run: `go test -tags wails ./... -count=1`
Expected: 全部 PASS

**Step 3: 提交**

```
git add README.md
git commit -m "docs(readme): 说明移除节点只在本机生效且会在对方上线后自动恢复"
```

---

## 收尾检查

1. `go test -tags wails ./... -count=1` 全绿；`cd frontend && npm run typecheck && npm run test` 干净。
2. 手工验收（两个节点）
   - **vault**：启动一次 → 记下 `node_id`（设置页/诊断）→ 退出 → 删掉 `<数据目录>\identity\` → 再启动 → `node_id` 不变，日志出现 `identity notice`。
   - **vault 降级**：设 `SWARMLINK_VAULT_DIR` 指向一个不可写路径 → 仍能启动，日志有告警。
   - **删除**：A 的列表里删掉 B → B 从 A 的列表消失；B 继续运行并广播 → B 自动回到 A 的列表。
   - **删除的抑制**：A 删掉一个**已死**的旧身份 → 重启 A、等待目录同步（B 的目录里仍有那条）→ 它不应回来。
3. 设计文档与实现一致：`docs/plans/2026-10-10-identity-vault-and-peer-removal-design.md` 的 §4.4（锁）、§4.6（DACL）、§9（新增「两个数据目录并存会抖动」）已在 Task A6 修好。

## 已知代价与风险（实现了也依然存在）

- 同机其他用户可继承身份（机器级 + DPAPI 机器作用域，已在设计文档 D2 记录）。
- 同一台机器上长期并用两个数据目录：托管副本会在两者之间来回抖动（后启动者胜），当前不做来源记账。
- 活跃节点删不掉（会自动恢复）——这是设计 D7 的既定行为，不是 bug。
- 删除某节点后，指向它的等待中的传输任务仍会重试到退避上限（`app/transfer_waiting.go`）。

### B2 评审期间记录的一条窗口期缺口（随 B3 关闭）

`tcp.Manager.publishOnline`（`adapters/net/tcp/manager.go`）与 `remove` 不查目录：
被本地移除的节点只要主动拨入本机（握手不拒绝，见 D6），就会发出 `TopicPeerOnline` →
前端把它以 online 状态推回列表。**B2 与 B3 之间**这条路径可达；B3 落地后它由恢复触发点
覆盖（握手成功 = 该身份确实在世 ⇒ 解除标记并发布 online，此时「回到列表」是设计行为）。
不为此在 tcp 适配器里加目录依赖：那会让适配器反向依赖业务语义（R2 的意图）。

### B2 评审期间记录的三条（不在本次修复范围）

1. **`Upsert` 的「空值不覆盖」语义两个仓储仍然不同**（先于本次改动存在）：
   sqlite 用 `CASE WHEN excluded.x <> ''` 保留旧值，mem 是整条替换（只保留 `FirstSeen`）。
   后果：目录交换送来的空 `display_name` 在 mem 上会抹掉已有名字，sqlite 不会。
   B2 的粘性用例刻意用了非空名字，因此检不出这条差。将来若有用例依赖它，两个实现要一起对齐。
2. **`Upsert` 的 `last_seen` 零值处理两边不同**（先于本次改动存在）：
   sqlite 无条件写 `excluded.last_seen`（零值会写成 0，破坏排序与 `SeenRecently`），
   mem 在零值时回填 `clk.Now()`。端口注释只对 `SetSuppressed` 的 `at` 做了零值警告。
3. **`test/e2e` 的 `TestFileTransferResumesAfterInterruption` 在全仓并发执行时偶发失败**：
   2026-10-10 实测单独跑 3 次、全仓连跑 2 次均通过，失败那次不经过本次改动的方法，
   判定为时序偶发（与 `docs/plans/2026-10-08-folder-transfer.md` 记录的同类现象一致）。
