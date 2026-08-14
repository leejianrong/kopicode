# CLAUDE.md — agent brief for kopicode

kopicode is a **terminal coding agent, and a harness you can tune per model**. A
line-oriented REPL and a headless benchmark runner drive one engine. The bet is that
harness quality — tool-call parse-and-repair, an edit tool that tolerates imprecision,
forced verification — moves a cheap open-weight model's measured coding ability by a
large margin, and that the rig proves it rather than asserting it.

Written in **Go**. Single static binary. No durable-execution runtime.

## Read this first if you are a sub-agent

[`.claude/skills/agent-ground-rules/SKILL.md`](.claude/skills/agent-ground-rules/SKILL.md),
before you run any command or write any test.

The short version, because it has already cost this repo one broken checkout: **a git
worktree is not a separate repository.** It shares `.git/config`, the object store and
the ref store with the repo it came from. Only `HEAD` and the index are private. So a
git subprocess with no working directory set runs in the *real* repository, and one
`git config` from inside a worktree can leave every command in the main checkout
failing with `fatal: this operation must be run in a work tree`. The skill has the
rules that prevent it and the checks that catch it.

## Build status — foundations landed, no loop yet

**Trust the code over the docs.** `docs/` describes the intended system; where they
disagree, the code is the truth for what exists today — verify before you believe a
docstring, including this section.

What is real now (2026-08-11):

- **Module + toolchain.** Go 1.26.5. `Makefile` with the gates below, `.golangci.yml`
  (v2 schema, validated), `.github/workflows/ci.yml` with parallel jobs, and a pre-push
  hook mirroring the cheap gates. Two dependencies, both sanctioned in advance:
  `github.com/google/go-cmp` in tests only, and `golang.org/x/term` for the line
  editor (ADR-0004 decision 2). Pure Go, no CGo, and `golang.org/x/sys` comes with
  it as an indirect.
- **Cross-compilation proven.** `make xbuild` produces both binaries for linux/amd64,
  linux/arm64, darwin/amd64, darwin/arm64 and windows/amd64 with `CGO_ENABLED=0` — the
  ADR-0001 distribution promise, checked rather than asserted.
- **Boundary guards, proven red.** `internal/arch` enforces ADR-0003: front ends may
  import only `internal/engine` and `internal/bench`, and the engine may not import a
  surface. Both were confirmed to fail when violated, and the failure names the file,
  the import and the ADR.
- **`internal/journal`** — the event tagged union. Envelope plus 19 payload types, with
  explicit `MarshalJSON`/`UnmarshalJSON` so an unknown future type survives a round trip
  as bytes rather than being dropped. Two guards, both seen red, make it structurally
  impossible for a payload *field* to hold a credential: no free-form map anywhere, and
  no field named for one. Use `journal.Marshal` and not `json.Marshal` — the latter
  re-escapes HTML in whatever `MarshalJSON` returns and fills the record with `\uXXXX`
  noise.
- **`internal/parse`** — tool-call extraction over three routes (native `tool_calls`,
  fenced JSON, XML-tagged). First success wins, a route that finds its marker commits to
  it rather than falling through, and ambiguity is a typed error rather than a guess.
  Returns the route it took as a typed value; it does **not** import the journal.
- **`internal/anchor`** — the anchor format, settled by ADR-0006 §7 and versioned.
  8 hex characters of SHA-256 over a length-prefixed (previous, this, next) window,
  rendered as `<anchor> <line-number>| <content>`. `read_file` must call `anchor.Render`
  rather than formatting its own lines: derivation and rendering are one model-facing
  contract and splitting them is how the halves drift.
