# AGENTS.md — kopicode

kopicode is a terminal coding agent, and a harness you can tune per model. One engine
(`internal/engine`) is driven by several front ends: a REPL, headless `run --print`,
resident `serve` (JSON-RPC) and `mcp` (MCP server), and the `kopibench` benchmark runner.
The bet is that harness quality — tool-call parse-and-repair, an edit tool that tolerates
imprecision, forced verification — moves a cheap open-weight model's measured coding
ability by a large margin, and that the rig proves it rather than asserting it. Go, one
static binary, no CGo, no durable-execution runtime. What it is for and how to use it:
[`README.md`](README.md).

**Trust the code over the docs.** Where `docs/`, a docstring or this file disagree with the
code, the code is right: verify, then fix the doc in the same PR. `ls internal/`,
`git log --oneline -30` and a package's own doc comment beat any paragraph written about
them. Where a document contradicts an ADR in [`docs/adr/`](docs/adr/README.md), the ADR wins.

## Read this first if you are a sub-agent

Read [`.claude/skills/agent-ground-rules/SKILL.md`](.claude/skills/agent-ground-rules/SKILL.md)
before running any command or writing any test. The one fact worth repeating here because
it has already cost this repo a broken checkout: **a git worktree is not a separate
repository.** It shares `.git/config`, the object store and the ref store with the repo it
came from — only `HEAD` and the index are private — so a git subprocess with no working
directory set runs in the *real* repository. The skill has the rules that prevent it and the
checks that catch it.

