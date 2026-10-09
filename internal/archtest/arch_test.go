// Package archtest 把《架构设计书》§3.2 的三条依赖红线固化成【可执行测试】。
//
// 为什么用测试取代 Makefile 里的 grep（原来的做法，见 Makefile 的 lint-arch）：
//
//  1. grep 分不清「注释里出现的 net」与「真的 import net」。原规则为此写了一串
//     行首/行尾锚定的正则，只能在注释里反复警告后人别改坏它。
//  2. 原 R2 只检查 adapters/net → adapters/store 这一条路径，而 R2 的语义是
//     「适配器之间禁止互引」—— 盲区里其实已经躺着一条真实互引（见下表的宽限名单）。
//  3. 最关键：红线只活在 Makefile 里，而 Windows 开发机上通常没有 make（本仓开发机
//     实测就没有）。「三条红线由 CI 强制」这句话同时写在 README、ADR-001、ADR-007
//     与实现规格里，但在本地根本无从执行。
//
// 改用标准库 go/parser 判定后，任何能跑 go test 的地方都能跑红线，并且随
// `go test ./...` 自动执行 —— 不需要 make，也不需要引入任何新依赖。
//
// 本包只含测试文件：红线检查不是产品代码，不应进入任何二进制。
package archtest

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go/format"
	"go/parser"
	"go/token"
)

// modulePath 是本仓的模块路径（与 go.mod 一致）。
const modulePath = "github.com/swarmlink/swarmlink"

// forbiddenForDomain 是 R1 禁止 domain 层 import 的包（「domain 零 I/O」）。
//
// 判定方式是「精确包名 + 子包前缀」，因此 net/http、os/exec、database/sql/driver
// 会被一并拦下。
//
// 与原规则的两点有意差异：
//   - 原规则额外豁免了 domain/ports。该豁免现已无必要：ports.go 只 import
//     context / errors / time 与各 domain 包（它当年需要 os，是因为 FileSink 曾把
//     OpenTemp() (*os.File, error) 放进接口，后来接口收窄了）。一条无人解释、且
//     已经不起作用的豁免，只会让下一个人误以为「ports 可以引 I/O」，故不再保留。
//   - 原规则只拦精确的 "os"，拦不住 os/exec。这里按前缀处理。
var forbiddenForDomain = []string{
	"net", // 网络 I/O：Dial / Listen / LookupHost
	"os",  // 文件与进程 I/O（含 os/exec）
	"database/sql",
	"syscall",
	"io/ioutil",
}

// allowedNetSubpackages 是 net/ 下【纯值类型】的例外：它们不做任何 I/O，
// 因此不违反「domain 零 I/O」的本意。
//
// 为什么写出来，而不是让 `net` 前缀一刀切：netip 的设计目标就是"不含 I/O 的 IP
// 值类型"，net/url 同理。把 IP 地址或 URL 的表达放进领域模型是正当需求；一刀切
// 只会逼后人要么把领域逻辑挪到 app 层，要么去引一个第三方 IP 库 —— 那才是真的坏结果。
var allowedNetSubpackages = []string{
	"net/netip",
	"net/url",
}

// adapterCrossImports 记录【已知且已接受】的适配器互引，作为整改前的宽限名单。
//
// 为什么要有宽限名单，而不是把规则放宽到能通过：原来的 R2 只查
// adapters/net → adapters/store 一条路径，于是下表这条真实互引一直没被发现。
// 显式登记有两个作用：① 规则对【新增】互引立即生效；② 这笔债留在代码里可见，
// 而不是藏在一条窄 grep 的盲区里。
//
// 待整改（见 docs/plans/2026-10-09-s1-可信基线.md 的 T2c）：wails 的
// SettingsService 直接依赖 config 适配器（既用 *config.Config 作字段与 DTO 来源，
// 也直接调 config.Save）。整改方向是让配置的进出走 ports 接口、由组合根注入，
// 而不是让两个适配器互相认识。这会动到设置页的 DTO 与启动装配，因此单独立项。
var adapterCrossImports = map[string]string{
	"internal/adapters/wails": "internal/adapters/config",
}

// goFile 是一个已解析出的 Go 源文件。
type goFile struct {
	// rel 是相对模块根、以 "/" 分隔的路径（如 internal/app/router.go）。
	rel string
	// imports 是 import 路径原文。
	imports []string
}

func (f goFile) isTest() bool { return strings.HasSuffix(f.rel, "_test.go") }

// ---------------------------------------------------------------------------
// 规则判定（纯函数，独立可测）
// ---------------------------------------------------------------------------

