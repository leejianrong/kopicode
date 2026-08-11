.DEFAULT_GOAL := help
SHELL := /bin/bash

MODULE   := github.com/leejianrong/kopicode

# The build identity that lands on every SessionStarted (KAN-806, ADR-0007
# decision 7). The harness config hash deliberately excludes the code, so two
# arms can carry identical hashes from different binaries; this is what tells
# them apart, and a dirty build is what it must never quietly call clean.
#
# These git commands target the ambient repository on purpose — the checkout
# being compiled *is* the subject, so there is no directory to name and nothing
# to isolate from. All three are read-only, and `status` carries
# --no-optional-locks so it reports without refreshing the index behind the
# user's back (`git status` writes the index while reporting; internal/repo
# refuses it outright for that reason).
#
# Empty is the honest failure. Every one of these substitutes to nothing outside
# a git checkout, and internal/build resolves an absent commit to "unknown"
# rather than to a plausible default, so a no-git build is recorded as one.
BUILDPKG := $(MODULE)/internal/build
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null)
COMMIT   ?= $(shell git rev-parse HEAD 2>/dev/null)

# Deliberately a stricter definition than `git describe --dirty`, which only
# looks at tracked files: an untracked .go file changes the binary just as much
# as an edited one does. Computed separately so the journaled bit is a value a
# machine reads rather than a suffix parsed off a human string.
TREE_STATE ?= $(shell git rev-parse --git-dir >/dev/null 2>&1 && \
  { [ -z "$$(git --no-optional-locks status --porcelain 2>/dev/null)" ] && echo clean || echo dirty; })

LDFLAGS  := -s -w \
  -X $(BUILDPKG).version=$(VERSION) \
  -X $(BUILDPKG).commit=$(COMMIT) \
  -X $(BUILDPKG).treeState=$(TREE_STATE)
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
#
# Both recipes check the list is non-empty first. `go list` fails whenever the
# module does not load — a syntax error, a broken go.mod, a repository someone
# has just half-broken — and an empty list makes `gofmt -l` read stdin, find
# nothing and exit 0. The gate would then report success having checked no files
# at all, which is worse than the failure it was covering for: it is loudest
# exactly when it goes quiet.
GOPKGDIRS = $(shell go list -f '{{.Dir}}' ./...)

define require_pkgdirs
	if [ -z "$(GOPKGDIRS)" ]; then \
	  echo "no packages found: 'go list ./...' failed, so this gate would check nothing"; \
	  echo "run 'go list ./...' to see why"; \
	  exit 1; \
	fi
endef

.PHONY: fmt
fmt: ## Format every file in place
	@$(require_pkgdirs)
	gofmt -w $(GOPKGDIRS)

.PHONY: fmtcheck
fmtcheck: ## Fail if any file is unformatted
	@$(require_pkgdirs)
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
