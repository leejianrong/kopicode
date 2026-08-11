.DEFAULT_GOAL := help
SHELL := /bin/bash

MODULE   := github.com/leejianrong/kopicode
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
BIN      := bin
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# Cheap gates run everywhere. Expensive gates run in CI only.
# Never let a slow check gate a local push.

.PHONY: help
help: ## List the targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: dev
dev: ## Download deps and install the dev tools
	go mod download
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	go install golang.org/x/tools/gopls@latest
	go install github.com/zricethezav/gitleaks/v8@latest

# gofmt takes directories literally and walks all of them. The rest of the Go
# toolchain — go build, go vet, go test, golangci-lint — skips directories whose
# name begins with "." or "_", so `gofmt -l .` is the one gate that sees files
# nothing else does. On a machine running parallel agents that means
# .claude/worktrees/*, where each agent keeps its own checkout of this same
# module: their half-written files fail the PM's pre-push hook, for a target
# whose failure message gives no hint where the file came from.
#
# Feeding gofmt the output of `go list` confines it to the packages this module
# actually builds, which is the set every other gate already checks. Assigned
# with `=` so the subprocess runs only in the recipes that use it.
GOPKGDIRS = $(shell go list -f '{{.Dir}}' ./...)

.PHONY: fmt
fmt: ## Format every file in place
	gofmt -w $(GOPKGDIRS)

.PHONY: fmtcheck
fmtcheck: ## Fail if any file is unformatted
	@out=$$(gofmt -l $(GOPKGDIRS)); \
	if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: lint
lint: ## golangci-lint (staticcheck is one of its linters, not a separate tool)
	golangci-lint run

.PHONY: tidycheck
tidycheck: ## Fail if go.mod/go.sum are not tidy
	@cp go.mod go.mod.bak; [ -f go.sum ] && cp go.sum go.sum.bak || true; \
	go mod tidy; \
	status=0; \
	if ! diff -q go.mod go.mod.bak >/dev/null; then echo "go.mod is not tidy: run 'go mod tidy'"; status=1; fi; \
	if [ -f go.sum.bak ] && ! diff -q go.sum go.sum.bak >/dev/null; then echo "go.sum is not tidy: run 'go mod tidy'"; status=1; fi; \
	mv go.mod.bak go.mod; [ -f go.sum.bak ] && mv go.sum.bak go.sum || true; \
	exit $$status

.PHONY: check
check: fmtcheck vet lint tidycheck ## All the cheap static gates

# -race because the loop is concurrent and a race here reproduces once a month.
# -count=1 because Go caches test results and a cached pass looks like a real one.
.PHONY: test
test: ## Fast tests: no infra, mock provider only — the inner loop
	go test -short -race -count=1 ./...

.PHONY: test-all
test-all: ## The FULL suite, as CI runs it
	go test -race -count=1 -tags=integration ./...

.PHONY: cover
cover: ## Fast tests with a coverage profile
	go test -short -race -count=1 -coverprofile=coverage.txt ./...
	@go tool cover -func=coverage.txt | tail -1

.PHONY: build
build: ## Build both binaries for this host
	mkdir -p $(BIN)
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/kopicode ./cmd/kopicode
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/kopibench ./cmd/kopibench

# The `go vet ./...` per platform is the load-bearing half, not the binary builds.
#
# cmd/kopicode and cmd/kopibench are stubs that import nothing from internal/, so
# building only them cross-compiled two main packages and proved nothing about the
# code that actually has platform-specific behaviour — path handling, os.Root,
# process groups, flock. The ADR-0001 distribution promise was being checked
# nominally.
#
# `go vet` is used rather than `go build ./...` because it typechecks _test.go
# files too, and a test that fails to compile under GOOS=windows is a platform
# break that only shows up when someone runs the suite there. It costs about the
# same second per platform.
.PHONY: xbuild
xbuild: ## Cross-compile and vet every target — proves the no-CGo distribution promise
	@set -e; for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=""; \
	  [ "$$os" = "windows" ] && ext=".exe"; \
	  echo "  $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go vet ./...; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    go build -ldflags '$(LDFLAGS)' -o $(BIN)/$$os-$$arch/kopicode$$ext ./cmd/kopicode; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    go build -ldflags '$(LDFLAGS)' -o $(BIN)/$$os-$$arch/kopibench$$ext ./cmd/kopibench; \
	done

.PHONY: run
run: build ## Build and run the REPL
	$(BIN)/kopicode

.PHONY: bench-smoke
bench-smoke: build ## Corpus against the MOCK provider — zero tokens, gates a PR
	$(BIN)/kopibench run --provider mock --corpus bench/tasks

.PHONY: bench
bench: build ## Corpus against the REAL pinned provider — COSTS MONEY, never in CI
	@echo "This spends real tokens (~\$$2 per full corpus run). Ctrl-C to abort."
	@read -r -p "continue? [y/N] " a; [ "$$a" = "y" ] || exit 1
	$(BIN)/kopibench run --corpus bench/tasks

.PHONY: secrets
secrets: ## gitleaks over history and the working tree
	gitleaks detect --redact --verbose
	gitleaks dir . --redact --verbose

.PHONY: vuln
vuln: ## govulncheck over the module
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: ci
ci: check test-all xbuild ## Everything CI gates on

.PHONY: install-hooks
install-hooks: ## Install the pre-push hook
	./scripts/install-hooks.sh

.PHONY: clean
clean: ## Remove build output and coverage
	rm -rf $(BIN) coverage.txt