// isForbiddenForDomain 报告某个 import 是否违反 R1。
func isForbiddenForDomain(imp string) bool {
	// Wails 是入站适配器框架，domain 层不得触碰（按子串判定：wails v3 的包路径
	// 形如 github.com/wailsapp/wails/v3/pkg/application，将来还可能有别的子包）。
	if strings.Contains(imp, "wails") {
		return true
	}
	// 例外先判：`net` 的前缀规则会把 net/netip、net/url 一并吃掉，
	// 而它们是纯值类型、不做 I/O。
	for _, ok := range allowedNetSubpackages {
		if imp == ok || strings.HasPrefix(imp, ok+"/") {
			return false
		}
	}
	for _, p := range forbiddenForDomain {
		if imp == p || strings.HasPrefix(imp, p+"/") {
			return true
		}
	}
	return false
}

// adapterUnit 返回文件所属的最小适配器包路径。
//
// 取最小单元而不是 adapters/net 这样的父目录：tcp 与 udp、sqlite 与 mem 在架构书
// §3.3 里是被当作【独立适配器】列的，它们之间同样不应互相认识。
func adapterUnit(rel string) string { return path.Dir(rel) }

// importedAdapterUnit 把一条 import 路径映射回本仓内的适配器包路径；
// 不是本仓适配器的返回空串。
func importedAdapterUnit(imp string) string {
	rest, ok := strings.CutPrefix(imp, modulePath+"/")
	if !ok {
		return ""
	}
	if !strings.HasPrefix(rest, "internal/adapters/") {
		return ""
	}
	return rest
}

// belongsToBusinessCode 报告该文件是否属于「业务代码」（app 用例层 + domain 领域层）。
func belongsToBusinessCode(rel string) bool {
	return strings.HasPrefix(rel, "internal/app/") || strings.HasPrefix(rel, "internal/domain/")
}

// ---------------------------------------------------------------------------
// 源码采集
// ---------------------------------------------------------------------------

// moduleRoot 从当前测试所在目录向上找到含 go.mod 的目录。
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("从测试目录向上找不到 go.mod，无法定位模块根")
		}
		dir = parent
	}
}

// collectGoFiles 解析 internal/ 下全部 Go 文件的 import（只读 ImportsOnly）。
func collectGoFiles(t *testing.T, root string) []goFile {
	t.Helper()
	fset := token.NewFileSet()
	var out []goFile

	walkErr := filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		parsed, perr := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if perr != nil {
			return fmt.Errorf("解析 %s: %w", p, perr)
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		imports := make([]string, 0, len(parsed.Imports))
		for _, spec := range parsed.Imports {
			unquoted, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				return fmt.Errorf("%s: 无法解析 import 字面量 %s", p, spec.Path.Value)
			}
			imports = append(imports, unquoted)
		}
		out = append(out, goFile{rel: filepath.ToSlash(rel), imports: imports})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("遍历/解析 Go 源文件失败: %v", walkErr)
	}

	// 一条"永远不报错"的红线比没有红线更糟：它会把「CI 强制」变成空话。
	// 采集为空通常意味着路径写错或工作目录不对，必须当场失败而不是静默通过。
	if len(out) == 0 {
		t.Fatal("没有解析到任何 Go 文件，红线检查会形同虚设")
	}
	return out
}

// ---------------------------------------------------------------------------
// 三条红线
// ---------------------------------------------------------------------------

// TestR1DomainHasNoIO：domain/** 不得 import net / os / database/sql / syscall / Wails。
func TestR1DomainHasNoIO(t *testing.T) {
	root := moduleRoot(t)
	for _, f := range collectGoFiles(t, root) {
		// 测试文件可以用 os（读写临时文件）；红线约束的是产品代码的依赖方向。
		if f.isTest() || !strings.HasPrefix(f.rel, "internal/domain/") {
			continue
		}
		for _, imp := range f.imports {
			if isForbiddenForDomain(imp) {
				t.Errorf("R1 违规：%s 不得 import %q —— domain 层必须零 I/O（架构书 §3.2）", f.rel, imp)
			}
		}
	}
}

