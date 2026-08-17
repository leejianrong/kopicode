.DEFAULT_GOAL := help
SHELL := /bin/bash

# Load a local .env if one exists, so OPENROUTER_API_KEY (or anything else a
# developer put there) reaches `smoke-live` and `bench` without kopicode itself
# ever parsing dotenv files (KAN-910) — the binary still reads the credential only
# from the ambient process environment, via internal/provider.APIKeyFromEnv.
# `-include` is silent when the file is absent, which is every CI runner and every
# machine that has not opted in, so this changes nothing there. The bare `export`
# marks every make variable — the ones above, whatever .env just defined, and
# everything defined below — for export to each recipe's child process; it prints
# nothing, so a value from .env never appears in `make`'s own output.
-include .env
export

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
# nothing else does. That sentence used to be an assumption; KAN-833 measured it
# and it holds for all four (the evidence is in the lint block below). What it
# does NOT mean is that those four are confined to this checkout — golangci-lint
# leaks across checkouts through its cache instead, which is a different
# mechanism and needs a different fix. Read both blocks together.
#
# On a machine running parallel agents that means
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

# golangci-lint is confined to this checkout by its CACHE, not by its file list.
# KAN-833, and the order matters because the obvious fix is the wrong one.
#
# The file list was never the problem, which was measured rather than reasoned
# about. With an unformatted Go file carrying a compile error planted in both
# .kan833probe/ and _kan833probe/, `gofmt -l .` listed both, while `go list
# ./...`, `go build ./...`, `go vet ./...` and `go test ./...` ignored both and
# stayed green. Under `strace -f -e trace=openat golangci-lint run` the linter
# made 47053 openat calls and not one named either directory. So confining lint
# to $(GOPKGDIRS) the way fmtcheck is confined would change nothing, and would
# leave a second comment asserting a mechanism that is not the one at work.
#
# The leak is the result cache. It lives in GOLANGCI_LINT_CACHE, default
# ~/.cache/golangci-lint — ONE directory shared by every checkout on the machine
# — and its key is the package's content, not its path. Two checkouts of this
# module whose sources match therefore hit each other's entries, and a restored
# issue carries the absolute filename of whichever checkout computed it first.
# When that checkout was an agent worktree that has since been removed, the
# result is a finding reported against a path that no longer exists. Reproduced
# by planting a misspelling, linting a throwaway copy of the tree, deleting the
# copy, and linting here again:
#
#   level=warning msg="[runner] Can't process results by generated_file_filter
#     processor: ... /tmp/<copy>/internal/bench/errors.go: no such file or directory"
#   ../../../../../../../../tmp/<copy>/internal/bench/errors.go:1:64: `recieve`
#     is a misspelling of `receive` (misspell)
#
# Note the exit code there is 1 and the finding is still printed. This is
# misattribution and noise, NOT a vacuous green: the cache is keyed on content,
# so a reused issue is always a true issue about identical bytes, merely labelled
# with a path the reader cannot open, and a failed processor leaves the issue
# list untouched rather than dropping it. That is why this gate gets no
# require_pkgdirs-style guard — unlike `gofmt -l` handed an empty file list, it
# has no way to report success having checked nothing.
#
# A per-checkout cache directory fixes it at the mechanism, and keeps working
# when the next dot-directory is called something other than .claude. $(BIN) is
# already git-ignored and already what `make clean` removes, so the cache needs
# no new ignore rule; the price is one cold run per checkout, ~4.3s against ~0.9s
# warm.
#
# CI never caught this and never will: it runs on a clean checkout with no
# .claude/worktrees/ and a cold cache, so there is no sibling to collide with.
# This defect exists only on a machine running parallel agents.
.PHONY: lint
lint: export GOLANGCI_LINT_CACHE := $(CURDIR)/$(BIN)/golangci-lint-cache
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

# The one test that reaches openrouter.ai. Behind the `live` build tag so that
# neither `test` nor `test-all` can pick it up, which is what keeps the default
# suite hermetic and free. It sends one 16-token request against the pinned
# endpoint — cents, not dollars — and prints what came back, including the exact
# casing of the response's `provider` field, which OpenRouter documents nowhere
# and docs/provider-pin.md has to record rather than guess.
#
# -v because the output is the point: this target exists to produce evidence, not
# a pass.
.PHONY: smoke-live
smoke-live: ## ONE live request against the pinned provider — spends a few cents
	@[ -n "$$OPENROUTER_API_KEY" ] || { echo "OPENROUTER_API_KEY is unset"; exit 1; }
	go test -v -race -count=1 -tags=live -run TestLiveCompletion ./internal/provider/

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

# bench-smoke joined this list when cmd/kopibench stopped being the stub that
# exits 4 (KAN-796, wired into the workflow by KAN-801). It is the MOCK-provider
# target: no network, no tokens, a couple of seconds. `bench` is the paid one and
# is never here and never in the hook. It runs before xbuild because it is much
# the faster of the two, and a gate that fails early is worth more than a tidy
# ordering.
#
# `secrets` and `vuln` are CI jobs that are still deliberately absent: the
# pre-push hook already runs `secrets`, and both need a tool this target does not
# install. So `ci` is the offline compile-and-behaviour subset of the workflow
# rather than a literal mirror of it.
.PHONY: ci
ci: check test-all bench-smoke xbuild ## Every CI gate that runs offline

.PHONY: install-hooks
install-hooks: ## Install the pre-push hook
	./scripts/install-hooks.sh

.PHONY: clean
clean: ## Remove build output and coverage
	rm -rf $(BIN) coverage.txt