- **`internal/build`** — the binary's own identity, journaled on `SessionStarted`
  (ADR-0007 decisions 6 and 7). Version, commit and a machine-readable `tree_state`,
  from the Makefile's `-X` injection, falling back to `runtime/debug.ReadBuildInfo`
  and then to an explicit `unknown` — never to a plausible default, because a
  fabricated build id pools with real ones and a missing one refuses. A leaf package:
  it imports nothing from this module, which is what makes it the one exception on
  `cmd/`'s import allowlist, and `internal/arch` enforces both halves. `internal/arch`
  also checks statically that every `-X` target in the Makefile names a real string
  variable — `go build` does not report a bogus one, it silently ships the zero value.
- **`internal/provider/fixture`** — provider traffic as data, plus the loader and
  validator for it. Enough to drive a two-turn session end to end, one fixture per
  extraction route. **Every fixture in it is hand-authored and says so in the file**
  (`"origin": "hand_authored"`), because the mock provider is build step 3 and the real
  client is step 8, so there is no traffic to record yet. That is a deliberate,
  temporary violation of the test-seam rule below, and the drift risk is real: a
  synthetic body that does not match OpenRouter produces a green suite over a loop that
  fails live. Bounded, not removed, by three things — the origin marker, a validator that
  holds each fixture to itself (the SSE frames must fold back into the assembled body,
  the declared finish reason and usage must match it, the pin must be a pin), and a note
  on every wire field saying whether it was verified against OpenRouter's docs. The
  streaming tool-call delta shape is the one OpenRouter documents nowhere; the fixtures
  follow OpenAI's contract and say so. A recording (KAN-774) replaces a hand-authored
  fixture rather than joining it. The pin is real as of KAN-775 —
  `provider.order: ["parasail/bf16"]`, `quantizations: ["bf16"]`, argued from observed
  traffic in [`docs/provider-pin.md`](docs/provider-pin.md) — and the validator now
  **refuses** both the old `PROVISIONAL-` placeholder and a quantization outside the
  vocabulary OpenRouter documents, so an invented pin fails a test rather than a
  benchmark. The pin being real does not make the bodies real: they are still
  hand-authored, and the fixtures say so.
- **`internal/repo`** — turn snapshots via git shadow refs (ADR-0002 §3), write
  only; restore and fork are slice 2. `git add -A` into a throwaway
  `GIT_INDEX_FILE`, `write-tree`, `commit-tree`, `update-ref
  refs/kopicode/<session>/<turn>`. Two guards hold "never touch the user's git
  state": the environment is **verified** to redirect the index rather than assumed
  to, and the read-only path **refuses** any index-writing subcommand outright
  (`status` included — it rewrites the index while reporting). Seen red both ways.
  The exclude goes in `$GIT_COMMON_DIR/info/exclude`, not `$GIT_DIR`'s — a linked
  worktree does not read the latter, so writing there works everywhere except under
  the bench runner. Commits carry a fixed `kopicode` identity and an injected clock,
  so a snapshot never depends on the user having configured git and the same tree
  yields the same sha.
- **`internal/provider` + `internal/provider/mock`** — the wire format the loop
  exchanges with a model, and the replay provider that serves recorded traffic through
  it. **This is the primary test seam** (SLICE-1 affordance P2), so the shape it settles
  is the one every engine test inherits. The interface is one method,
  `Complete(ctx, Request) (*Stream, error)`, and it is declared in **`internal/engine`**
  because an interface belongs where it is consumed — the real client (KAN-776) will
  satisfy the same one without importing the engine. A reply arrives as a `Stream`
  pulled synchronously by its consumer: **no goroutine, no map iteration in any output
  path**, which is what a byte-identical replayed journal rests on. Replay is at the
  interface but over the fixture's **recorded SSE frames**, decoded by the same reader
  the live client will use, so keep-alive comments, argument fragments split across
  chunks and mid-stream `Ctrl-C` are all driven rather than assumed; the assembled body
  is passed through untouched as `Reply.Raw`, so the journal records what the provider
  sent rather than a re-encoding. Asking for traffic the recording does not have is
  always an error — exhausted, out of order, another arm's pin or model — and
  `Drained()` reports replies left unconsumed, because a session that ended early
  otherwise passes every assertion about the turns it did reach.
