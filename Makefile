.PHONY: build test test-domain-cover test-e2e test-scale test-fuzz lint-arch frontend-install frontend-build frontend-typecheck ci cli gui

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

# 常用入口
cli:
	go build -o bin/swarmlink-cli ./cmd/swarmlink-cli

# 需要先：安装 wails3 CLI 且 go get github.com/wailsapp/wails/v3
gui:
	go build -tags wails -o bin/swarmlink-gui ./cmd/swarmlink-gui