// TestR2AdaptersDoNotImportEachOther：适配器之间禁止互引，跨模块只走 ports 接口或事件总线。
func TestR2AdaptersDoNotImportEachOther(t *testing.T) {
	root := moduleRoot(t)
	for _, f := range collectGoFiles(t, root) {
		// 测试文件跨引适配器是刻意的：内存适配器与 sqlite 适配器互作替身正是本仓的测试策略。
		if f.isTest() || !strings.HasPrefix(f.rel, "internal/adapters/") {
			continue
		}
		from := adapterUnit(f.rel)
		for _, imp := range f.imports {
			to := importedAdapterUnit(imp)
			if to == "" || to == from {
				continue
			}
			if adapterCrossImports[from] == to {
				continue // 已登记的历史欠账，见 adapterCrossImports 的说明
			}
			t.Errorf("R2 违规：%s（适配器 %s）不得 import 适配器 %s —— 跨模块只走 ports 接口或事件总线（架构书 §3.2）",
				f.rel, from, to)
		}
	}
}

// TestR3BusinessCodeDoesNotDependOnAdapters：app / domain 不得依赖任何具体适配器。
//
// 这条比原规则更强也更准。原规则用 grep 找两个字面量（sqlite.New、tcp.NewManager），
// 因此「import 了适配器但没调这两个构造函数」不算违规 —— 可那同样是业务代码反向依赖
// 了具体实现（依赖倒置的全部意义就在这里）。改为「不得 import internal/adapters/**」，
// 由组合根负责装配。
func TestR3BusinessCodeDoesNotDependOnAdapters(t *testing.T) {
	root := moduleRoot(t)
	for _, f := range collectGoFiles(t, root) {
		if f.isTest() || !belongsToBusinessCode(f.rel) {
			continue
		}
		for _, imp := range f.imports {
			if strings.HasPrefix(imp, modulePath+"/internal/adapters/") {
				t.Errorf("R3 违规：%s 不得 import 具体适配器 %q —— 业务代码只依赖 ports 接口，由组合根注入（架构书 §3.2）",
					f.rel, imp)
			}
		}
	}
}

// TestRuleHelpersCatchWhatTheyClaim 钉住三条规则的边界判定。
//
// 存在的理由与本仓既有的教训一致：一个语义走样的测试替身会让用例变成空跑。
// 红线检查本身如果不做边界测试，一行写错就可能让它永远返回"无违规"，
// 而所有人都以为红线在生效。
func TestRuleHelpersCatchWhatTheyClaim(t *testing.T) {
	domainImports := []struct {
		imp  string
		want bool
	}{
		{"net", true},
		{"net/http", true},
		{"net/http/httptest", true},
		{"os", true},
		{"os/exec", true},
		{"database/sql", true},
		{"database/sql/driver", true},
		{"syscall", true},
		{"io/ioutil", true},
		{"github.com/wailsapp/wails/v3/pkg/application", true},
		// 纯值类型与普通依赖不算 I/O，不应误报 —— 否则规则会被迫放宽到没意义。
		{"net/url", false},
		{"net/netip", false},
		{"fmt", false},
		{"time", false},
		{"encoding/json", false},
		{"context", false},
	}
	for _, c := range domainImports {
		if got := isForbiddenForDomain(c.imp); got != c.want {
			t.Errorf("isForbiddenForDomain(%q) = %v，期望 %v", c.imp, got, c.want)
		}
	}

	units := []struct {
		rel  string
		want string
	}{
		{"internal/adapters/wails/services.go", "internal/adapters/wails"},
		{"internal/adapters/net/tcp/manager.go", "internal/adapters/net/tcp"},
		{"internal/adapters/config/config.go", "internal/adapters/config"},
	}
	for _, c := range units {
		if got := adapterUnit(c.rel); got != c.want {
			t.Errorf("adapterUnit(%q) = %q，期望 %q", c.rel, got, c.want)
		}
	}

	imports := []struct {
		imp  string
		want string
	}{
		{"github.com/swarmlink/swarmlink/internal/adapters/config", "internal/adapters/config"},
		{"github.com/swarmlink/swarmlink/internal/adapters/store/mem", "internal/adapters/store/mem"},
		// 指向 app / domain / 外部库的 import 不是「适配器互引」。
		{"github.com/swarmlink/swarmlink/internal/domain/ports", ""},
		{"github.com/swarmlink/swarmlink/internal/app", ""},
		{"fmt", ""},
		{"github.com/google/uuid", ""},
		// 前缀相同但不是本模块的包，不得误判。
		{"github.com/swarmlink/swarmlink-extra/internal/adapters/x", ""},
	}
	for _, c := range imports {
		if got := importedAdapterUnit(c.imp); got != c.want {
			t.Errorf("importedAdapterUnit(%q) = %q，期望 %q", c.imp, got, c.want)
		}
	}

	business := []struct {
		rel  string
		want bool
	}{
		{"internal/app/router.go", true},
		{"internal/domain/peer/peer.go", true},
		{"internal/adapters/wails/services.go", false},
		{"internal/infra/log/log.go", false},
	}
	for _, c := range business {
		if got := belongsToBusinessCode(c.rel); got != c.want {
			t.Errorf("belongsToBusinessCode(%q) = %v，期望 %v", c.rel, got, c.want)
		}
	}
}

