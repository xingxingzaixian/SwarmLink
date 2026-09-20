.PHONY: build test test-domain-cover lint-arch

build:
	go build ./...

test:
	go test ./...

test-domain-cover:
	go test ./internal/domain/... -cover

# 架构红线检查（架构书 7.1）：让架构约束可执行，而非靠自觉。
#
# 注意：R1 只匹配【真实的 import 行】（行首可选空白 + 引号包裹的路径 + 行尾），
# 以免把注释里出现的 "net" / "os" 等词语误判为导入。
lint-arch:
	@! grep -rnE '^[[:space:]]*("net"|"database/sql"|"os"|"[^"]*wails[^"]*")[[:space:]]*$$' internal/domain/ --include=*.go | grep -v '_test.go' | grep -v 'domain/ports' || (echo "R1 VIOLATION: domain must not import net/database-sql/os/wails" && exit 1)
	@! grep -rn 'internal/adapters/store' internal/adapters/net/ --include=*.go || (echo "R2 VIOLATION: adapters must not cross-import" && exit 1)
	@! grep -rnE '\b(sqlite\.New|tcp\.NewManager)\b' internal/app/ internal/domain/ --include=*.go | grep -v '_test.go' || (echo "R3 VIOLATION: business code must not construct adapters" && exit 1)
	@echo "arch OK"