- **`cmd/kopicode/lineedit`** — the line editor behind the prompt (ADR-0004): raw
  mode via `golang.org/x/term`, history, arrows, `Ctrl-A/E/K/U`. It lives under
  `cmd/` and not `internal/` because it is presentation, and the ADR-0003 allowlist
  is not the place to make room for a misplaced package. The raw-mode transition is
  the only platform-specific code and sits behind a two-method `Terminal` seam, so
  the whole editor is driven headless by the byte sequences a terminal sends. Three
  properties are guarded rather than hoped for: escape sequences split across reads
  decode identically at **every** split point, an unrecognised sequence is consumed
  whole so `F5` cannot type `15~` into the prompt, and the terminal is handed back on
  every exit path including a panic. The non-TTY path emits no escape byte at all.
  A key constant that lands without a name, a decoder entry *and* a binding fails the
  suite — `key_internal_test.go` derives the set from the source with `go/ast` rather
  than trusting a hand-written table.
- **`bench/tasks` + `internal/corpus`** — the frozen 10-task corpus (build plan step
  15) and the loader that validates it. Data, not code: a task is a starting tree, a
  statement in the form a user would type, and an argv oracle that exits non-zero
  before the fix and zero after. Both directions are checked for every task under the
  `integration` tag, which caught three tasks whose suites could not pass even with the
  correct fix. Tasks are discovered by walking the directory, so an eleventh cannot be
  silently unvalidated, and `Load` **refuses** a corpus whose contents no longer match
  the digest in `corpus.json` — ADR-0005's experiment-series boundary made checkable
  rather than conventional. Go tasks carry their own `go.mod`, which keeps them out of
  `go list ./...` and therefore out of every root gate; reference fixes live in
  `bench/_solutions/`, outside the corpus tree and behind a `_` the Go tool ignores.

What does **not** exist: the engine loop (`internal/engine` holds the provider interface
and nothing else), the tools, `FileJournal` and blob spill, the real OpenRouter client,
the REPL itself, the bench runner. `cmd/kopicode` and
`cmd/kopibench` are stubs that exit **4** rather than 0 — an unimplemented binary exiting
cleanly is how a broken harness passes a smoke test.

Build order is [`docs/SLICE-1.md`](docs/SLICE-1.md) §Build Plan. Step 1 is done, and
steps 2–6 are partially landed as listed above.

Do not add a test count here. It goes stale on the next PR. `make test` prints the real
number, and a red suite — not a changed count — is the signal something is wrong.

## Decisions of record — read before proposing anything

Seven ADRs bind, and two of them reverse earlier plans that still appear in older
project notes. If a document contradicts an ADR, the ADR wins.

| | |
|---|---|
| [0001](docs/adr/0001-go-implementation-language.md) | **Go**, not Python or TypeScript. Distribution, compile-time safety, process control. Rust rejected. |
| [0002](docs/adr/0002-no-durable-runtime-own-journal.md) | **No durable-execution runtime.** Satay is out: a journal records the agent's decisions, not the repo, so a replayed fork rewinds the conversation over a working tree that never moved. kopicode owns a typed append-only event log; **git** versions the tree via shadow refs. |
| [0003](docs/adr/0003-single-repo-internal-engine.md) | **One repo.** The engine is `internal/`, not a library. `kopi-engine` is retired. |
| [0004](docs/adr/0004-line-oriented-repl.md) | **Line-oriented REPL.** No alt-screen, no TUI framework. |
| [0005](docs/adr/0005-benchmark-and-ab-methodology.md) | **Paired McNemar**, pinned providers, mock/replay provider, early stopping, task pruning. No add-on catalogue yet. |
| [0006](docs/adr/0006-hash-anchored-edits-and-failure-attribution.md) | **Hash-anchored edits** — the model never reproduces file content; anchor drift is a hard rejection. Three-bucket failure attribution so a harness bug can't be laundered into a `model` number. Post-edit syntax gate. No AST editing. |
| [0007](docs/adr/0007-model-selection-and-harness-config-shape.md) | **One binary, every supported model.** `--model` > `.kopicode/config.toml` > default; **no env var**; an unknown id is a startup usage error (exit 2), never a provider error. Harness config is a named in-binary value resolved from the model id — slice 1 registers one. A bench **arm** is (model × harness config × provider pin), sampling inside the harness config. |

