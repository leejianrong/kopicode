# CLAUDE.md — agent brief for kopicode

kopicode is a **terminal coding agent, and a harness you can tune per model**. A
line-oriented REPL and a headless benchmark runner drive one engine. The bet is that
harness quality — tool-call parse-and-repair, an edit tool that tolerates imprecision,
forced verification — moves a cheap open-weight model's measured coding ability by a
large margin, and that the rig proves it rather than asserting it.

Written in **Go**. Single static binary. No durable-execution runtime.

## Read this first if you are a sub-agent

Read
[`.claude/skills/agent-ground-rules/SKILL.md`](.claude/skills/agent-ground-rules/SKILL.md)
before running any command or writing any test. The one fact worth repeating here
because it has already cost this repo a broken checkout: **a git worktree is not a
separate repository.** It shares `.git/config`, the object store and the ref store
with the repo it came from — only `HEAD` and the index are private — so a git
subprocess with no working directory set runs in the *real* repository. The skill has
the rules that prevent it and the checks that catch it.

**At most two subagents work in this repository at once.** Lease a `treehouse`
worktree each — see [Worktrees](#worktrees-lease-them-do-not-sweep-them) below — and
do not exceed two even if more work is queued.

## Build status

**Trust the code over the docs.** `docs/` describes the intended system; where they
disagree, the code is the truth — verify before believing a docstring, including this
section, which drifts faster than the code does. `ls internal/`, `git log --oneline
-30`, and a package's own doc comment all beat a paragraph here.

**Slice 1 is complete** (verified 2026-08-17; all 18 build-plan steps landed,
including the real-repo demo and the full corpus run — see
[`docs/SLICE-1.md`](docs/SLICE-1.md)). One engine, two front ends, one model
registered and measured end to end.

Module + toolchain: Go 1.26.5, two dependencies (`golang.org/x/term` for the line
editor, `github.com/google/go-cmp` in tests only), no CGo. `make xbuild` cross-compiles
and `go vet`s both binaries for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
and windows/amd64 with `CGO_ENABLED=0` — the ADR-0001 distribution promise, checked
per-platform rather than asserted. `internal/arch` holds three static guards red until
proven green: import hygiene (ADR-0003's allowlist), every Makefile `-X` ldflags target
naming a real string variable, and the subprocess `Dir`/`Env` rule (see
[Boundaries](#boundaries-that-must-not-be-crossed)).

What each package is, in one or two lines — read the package's own doc comment for why,
not this list:

- **`internal/journal`** — the tagged-union event log. JSONL under
  `.kopicode/sessions/<id>/`, mode 0600. A `Text` field over 64 KiB spills to a
  content-addressed blob rather than being clipped; a redactor strips known secret
  *values* at append time. Use `journal.Marshal`, never `json.Marshal` (the latter
  re-escapes HTML into `\uXXXX` noise). No map anywhere in a payload type, and no field
  named for a credential — both enforced, not just documented.
- **`internal/parse`** — tool-call extraction (native `tool_calls`, fenced JSON,
  XML-tagged; first success wins) and two-attempt repair, each failure carrying a
  `Kind` that becomes a specific message back to the model. Does not import the
  journal — the engine journals from what `parse` returns.
- **`internal/anchor`** — the anchor format (ADR-0006 §7), settled and versioned: 8 hex
  characters of SHA-256 over a length-prefixed (previous, this, next) line window,
  rendered `<anchor> <line-number>| <content>`. A version bump invalidates every
  fixture and opens a new experiment series.
- **`internal/build`** — the binary's own identity (version, commit, `tree_state`) from
  the Makefile's `-X` injection, falling back to `runtime/debug.ReadBuildInfo` and then
  to an explicit `unknown` — never a plausible default, since a fabricated build id
  would pool with real ones. A leaf package; the one exception on `cmd/`'s import
  allowlist.
- **`internal/tools`** — `read_file`, `list_dir`, `grep`, `write_file`, `edit_file`,
  `edit_file_fuzzy`, `run_shell`. Anchors come from `read_file` and nowhere else, so an
  edit into a region the model was never shown is structurally impossible. Bounds are
  declared, never silent. Every path goes through `Root.Resolve` (symlinks resolved
  before cleaning, then `os.Root` refuses traversal at the syscall level).
  `edit_file` re-derives every anchor from disk before writing and **rejects** on
  mismatch with a typed reason (`anchor_malformed` / `anchor_drift` / `anchor_ambiguous`);
  `edit_file_fuzzy` is a separate tool, fallback-only, and every use marks the session
  `unattributed` in the bench classifier. `run_shell` is the one tool with no
  containment claim — its working directory is resolved like any other path, but a
  shell goes where it likes, which is why the permission gate exists.
- **`internal/syntax`** — the post-edit gate: a language-native check (`gofmt -e`,
  `node --check`, `py_compile`, …) run immediately after an edit. `NotRun` is the zero
  value of `Outcome` — a gate that didn't run must never read as one that passed — and
  a verdict-step failure right after an edit charges the `harness` bucket directly.
- **`internal/procgroup`** — kills a subprocess by signalling its whole process group
  (negative pid), graceful then forceful, so nothing is reparented to init and left
  running.
- **`internal/permission`** — the policy half of consent, nothing else: no prompt
  rendering, no terminal awareness. Fails closed twice over (the zero `Outcome` denies;
  every non-allow wraps `ErrDenied`). `allow_session` is exact-match only — no prefix or
  directory grant, deliberately.
- **`internal/verify`** — forced verification: the project's own command, run after any
  turn that could have changed the tree. `NotRun` is the zero value, not `Passed`; only
  a command that ran and failed blocks a success report. Discovery **executes nothing**
  — a Makefile target, `go.mod`, `scripts.test`, a uv project, in that order.
- **`internal/provider/fixture`** — provider traffic as data. Every fixture is
  hand-authored and says so (`"origin": "hand_authored"`) — the recorder that would
  scrub secrets from real traffic doesn't exist yet, so this is a deliberate, bounded
  violation of the test-seam rule below, not an oversight.
- **`internal/repo`** — turn snapshots via git shadow refs
  (`refs/kopicode/<session>/<turn>`), written through a throwaway index so the user's
  real git state is never touched. `Restore` reads a tree back out via `git archive`
  without deleting files the target tree doesn't mention — turning that into an exact
  checkout is a bigger promise than "read a tree back safely" and is left to the
  caller.
- **`internal/lock`** — one session per **working tree** (`repo.WorkTreeRoot`, not the
  git directory — a linked worktree shares the latter with its parent, which would
  serialise the bench runner's parallel task worktrees). `flock`-based, no staleness
  heuristic needed: the kernel releases the lock on any exit, including a crash.
- **`internal/provider` + `internal/provider/mock`** — the wire format and the primary
  test seam. `Complete(ctx, Request) (*Stream, error)` is one method, declared in
  `internal/engine` where it's consumed. Replay drives the fixture's recorded SSE
  frames through the same reader the live client uses — no goroutines, no map
  iteration anywhere in the output path, which is what a byte-identical replayed
  journal rests on.
- **`internal/harness`** — ADR-0007 made mechanical: the model-id → (harness config,
  provider pin) registry, the config's hash, and `.kopicode/config.toml`'s precedence
  (`--model`/`--harness` > repo config > built-in default — **no environment variable
  anywhere in the chain**). `Config` holds no map anywhere in its type graph, because
  Go randomises map iteration and a replayed journal must be byte-identical.
- **The system prompt** is a harness *value*, not a loop detail — `go:embed`ed,
  in the hash preimage by digest, held to an 8 KiB budget, and tested to document
  every tool and every argument the harness config actually carries.
- **The tool catalogue** reaches the wire in OpenAI's documented shape
  (`tools: [{type:"function", function:{...}}]`), rendered from the same struct tags
  the system prompt is tested against, and is itself in the hash preimage. Whether it's
  advertised at all is `Config.AdvertiseNativeTools` — the harness offers three
  extraction routes precisely because not every model reliably uses the native one.
- **The live OpenRouter client** — one POST, `stream=true`, decoded by the same SSE
  reader the replay provider drives. The provider pin is an input, written to the wire
  verbatim, never a client-owned copy. Capped exponential backoff with full jitter on
  429/5xx/transport failures only. `APIKey` redacts itself everywhere a credential
  could otherwise leak (`String`, `MarshalJSON`, `LogValue`, …), proven absent from the
  journal and logs by a dedicated leak test.
- **`internal/engine`** — the bounded turn loop: prompt → provider → parse → dispatch →
  observe → repeat. **A single turn tree — no subagents, no planner, no second loop.**
  Every bound (turn cap, token budget, repair attempts) comes off the harness config,
  never a package constant, so two runs differing in a bound compare as different arms.
  Context assembly is naive full history, a decision made deliberately rather than a
  gap — a compaction strategy chosen without session data would be a guess.
  `TurnCancelled` records what was *in flight* when a turn was cancelled
  (`provider_stream` / `tool_call` / `verification` / `between_steps`), so a cancelled
  session is distinguishable from a genuinely stuck one.
- **`engine.Open`** — the presentation-neutral entry point a front end actually uses.
  `Options` deliberately does not accept the two permission gates, the tool set, or the
  journal as arguments — a surface able to substitute one could disable it by
  forgetting a field. `Options.Events` streams one `engine.Event` per journal event,
  appended first and announced only once accepted.
- **`--resume <id>`** reopens a session's journal, rehydrates blob-spilled fields, and
  replays history into a fresh context assembler — but does **not** restore the
  working tree. That's deliberate: an interrupted process's tree is usually already
  where it was left, and silently rewinding one the user has since touched by hand
  would destroy work with no consent asked. `Repo.Restore` is available as an explicit
  separate step for a caller that wants the tree rewound first.
- **`engine.Fork`** is the opposite case: branching a brand-new session from a copy of
  another one's history *does* restore the working tree automatically, because trying
  a different continuation from turn N requires the tree to actually be at turn N's
  state. The source session's history is copied into the fork's own journal (not
  referenced), so the forked session's record is self-contained.
- **`cmd/kopicode/lineedit`** — the raw-mode line editor behind the prompt, driven
  headless through a two-method `Terminal` seam so escape-sequence decoding is testable
  without a real tty. The non-TTY path emits no escape byte at all.
- **`cmd/kopicode`** — two surfaces over one engine. The REPL streams model text as an
  optimistic prefix of the record, then reconciles against the journal's actual
  `AssistantMessage` and prints any divergence rather than hiding it. `run --print` is
  the headless surface: newline-delimited JSON, schema 1, five exit codes (`0` success,
  `1` task not completed, `2` usage, `3` provider, `4` harness / unmapped default),
  accumulating no session state so it cannot print anything the journal doesn't hold.
- **`internal/bench` + `cmd/kopibench run`** — the headless benchmark, a first-class
  front end. One invocation is one arm over the frozen corpus; a paired A/B is two
  invocations of one binary. Every task gets its own git worktree, reclaimed and
  reported (not silently swept). **Not a security boundary** — model-authored shell
  runs in a worktree, not a container.
- **Three-bucket failure attribution** — derived from journal events, never judged.
  Priority order when a session trips more than one rule: nothing-to-attribute, then
  `harness`, then `unattributed`, then `model` — `harness` outranks `unattributed`
  deliberately, because letting ambiguity swallow a known harness failure would move it
  out of the one bucket with a zero acceptance bar.
- **`bench/tasks` + `internal/corpus`** — the frozen 10-task corpus. `Load` refuses a
  corpus whose contents don't match its digest, and every task's oracle is checked in
  both directions (fails before the fix, passes after).

**First real numbers** (KAN-799/800, 2026-08-18): a real-repo demo landed a correct
one-line fix for ~$0.04, with `Ctrl-C` proven to cancel cleanly mid-stream. The frozen
corpus scored **9/10** against the pinned `qwen/qwen3-coder-next` arm for **$0.0846**
total. The one failure was traced through the journal to a model self-correction
limitation (it copied `read_file`'s anchor-decorated display verbatim into an edit,
then couldn't see the resulting syntax error even after reading the file back) — every
kopicode mechanism behaved as designed, and the classifier still bucketed it `harness`
per its deliberately conservative rule. This is the harness's first honest number, not
a flattering one.

**What doesn't exist yet:** the fixture recorder, so every provider fixture stays
hand-authored; a second registered model, so the benchmark rig has never produced an
A/B result; `slog` inside the engine (wired at the front-end level only); the
`.kopicode/lock` advisory lock (nothing stops two sessions in one repository today).
The CLI surface for `--resume`/`--fork` (listing sessions worth resuming from) doesn't
exist either — the engine-level mechanism does.

Do not add a test count here — it goes stale on the next PR. `make test` prints the
real number; a red suite, not a changed count, is the signal something is wrong.

## Decisions of record — read before proposing anything

Eleven ADRs exist; two reverse earlier plans that still appear in older project notes,
and two are amendments layered on top of earlier ones. If a document contradicts an
ADR, the ADR wins.

| | |
|---|---|
| [0001](docs/adr/0001-go-implementation-language.md) | **Go**, not Python or TypeScript. Distribution, compile-time safety, process control. |
| [0002](docs/adr/0002-no-durable-runtime-own-journal.md) | **No durable-execution runtime.** kopicode owns a typed event log; **git** versions the tree via shadow refs. |
| [0003](docs/adr/0003-single-repo-internal-engine.md) | **One repo.** The engine is `internal/`, not a library. |
| [0004](docs/adr/0004-line-oriented-repl.md) | **Line-oriented REPL.** No alt-screen, no TUI framework. |
| [0005](docs/adr/0005-benchmark-and-ab-methodology.md) | **Paired McNemar**, pinned providers, mock/replay provider, early stopping, task pruning. No add-on catalogue yet. |
| [0006](docs/adr/0006-hash-anchored-edits-and-failure-attribution.md) | **Hash-anchored edits** — anchor drift is a hard rejection. Three-bucket failure attribution. Post-edit syntax gate. No AST editing. |
| [0007](docs/adr/0007-model-selection-and-harness-config-shape.md) | **One binary, every supported model.** Harness config is a named in-binary value resolved from the model id. A bench **arm** is (model × harness config × provider pin). |
| [0008](docs/adr/0008-shell-isolation-accepted-risk.md) *(Proposed)* | **Model-authored shell isolation is an accepted risk, not a sandbox.** Trust model: one developer, their own machine. Revisit when that stops being true — see 0011. |
| [0009](docs/adr/0009-ask-tool-contract.md) *(Proposed)* | **The `ask` tool** is a sibling mechanism to consent, not an extension of `internal/permission` — free-text question/answer, never a `Verdict`. |
| [0010](docs/adr/0010-declarative-harness-configs-and-self-tuning.md) | **Declarative harness configs + `kopitune`.** Amends 0007: a second, *declared* config class (TOML, base + overrides) alongside the built-in registry, for models with no hand-tuned entry. Local-only — never anchors a published benchmark number. |
| [0011](docs/adr/0011-unattended-invocation-policy-gate.md) | **A policy gate for unattended invocation.** Amends 0008: a new opt-in `permission.Policy` (declared allowlist) for a caller like cuttlefish that spawns kopicode with no human present. Real containment is the caller's job, not kopicode's — kopicode gains no sandbox dependency from this. |

Satay's natural consumer in this suite is **cuttlefish** (unattended, triggered,
credential-holding, not started), not kopicode. Do not reintroduce it here.

## Commands

Prefer the `make` targets — the pre-push hook and CI both invoke them, so a gate's
*definition* cannot drift between them. They do not run the same **set**: the hook is
the cheap subset. `make help` lists everything.

```bash
make dev          # go mod download + tool install
make fmt          # gofmt -w over the package directories `go list` reports
make fmtcheck     # the same set, read-only; fails on any unformatted file
make lint         # golangci-lint run  (staticcheck is one of its linters, not separate)
make vet          # go vet ./...
make tidycheck    # fails if go.mod/go.sum are not tidy, without leaving them changed
make check        # fmtcheck + vet + lint + tidycheck — every cheap static gate
make test         # go test -short -race -count=1 ./...  — fast inner loop, mock provider only
make test-all     # the FULL suite as CI runs it: -race, integration build tag, e2e git fixtures
make xbuild       # cross-compile AND vet every GOOS/GOARCH target
make bench-smoke  # the 10-task corpus against the MOCK provider — zero tokens, required in CI
make bench        # the corpus against the real pinned provider — COSTS MONEY (~$0.08 observed)
make smoke-live   # ONE 16-token request against the pinned endpoint — costs cents, behind `live`
make secrets      # gitleaks over history + tree
make vuln         # govulncheck over the module
make ci           # check + test-all + bench-smoke + xbuild
make install-hooks
```

Notes worth knowing before they cost you time:

- **`fmt`/`fmtcheck` are fed `go list` output, not `.`** — gofmt walks directories
  literally (including `.`-prefixed ones `go build`/`go vet`/lint skip), so on a
  machine running parallel agents, another agent's sibling checkout under
  `.claude/worktrees/` can fail *your* pre-push hook otherwise.
- **The pre-push hook (`check`, `test`, `secrets`) is not a CI predictor.** CI also
  runs `test-all`, `xbuild`, and `bench-smoke` (a required check since KAN-801) — a
  green pre-push does not mean CI will be green, and `--no-verify` doesn't fix that.
- **`-race` and `-count=1` are not optional.** The loop is concurrent (streaming, tool
  dispatch, cancellation), and Go caches test results — a cached pass looks identical
  to a real one.
- **`make bench` spends real money** (never in `ci` or the hook) — treat ~$0.08 as the
  observed cost, not a ceiling; a task that gets stuck to the turn cap costs more.
  `make bench-smoke` is the free equivalent and is what gates a PR.
- Toolchain: Go **1.26.5** at `~/.local/go`; `golangci-lint`, `gopls`, `gitleaks` in
  `~/go/bin`, both on `PATH`.
- `OPENROUTER_API_KEY` is read from the environment and must never appear in the
  journal, a blob, a log line, or a test fixture.

## Workflow conventions

- **`main` is protected — PR-only, never push to `main`.**
- **Branch per slice:** `git switch -c feat/<slice>` off `origin/main`; open a PR.
- Run `make ci` locally before pushing — the pre-push hook is a cheaper subset, so
  passing it is not evidence CI will pass.
- Commit trailer: `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- **At most two subagents work in this repository at once.**
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

## Module map

```
cmd/kopicode/        both surfaces — main package: `repl`, `run --print`, `version`
  lineedit/          raw-mode line editing for the prompt: history, arrows, Ctrl-A/E/K/U
  repl/              the interactive surface: streaming, consent prompt, Ctrl-C
cmd/kopibench/       headless bench runner — main package
internal/
  arch/              the static guards: import hygiene, ldflags targets, subprocess Dir/Env
  build/             the binary's identity: version, commit, dirty bit
  engine/            the turn loop, context assembly, dispatch, Open, the event stream
  harness/           the registry, the harness config and its hash, the system prompt
  provider/          the wire format, SSE streaming, and the OpenRouter client
  provider/fixture/  recorded (today: hand-authored) provider traffic + its loader
  provider/mock/     the replay provider — the primary test seam
  parse/             tool-call extraction and repair
  anchor/            the anchor format — derivation and rendering, kept together
  tools/             read, list, grep, write, edit, edit-fuzzy, shell
  syntax/            the post-edit language gate
  verify/            forced verification: discovery and the run
  procgroup/         start a subprocess in its own group, end the whole group
  journal/           Journal interface, FileJournal, blob spill, redaction, event types
  repo/              git shadow refs — write, restore, resume and fork
  lock/              the advisory lock at .kopicode/lock — one session per working tree
  permission/        policy decisions
  corpus/            the loader, validator and digest for bench/tasks
  bench/             runner, worktrees, oracle execution, attribution, McNemar scoring
bench/tasks/         the frozen task corpus (data, not code)
bench/_solutions/    reference fixes, outside the corpus tree and behind a `_`
docs/adr/            decisions of record
docs/SLICE-1.md      the current slice
```

`bench/` in ADR-0003's original sketch is split here into `cmd/kopibench/` plus
`internal/bench/`, following Go's convention that main packages live under `cmd/`. The
boundary the ADR cares about — two front ends, one engine, neither reaching past the
engine's interface — is unchanged.

## Boundaries that must not be crossed

These are the product's structural promises. Hold them.

- **`internal/` is the engine boundary.** Nothing outside this module imports it. Do
  not add a `pkg/` escape hatch, and do not promote `internal/engine` to a published
  module without an ADR.
- **Both front ends talk to the engine through its interface only.** The import-hygiene
  allowlist has three entries — `internal/engine`, `internal/bench`, and
  `internal/build` (a leaf, allowed only for `--version`). Do not add a fourth without
  an ADR; `engine.Open` exists precisely so a front end doesn't need one.
- **One session record, and it is the journal.** Do not build a parallel transcript.
  Everything either surface prints is **derived** from journal events — the lesson from
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
doesn't mask real breakage by drifting from actual provider behaviour. Today this
comes with one open exception: the fixture recorder doesn't exist yet, so every shipped
fixture is hand-authored rather than recorded from a real run.

## Pointers

- [`docs/PRD.md`](docs/PRD.md) — what this is for, numbered requirements R1-R17,
  success measures, scope boundary
- [`docs/SLICE-1.md`](docs/SLICE-1.md) — the current slice: scope, build plan,
  acceptance criteria, risks
- [`docs/adr/`](docs/adr/) — decisions of record, 0001–0011
- [`docs/provider-pin.md`](docs/provider-pin.md) — which provider and quantization
  every benchmark request pins, and why
- [`docs/token-growth.md`](docs/token-growth.md) — real per-turn context growth from
  two dogfood sessions (KAN-935/947); what it does and doesn't say about compaction
- [`README.md`](README.md) — the thesis, where the harness gains are, the model table
- `../agentic-harness-ideas.md` — pre-ADR strategy notes; superseded where an ADR
  disagrees, still holds the open questions ADRs haven't resolved yet