// TestSourcesAreGofmtClean：仓库必须保持 gofmt 干净。
//
// 用标准库 go/format 而不是外部 gofmt 进程：与本包其他检查一致 —— 不依赖 PATH、
// 不依赖 make，任何能跑 go test 的地方都能跑（本仓开发机没有 make）。
//
// 为什么值得设成闸门：这个仓库此前并不干净，而且**没有任何人发现**：
//   - 7 个 .go 文件是以 CRLF 提交进仓库的（在 Windows 上编辑后直接提交，而仓库
//     既无 .gitattributes、core.autocrlf 也是 false）。gofmt 会把 CRLF 视为需要
//     重写的格式，于是 `gofmt -l` 永远非空 —— 既没法当闸门，也让每次 diff 都
//     混着行尾噪声。
//   - 另有 2 处 struct / const 声明的对齐漂移（新增一个更长的常量名之后没有
//     重新对齐）。
//
// 行尾由 .gitattributes 的 `*.go text eol=lf` 固定，因此这条闸门在任何平台、任何
// 人的检出上结论一致。
func TestSourcesAreGofmtClean(t *testing.T) {
	root := moduleRoot(t)

	// 这些目录不属于本仓源码：.git 巨大且无 .go；node_modules 是第三方；
	// dist/bin 是构建产物（bin/ 里可能有打进去的源码副本）。
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "dist": true, "bin": true, ".task": true,
	}

	var checked int
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		src, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		formatted, ferr := format.Source(src)
		if ferr != nil {
			return fmt.Errorf("解析/格式化 %s: %w", p, ferr)
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if !bytes.Equal(src, formatted) {
			t.Errorf("gofmt 需要重写 %s：请运行 `gofmt -w`（行尾必须是 LF，.gitattributes 已固定 *.go）",
				filepath.ToSlash(rel))
		}
		checked++
		return nil
	})
	if walkErr != nil {
		t.Fatalf("遍历 Go 源文件失败: %v", walkErr)
	}
	if checked == 0 {
		t.Fatal("没有检查到任何 .go 文件，格式闸门形同虚设")
	}
}

// TestGofmtGateCatchesCrlfAndMisalignment 钉住上面那条闸门的判定能力。
//
// 与 TestRuleHelpersCatchWhatTheyClaim 同样的理由：一个永远返回「干净」的闸门
// 比没有闸门更糟 —— 它会让「仓库保持 gofmt 干净」变成一句空话。这里直接对
// format.Source 的行为下断言，正是当年真正出问题的那两种形态。
func TestGofmtGateCatchesCrlfAndMisalignment(t *testing.T) {
	// 注意 A 后面是两个空格：gofmt 会把同一个 const 块里各行的 `=` 对齐，
	// 所以真正"干净"的样本本身就必须是对齐过的 —— 这一条我第一次就写错了，
	// 正是下面那句前置条件断言把它抓出来的。
	clean := []byte("package p\n\nconst (\n\tA  = 1\n\tBB = 2\n)\n")
	if formatted, err := format.Source(clean); err != nil {
		t.Fatalf("干净源码不该报错: %v", err)
	} else if !bytes.Equal(clean, formatted) {
		t.Fatal("前置条件不成立：样本本身应当已经是 gofmt 干净的")
	}

	// 形态一：CRLF 行尾（当年 7 个 .go 文件就是这样提交进仓库的）。
	crlf := bytes.ReplaceAll(clean, []byte("\n"), []byte("\r\n"))
	formatted, err := format.Source(crlf)
	if err != nil {
		t.Fatalf("CRLF 源码不该报错: %v", err)
	}
	if bytes.Equal(crlf, formatted) {
		t.Fatal("闸门抓不到 CRLF：正是它让 gofmt -l 常年非空")
	}

	// 形态二：对齐漂移（当年 message.go / eventbus.go 的形态）。
	misaligned := []byte("package p\n\nconst (\n\tA = 1\n\tBB  = 2\n)\n")
	formatted, err = format.Source(misaligned)
	if err != nil {
		t.Fatalf("对齐漂移的源码不该报错: %v", err)
	}
	if bytes.Equal(misaligned, formatted) {
		t.Fatal("闸门抓不到对齐漂移")
	}
}