Satay's natural consumer in this suite is **sotong** (unattended, triggered,
credential-holding, not started), not kopicode. Do not reintroduce it here.

## Commands

Prefer the `make` targets — the pre-push hook and CI both invoke them rather than
open-coding the commands, so a gate's *definition* cannot drift between them. They do
not run the same **set**: the hook is deliberately the cheap subset. See below.
`make help` lists everything.

```bash
make dev          # go mod download + tool install
make fmt          # gofmt -w ./...
make lint         # golangci-lint run  (staticcheck is one of its linters — not a separate tool)
make vet          # go vet ./...
make check        # gofmt -l (fails on any unformatted file) + vet + lint
make test         # go test -short -race -count=1 ./...  — the fast inner loop, mock provider only
make test-all     # the FULL suite as CI runs it: -race, integration build tag, e2e git fixtures
make xbuild       # cross-compile every GOOS/GOARCH target — catches platform-specific code early
make bench-smoke  # the 10-task corpus against the MOCK provider — zero tokens
make bench        # the corpus against the real pinned provider — COSTS MONEY
make secrets      # gitleaks over history + tree
make ci           # check + test-all + xbuild
make install-hooks
```

**What actually runs where, because three agents have now reasoned from a wrong version
of this.** `make ci` is `check test-all xbuild` — `bench-smoke` is **not** in it, and
must not be until `cmd/kopibench` stops being the stub that exits 4, or `ci` is red by
construction (KAN-801 enables it, blocked on KAN-796). The **pre-push hook** runs
`check`, `test` and `secrets` — not `test-all`, not `xbuild`, not `bench-smoke` — by
design, because the hook exists to catch cheap mistakes and CI exists to catch the rest.
So a green pre-push does not predict a green CI, and `--no-verify` is not the way to get
past a target the hook never runs.

**Go-specific gates that are not optional here.** `-race` on every test run, because the
loop is concurrent (streaming, tool dispatch, cancellation) and a data race in an agent
loop is exactly the kind of bug that reproduces once a month. `-count=1` because Go
**caches test results** and a cached pass looks identical to a real one. `gofmt -l` as a
hard failure, not a suggestion. `make xbuild` because ADR-0001's distribution promise is
cross-compilation, and `flock`/process-group code is where that breaks first.

Toolchain: Go **1.26.5** at `~/.local/go`, with `golangci-lint`, `gopls` and `gitleaks`
in `~/go/bin`. Both are on `PATH` via `~/.bashrc`.

**`make bench` spends real money.** It is never in `ci` and never in the hook. Roughly
$2 per full corpus run at current `qwen/qwen3-coder-next` pricing — the pinned endpoint's
prices were re-checked on 2026-08-14 and are unchanged ($0.12/$0.80 per million tokens),
so the figure still rests on the numbers it was written against. It is an estimate and not
a measurement: no bench run has happened, and the first one's reported usage replaces it.
See [`docs/provider-pin.md`](docs/provider-pin.md). `make bench-smoke` is the free
equivalent and is what gates a PR.

`OPENROUTER_API_KEY` is read from the environment. It must never appear in the journal,
in a blob, in a log line, or in a test fixture.

## Workflow conventions

- **`main` is protected — PR-only, never push to `main`.**
- **Branch per slice:** `git switch -c feat/<slice>` off `origin/main`; open a PR.
- Run `make ci` locally before pushing. The pre-push hook runs a **cheaper subset**
  (`check`, `test`, `secrets`), so passing it is not evidence CI will pass.
