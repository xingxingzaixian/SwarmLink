.PHONY: build test test-domain-cover test-e2e test-scale test-fuzz lint-arch frontend-install frontend-build frontend-typecheck ci bindings gui gui-dev package

# 桌面入口 main_wails.go 带 `//go:build wails`：不传这个 tag 时，根目录一个
# 可编译的 Go 文件都没有（原先的占位入口已删除），`go build` 会【直接失败】。
#
# 这正是想要的行为：以前忘传 tag 会编出一个 2.4MB 的占位程序并被正常打包成
# 安装包 —— 产物是假的、构建却报成功，是最难发现的一类错误。
#
# 因此这个 tag 必须【由调用方】给出：wails3 CLI 只把 `-tags` 映射成 EXTRA_TAGS
# （internal/commands/task_wrapper.go），不带 `-tags` 时它什么都不加，
# 而 build/*/Taskfile.yml 的生产 BUILD_FLAGS 恰好只有 `-tags production`。
#
# 用 make 变量而不是写死在每条 recipe 里：make 会把变量导出到子进程环境，
# 而 wails3 的 Taskfile 正是从 EXTRA_TAGS 环境变量读取它 ——
# 这样 build/package 里任何【嵌套】的 build 调用（包括 deps 链）都能拿到，
# 不必依赖 go-task 的变量传递规则。
export EXTRA_TAGS = wails

build:
	go build ./...

test:
	go test ./... -count=1

test-domain-cover:
	go test ./internal/domain/... -cover

test-e2e:
	go test ./test/e2e/... -count=1 -v

test-scale:
	go test ./test/scale/... -count=1 -v

test-fuzz:
	go test ./internal/domain/protocol/ -run='^$$' -fuzz=FuzzRead -fuzztime=20s

# 架构红线检查（架构书 7.1）：让架构约束可执行，而非靠自觉。
#
# R1 只匹配【真实的 import 行】（行首可选空白 + 引号包裹的路径 + 行尾），
# 以免把注释里出现的 "net" / "os" 等词语误判为导入。
lint-arch:
	@! grep -rnE '^[[:space:]]*("net"|"database/sql"|"os"|"[^"]*wails[^"]*")[[:space:]]*$$' internal/domain/ --include=*.go | grep -v '_test.go' | grep -v 'domain/ports' || (echo "R1 VIOLATION: domain must not import net/database-sql/os/wails" && exit 1)
	@! grep -rn 'internal/adapters/store' internal/adapters/net/ --include=*.go || (echo "R2 VIOLATION: adapters must not cross-import" && exit 1)
	@! grep -rnE '\b(sqlite\.New|tcp\.NewManager)\b' internal/app/ internal/domain/ --include=*.go | grep -v '_test.go' || (echo "R3 VIOLATION: business code must not construct adapters" && exit 1)
	@echo "arch OK"

frontend-install:
	cd frontend && npm install

frontend-build:
	cd frontend && npm run build

frontend-typecheck:
	cd frontend && npm run typecheck

# 本地等价于 CI 的一条命令
ci: build lint-arch test frontend-typecheck frontend-build

# 绑定生成（服务注册在根目录 main_wails.go，因此从 . 扫描）
#
# -ts 不能省：省略时生成器输出 .js，而入库的是 .ts —— 混在一起后
# `import ... from './models.js'` 会解析到 JS 文件，类型全部退化成 any。
# -i 生成聚合的 index.ts。
bindings:
	wails3 generate bindings -f "-tags wails" -clean=true -ts -i .

# 桌面 GUI（产物 bin/SwarmLink）
gui: bindings
	wails3 build -tags wails

# 桌面安装包（当前平台）：Windows → NSIS 安装器，macOS → .app / .dmg，Linux → AppImage / deb / rpm
#
# 注意这里【不】传 -tags：wails3 package 的参数里根本没有 tags
# （internal/commands/task_wrapper.go 的 Package 签名忽略它），
# 只能靠上面 export 的 EXTRA_TAGS 环境变量传下去。
package: bindings
	wails3 package

# 开发模式（热重载）
gui-dev: bindings
	wails3 dev -config ./build/config.yml -port 9245