**At most two subagents work in this repository at once.** Lease a `treehouse` worktree each
(see [Worktrees](#worktrees-lease-them-do-not-sweep-them)) and do not exceed two even if
more work is queued.

## Environment and commands

- **Go 1.26.5** from the official tarball at `~/.local/go`. **Never export `GOROOT`**: the
  `go` binary finds its own root, and a stale `GOROOT` (an old `/usr/local/go`) makes every
  Go command fail with "cannot find GOROOT directory". `PATH` needs `~/.local/go/bin` and
  `~/go/bin`. If `go` fails that way, `env | grep GOROOT` and fix the shell profile.
- **`golangci-lint` v2.12.2 and `gitleaks` v8.30.1** live in `~/go/bin`, the versions CI
  pins; `make dev` installs the tools (it tracks `@latest`, so pin to CI's if lint differs).
- `OPENROUTER_API_KEY` is read from the environment only. It must never appear in the
  journal, a blob, a log line, or a test fixture.

Prefer the `make` targets — the pre-push hook and CI both invoke them, so a gate's
*definition* cannot drift between them. `make help` lists all of them. The ones that matter:

```bash
make check        # fmtcheck + vet + lint + tidycheck + install-test: every cheap static gate
make test         # go test -short -race -count=1 ./...: fast loop, mock provider only
make test-all     # the FULL suite as CI runs it (-race, integration tag, e2e git fixtures)
make xbuild       # cross-compile AND vet every GOOS/GOARCH target (the no-CGo promise)
make bench-smoke  # the 13-task corpus against the MOCK provider: zero tokens, required in CI
make secrets      # gitleaks over history and the tree
make ci           # check + test-all + bench-smoke + xbuild: everything CI runs offline
```

- **The pre-push hook (`check`, `test`, `secrets`) is not a CI predictor.** CI also runs
  `test-all`, `xbuild` and `bench-smoke`, so run `make ci` before pushing; a green hook does
  not mean green CI, and `--no-verify` does not change that. Chain gates with `&&` so a red
  one stops you before the push.
- **`-race` and `-count=1` are not optional.** The loop is concurrent (streaming, tool
  dispatch, cancellation), and Go caches test results: a cached pass looks identical to a
  real one. Never run a bare `go test`.
- **`make bench` spends real money** (about $0.08 observed, more if a task sticks to the turn
  cap) and never runs in `ci` or the hook. `make bench-smoke` is the free equivalent.
- **`fmt`/`fmtcheck` are fed `go list` output, not `.`**: gofmt walks directories literally,
  including `.`-prefixed ones, so another agent's checkout under `.claude/worktrees/` would
  otherwise fail your hook.
- A user downloads one binary and uses kopicode; there is no Go toolchain on their machine.
  Releases are a deliberate human act (pushing a `v*` tag) described in
  [`docs/RELEASING.md`](docs/RELEASING.md). Don't add an install step that assumes a compiler.

## Workflow conventions

- **`main` is protected — PR-only, never push to `main`.**
- **Branch per slice:** `git switch -c feat/<slice>` off `origin/main`; open a PR.
- **No AI-authorship trailer.** Commits and PRs are authored by Jian alone — no
  `Co-Authored-By`, no "Generated with" footer, regardless of which agent did the work.
- **Strict branch protection serialises landings.** Every merge puts other open PRs out
  of date, so each needs `gh pr update-branch <n>` and a full re-run before it can
  merge. Wait for all seven checks to be *created and resolved*, not merely
  "not pending."

### Worktrees: lease them, do not sweep them

Manage worktree lifecycle with **`treehouse`** rather than hand-rolled `git worktree
add` plus a cleanup sweep, which is the combination that eventually deletes a worktree
an agent is still working in.

```bash
treehouse get --lease --lease-holder agent-<id>   # prints the path; never handed out twice
treehouse status --json                            # what is live — read this before cleaning
treehouse return <path>                            # release when the agent is done
treehouse prune                                    # dry run by default; --yes to act
treehouse destroy <path>                           # dry run by default; skips risky classes
```

A leased worktree is never handed out by a later `get`, and never removed by `prune`
until it is returned. `prune` only treats a worktree as stale when it is unleased,
idle, clean, and already merged into the default branch.

**`treehouse` is a lifecycle tool, not an isolation boundary** — a leased worktree is
still an ordinary git worktree sharing config, objects, refs and the stash with its
parent (see the sub-agent note at the top of this file). Clean up only when nothing is
running, and remove completed agents' worktrees **by path**, never by sweeping a glob
— then run `golangci-lint cache clean`, since its cache is keyed on package content and
a deleted sibling checkout otherwise resurfaces as findings against paths that no
longer exist.

**One honest gap:** an agent run under the harness's `isolation: "worktree"` gets a
worktree the harness creates itself under `.claude/worktrees/`, which `treehouse` does
not manage or see. Brief such an agent to `treehouse get --lease` explicitly instead.

## Boundaries that must not be crossed

These are the product's structural promises. Hold them.

- **`internal/` is the engine boundary.** Nothing outside this module imports it. Do
  not add a `pkg/` escape hatch, and do not promote `internal/engine` to a published
  module without an ADR.
- **Every front end talks to the engine through its interface only** (REPL, `run --print`,
  `serve`, `mcp`, `kopibench`; `cmd/kopicode/session` is part of `cmd/`, not a new one). The
  import-hygiene allowlist has three entries — `internal/engine`, `internal/bench`, and
  `internal/build` (a leaf, allowed only for `--version`). Do not add a fourth without
  an ADR; `engine.Open` exists precisely so a front end doesn't need one.
- **One session record, and it is the journal.** Do not build a parallel transcript.
  Everything any surface prints is **derived** from journal events — the lesson from
  sibei-flow
  [ADR-0013](https://github.com/leejianrong/sibei-flow/blob/main/docs/design/adr/0013-transcript-tagged-union.md),
  where a hand-rolled, clipped transcript drifted from reality the first time someone
  forgot to append a call.
- **`slog` never duplicates the journal.** The journal owns session content; `slog`
  owns engine-internal diagnostics only (startup, config resolution, lock acquisition),
  off by default, to **stderr** since `--print` owns stdout.
- **The fixture recorder scrubs secrets at write time**, via a header **allowlist**,
  not a denylist — scrubbing on read means the key already sits in a committable file.
- **Never truncate tool output.** Payloads over 64 KiB spill to content-addressed
  blobs. Clipping the diagnostic output that justifies a fix, exactly where a reviewer
  looks, is the specific failure being designed out.
- **Events are a compatibility surface from the first commit.** Version them, and make
  the unmarshaller preserve unknown types rather than dropping them.
- **Edits fail closed.** An `edit_file` whose anchors no longer match the file is
  rejected, never applied somewhere plausible. Fuzzy matching is a fallback that marks
  the session `unattributed`. Never add an edit path that can land in the wrong region
  without emitting a signal.
- **Shell out, do not link.** Language tooling, git, and diff rendering are all
  subprocesses — no CGo, which would cost cross-compilation and break `go install` for
  users without a compiler.
- **Dependencies stay near-zero**, and adding one needs a reason in the PR description.
  **Never add a diff library** — a tree diff for a human to read shells out to `git
  diff --no-index` (zero deps, exact fidelity); a diff that gets journaled is built
  in-process (`internal/tools/edit.go`), because `git diff`'s output varies with the
  installed git version and a replayed session must be byte-identical.
- **Consent is never loosened into prefix or substring matching.** ADR-0008, -0011 and -0016
  each rejected it ("the version of this feature that quietly becomes allow everything"). A
  shell line is analysed by token (`internal/permission/shellscan.go`), every command in it
  must pass, and anything the tokenizer cannot account for is **denied**, not guessed at
  (ADR-0017, ADR-0018). `auto` mode is not a sandbox, and its never-allow list can be added to
  but never shortened.
- **The engine decides policy; the surfaces decide presentation.** The engine decides
  *that* permission is required and what a decision means, never how it is asked.
- **Never touch the user's git state.** Shadow refs live under `refs/kopicode/`,
  through a throwaway `GIT_INDEX_FILE`. `.kopicode/` goes in `.git/info/exclude`, never
  `.gitignore`.
- **Every subprocess names the directory it runs in *and* builds its own
  environment**, in tests as well as product code. `Dir` alone is not enough —
  `GIT_DIR` overrides the working directory entirely, and this has already set
  `core.bare = true` in the real `.git/config` and staged a stash that would have
  deleted every tracked file. `internal/arch/subprocess_test.go` enforces both halves
  statically over every `exec.Command`/`exec.CommandContext`/struct-literal `Cmd` in
  the tree, and fails closed on one it can't follow. A genuine exception carries
  `//kopicode:allow-nodir: <reason>` or `//kopicode:allow-noenv: <reason>` — a waiver
  with no reason waives nothing.
  - **Assigning `Env` isn't the same as deciding what it holds.** `cmd.Env =
    os.Environ()` (or `nil`) inherits everything while satisfying the assignment rule —
    a separate, `//kopicode:allow-ambientenv`-waivable check reads the actual value.
    Every package that spawns something owns a named environment builder (an
    **allowlist** for secrets, where missing one is unbounded risk; a **denylist** for
    a toolchain invocation, where enumerating every variable that could change what
    `go build`/`node`/`python` does is a losing game). Build from one of those, never
    from the environment you were handed — see `internal/arch/subprocess_test.go` for
    the enforced list of builders.
- **Pin the provider on every benchmark request.** `provider.order`,
  `allow_fallbacks: false`, fixed `quantizations`, all recorded per result. An unpinned
  A/B number is not evidence.

## Test seam

The **primary seam is the engine's public interface, driven by the mock/replay
provider against a temp git-repo fixture**, with an injected clock and a seeded RNG —
testable at zero token cost, deterministic given fixed provider output.

Assert **observable outcomes** — journal events, the resulting tree, exit codes,
captured stdout — never internal loop state.

The mock provider **replays recorded traffic rather than synthesising it**, so it
doesn't mask real breakage by drifting from actual provider behaviour. Two
recorded fixtures now ship (`recorded_*`, deepseek/deepseek-v3.2), made by
`kopibench run --record-dir`; the three older ones are still hand-authored and
say so, and are being replaced as tasks are recorded.

## Where things are

- [`agent_docs/packages.md`](agent_docs/packages.md) — what each package is and *why it is
  shaped that way*, and the module map. Read it before changing a package whose reason you
  don't know.
- [`docs/adr/README.md`](docs/adr/README.md) — the decisions of record, one line each.
  Read the ADR before proposing anything it settles.
- [`docs/kopicode-serve-protocol.md`](docs/kopicode-serve-protocol.md),
  [`docs/kopicode-mcp.md`](docs/kopicode-mcp.md),
  [`docs/run-print-protocol.md`](docs/run-print-protocol.md) — the three machine wires: the
  methods, event schema, error codes, consent modes, and the policy and ask flags.
- [`docs/PRD.md`](docs/PRD.md) (requirements, scope boundary) and
  [`docs/SLICE-1.md`](docs/SLICE-1.md) (the first slice's build plan).
- [`docs/provider-pin.md`](docs/provider-pin.md) — which provider and quantization every
  benchmark request pins, and why. [`docs/token-growth.md`](docs/token-growth.md) — real
  per-turn context growth, and what it does and doesn't say about compaction.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — the human-facing version of the setup and workflow.