- Commit trailer: `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- **Strict branch protection serialises landings.** Every merge puts the other open PRs
  out of date, so each one needs `gh pr update-branch <n>` and a full re-run before it
  can merge. Wait for all six checks to be *created and resolved*, not merely
  "not pending" — a loop that only greps for `pending` exits immediately in the window
  after `update-branch` when the checks do not exist yet.

### Worktrees: lease them, do not sweep them

Parallel agents get a worktree each. **Manage their lifecycle with `treehouse`** rather
than hand-rolled `git worktree add` plus a cleanup sweep, which is the combination that
eventually deletes a worktree an agent is still working in.

```bash
treehouse get --lease --lease-holder agent-<id>   # prints the path; never handed out twice
treehouse status --json                            # what is live — read this before cleaning
treehouse return <path>                            # release when the agent is done
treehouse prune                                    # dry run by default; --yes to act
treehouse destroy <path>                           # dry run by default; skips risky classes
```

A leased worktree is never handed out by a later `get` and never removed by `prune`, even
with no process running in it, until it is returned. `prune` treats a worktree as stale
only when it is unleased, idle, clean, and already merged into the default branch.
`destroy` removes only the disposable set unless you opt in per risk class
(`--include-unlanded`, `--include-in-use`, `--include-leased`) and refuses a global sweep.

**What this is and is not.** It is a *lifecycle* tool. It is **not** an isolation
boundary and not a defence against the corruption this repo has already suffered: a
leased worktree is an ordinary git worktree, sharing `.git/config`, the object store, the
ref store and the stash with the parent. The defences against *that* are unchanged and
not superseded — every git subprocess names its `Dir` **and** builds its own `Env`,
`internal/arch/subprocess_test.go` enforces it statically, and fixtures assert isolation
before use. Read
[`.claude/skills/agent-ground-rules/SKILL.md`](.claude/skills/agent-ground-rules/SKILL.md).

**Housekeeping rules that hold whatever created the worktree.** Clean up only when
nothing is running, and remove completed agents' worktrees **by path** rather than
sweeping a glob. After removing any, run `golangci-lint cache clean`: the lint cache is
keyed on package *content*, not path, so a deleted sibling checkout leaves entries that
resurface as findings against paths that no longer exist. `make lint` now sets a
per-checkout `GOLANGCI_LINT_CACHE`, which prevents new collisions but does not clean up
old ones.

**One honest gap.** When an agent runs under the Claude Code harness with
`isolation: "worktree"`, the harness creates the worktree itself under
`.claude/worktrees/` and `treehouse` does not manage it — `treehouse status` will not
show it and `treehouse prune` will not reclaim it. Using treehouse properly means
briefing the agent to `treehouse get --lease` and work there instead. Until that is the
default, expect harness worktrees to need the manual path above.

## Module map

```
cmd/kopicode/        REPL surface — main package
  lineedit/          raw-mode line editing for the prompt: history, arrows, Ctrl-A/E/K/U
cmd/kopibench/       headless bench runner — main package
internal/
  build/             the binary's identity: version, commit, dirty bit
  engine/            agent loop, turn state, context assembly; the Provider interface
  provider/          the wire format, SSE streaming, and the OpenRouter client
  provider/fixture/  recorded (today: hand-authored) provider traffic + its loader
  provider/mock/     the replay provider — the primary test seam
  parse/             tool-call extraction and repair
  tools/             read, write, edit, list, grep, shell
  journal/           Journal interface, FileJournal, blob spill, event types
  repo/              git shadow refs, worktrees, diff rendering
  permission/        policy decisions
  bench/             runner, oracle execution, McNemar scoring
bench/tasks/         the frozen task corpus (data, not code)
docs/adr/            decisions of record
docs/SLICE-1.md      the current slice
```

`bench/` in ADR-0003's sketch is split here into `cmd/kopibench/` plus
`internal/bench/`, following Go's convention that main packages live under `cmd/`. The
boundary the ADR cares about — two front ends, one engine, neither reaching past the
engine's interface — is unchanged.

## Boundaries that must not be crossed

These are the product's structural promises. Hold them.

- **`internal/` is the engine boundary.** Nothing outside this module imports it. The
  compiler enforces it; do not add a `pkg/` escape hatch, and do not promote
  `internal/engine` to a published module without an ADR.
- **Both front ends talk to the engine through its interface only.** An import-hygiene
  test asserts `cmd/*` never reaches into engine internals. Keep it green — it is the
  only thing holding a boundary that has no network or module edge to enforce it. The
  allowlist has exactly one non-engine entry, `internal/build`, because `--version` is a
  surface concern and that package holds no behaviour; it is allowed only for as long as
  it stays a leaf, which a second test enforces. Do not add a third entry without an ADR.
- **One session record, and it is the journal.** Do not build a parallel transcript.
  Everything the REPL prints and everything `--print` emits is **derived** from journal
  events. This is the lesson from sibei-flow
  [ADR-0013](https://github.com/leejianrong/sibei-flow/blob/main/docs/design/adr/0013-transcript-tagged-union.md):
  a hand-rolled `list[str]` clipped at 1200 characters was lossy by construction and
  drifted from reality the first time someone added a tool call and forgot the append.
- **`slog` never duplicates the journal.** The journal owns session content (messages,
  tool calls, results, edits, permissions). `slog` owns engine-internal diagnostics only
  — startup, config resolution, provider connection, lock acquisition, worktree
  lifecycle — off by default, `--debug`, to **stderr** because `--print` owns stdout. A
  logger that records session facts is the parallel transcript, arriving disguised as
  observability. If a fact belongs in both, it belongs in the journal only.
- **The fixture recorder scrubs secrets at write time**, via a header **allowlist** and
  not a denylist. Recorded provider traffic is real HTTP, and real headers carry the API
  key; scrubbing on read means the key already sits in a file someone can commit.
- **Never truncate tool output.** Payloads over 64 KiB spill to content-addressed blobs.
  Clipping the diagnostic output that justifies a fix, exactly where a reviewer looks,
  is the specific failure being designed out.
- **Events are a compatibility surface from the first commit.** Version them, and make
  the unmarshaller preserve unknown types rather than dropping them.
- **Edits fail closed.** An `edit_file` whose anchors no longer match the file is
  **rejected**, never applied somewhere plausible. Fuzzy matching is a fallback only,
  and using it marks the session `unattributed` in the bench classifier. Never add an
  edit path that can apply to the wrong region without emitting a signal — that is the
  specific defect ADR-0006 exists to prevent, and it corrupts the project's core metric
  rather than merely breaking a file.
- **Shell out, do not link.** Language tooling — the syntax gate's `gofmt`/`node
  --check`/`py_compile`, git, diff rendering — is invoked as a subprocess. No CGo. It
  costs cross-compilation without a C toolchain and breaks `go install` for users
  without a compiler, which is the distribution promise ADR-0001 rests on.
- **Dependencies stay near-zero.** stdlib, plus `golang.org/x/term` for line editing and
  `github.com/google/go-cmp` in tests. Git is shelled out to, not linked. Adding a
  dependency needs a reason in the PR description. **Never add a diff library** — but
  which of the two ways to render a diff you pick depends on where it goes:
  - A **tree diff for a human to read** (turn snapshots, `internal/repo`) shells out to
    `git diff --no-index`. Zero deps, exact fidelity, and nothing downstream depends on
    the bytes being stable.
  - A **diff that gets journaled** is built in-process, and `internal/tools/edit.go` is
    the worked example. SLICE-1 requires a replayed session to produce a byte-identical
    journal, and `git diff` output varies with the installed git version and the user's
    config (`diff.algorithm`, external drivers), so shelling out would make the criterion
    fail on someone else's machine. An anchored edit replaces one contiguous region, so
    there is no diff *algorithm* to run and the in-process version is exact by
    construction rather than approximated.
- **The engine decides policy; the surfaces decide presentation.** The engine decides
  *that* permission is required and what a decision means, never how it is asked.
- **Never touch the user's git state.** Shadow refs are written under
  `refs/kopicode/`, through a throwaway `GIT_INDEX_FILE`. The user's branch, HEAD, index
  and stashes are off limits, and `.kopicode/` goes in `.git/info/exclude`, never
  `.gitignore`.
- **Every subprocess names the directory it runs in *and* builds its own environment,
  in tests as well as in product code.** `Dir` alone is not enough: `GIT_DIR` overrides
  the working directory entirely, so an inherited one redirects a command that looks
  perfectly targeted. That is not hypothetical. It has already set `core.bare = true`
  in the real `.git/config`, and left a stash that would have deleted every tracked
  file. A git worktree shares config, objects and refs with its parent, so being "in a
  worktree" protects nothing here. Fixtures assert their git dir is under
  `t.TempDir()` before use, and `TestMain` fingerprints the ambient repo either side of
  the run. `internal/repo` is the worked example;
  [`.claude/skills/agent-ground-rules/SKILL.md`](.claude/skills/agent-ground-rules/SKILL.md)
  has the reproductions.

  **The rule is not git-specific and neither is the guard** (KAN-837). An inherited
  environment changes what any subprocess does while the command still reads correctly:
  `GOFLAGS`/`GOOS` change what `go build` produces, `PYTHONPATH`/`VIRTUAL_ENV` change
  what `python` imports, `NODE_OPTIONS` changes what `node` runs, and `PATH` decides
  which binary runs at all — and the syntax gate spawns all three.
  `internal/arch/subprocess_test.go` holds every `exec.Command`/`exec.CommandContext` in
  the tree to both halves, whatever it runs, and fails closed on a `Cmd` it cannot
  follow. The git half was **not** relaxed to fit the general case: the rule is
  identical and only the failure message differs. Sites where the inherited directory or
  environment genuinely is the right one carry `//kopicode:allow-nodir: <reason>` or
  `//kopicode:allow-noenv: <reason>` in place. The two are independent, a waiver with no
  reason waives nothing, and a comment that only looks like a waiver fails the suite.
- **Pin the provider on every benchmark request.** `provider.order`,
  `allow_fallbacks: false`, fixed `quantizations`, all recorded per result. An unpinned
  A/B number is not evidence.

## Test seam

The **primary seam is the engine's public interface, driven by the mock/replay provider
against a temp git-repo fixture**, with an injected clock and a seeded RNG. That is what
makes the loop testable at zero token cost and deterministic given fixed provider
output.

Assert **observable outcomes** — journal events, the resulting tree, exit codes,
captured stdout — never internal loop state. "Did the agent behave correctly" is a
question about the journal and the diff, not about which method got called.

The mock provider **replays recorded traffic rather than synthesising it**. If it drifts
from real provider behaviour, green plumbing tests will mask real breakage, so recorded
fixtures come from actual runs and get refreshed when the provider changes.

## Pointers

- [`docs/PRD.md`](docs/PRD.md) — what this is for, numbered requirements R1-R17, success
  measures, scope boundary, and the epic-to-requirement traceability table
- [`docs/SLICE-1.md`](docs/SLICE-1.md) — the current slice: scope, build plan,
  acceptance criteria, risks
- [`docs/adr/`](docs/adr/) — decisions of record, 0001–0007
- [`docs/provider-pin.md`](docs/provider-pin.md) — which provider and quantization every
  benchmark request pins, the observed traffic it was chosen from, and the date
- [`README.md`](README.md) — the thesis, where the harness gains are, model table
- `../agentic-harness-ideas.md` — strategy notes and the still-open questions (sandbox
  model, context management, whether cheap-A/B replay reopens durability)
