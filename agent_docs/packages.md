# Packages: what each is, and why

Read this before changing a package whose reason you do not already know. It is the
rationale behind the structure, not a tour of it: `ls internal/` and a package's own doc
comment are the map, and where this file and the code disagree, the code is right.

## What each package is, and why

**Trust the code over the docs.** `docs/` describes the intended system; where they
disagree, the code is the truth — verify before believing a docstring, including this
section, which drifts faster than the code does. `ls internal/`, `git log --oneline
-30`, and a package's own doc comment all beat a paragraph here.

**Slice 1 is complete** (verified 2026-08-17; all 18 build-plan steps landed,
including the real-repo demo and the full corpus run — see
[`docs/SLICE-1.md`](../docs/SLICE-1.md)). One engine, two front ends, one model
registered and measured end to end.

Module + toolchain: Go 1.26.5, two dependencies (`golang.org/x/term` for the line
editor, `github.com/google/go-cmp` in tests only), no CGo. `make xbuild` cross-compiles
and `go vet`s both binaries for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
and windows/amd64 with `CGO_ENABLED=0` — the ADR-0001 distribution promise, checked
per-platform rather than asserted. `internal/arch` holds three static guards red until
proven green: import hygiene (ADR-0003's allowlist), every Makefile `-X` ldflags target
naming a real string variable, and the subprocess `Dir`/`Env` rule (see
[Boundaries](../AGENTS.md#boundaries-that-must-not-be-crossed)).

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
  `edit_file_fuzzy`, `delete_file`, `run_shell`. Anchors come from `read_file` and nowhere else, so an
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
- **`internal/provider/fixture`** — provider traffic as data. Some shipped fixtures are
  recorded (`"origin": "recorded"`, `recorded_*`) and some are still hand-authored and say
  so. The recorder (KAN-774, `recorder.go`) is a `RoundTripper` that turns real traffic
  into fixture data, scrubbing secrets through a header allowlist; `kopibench run
  --record-dir DIR [--task ids]` drives it through `internal/bench/record.go`, and writes
  only a recording that passes `Validate` (a session cut off at the turn cap is refused).
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
  ADR-0010's **declared** configs also live here (`declared.go`): a TOML file naming a
  built-in `base` and overriding `max_turns`/`token_budget`/`repair_budget`/`max_tokens`,
  resolved to a full `Config` named `declared:<base>` and named by `--harness-config` or
  `harness_config =` at the same rung as `--harness`/`harness =`. It is local-only — the
  `declared:` name is in the hash preimage, so a declared arm can never pool with a
  built-in — and `kopitune`/unofficial-corpus (the rest of ADR-0010) are not built yet.
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
- **`cmd/kopicode/session`** — the protocol-independent session core (ADR-0015 decision 3):
  the registry of open sessions, one FIFO worker per session, start/submit/cancel/close
  against `engine.Open`, consent-mode resolution, shutdown. Results come back through
  callbacks and events through an `engine.Observer`, so it holds no transcript and imports
  only the engine. `serve` and `mcp` are the two skins over it; the schema-1 `record`
  projection stays in `print.go`.
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
- **`bench/tasks` + `internal/corpus`** — the frozen 13-task corpus. `Load` refuses a
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

**What doesn't exist yet:** the recorder is driven by `kopibench run --record-dir`, but only two
tasks are recorded so far; the three older shipped fixtures are still hand-authored. The
other gaps this paragraph used to list have since closed — verify against the code, not
this note: three models are registered (`internal/harness/registry.go`) and paired A/B
numbers exist (`docs/paired-ab-*`); `slog` runs inside the engine
(`internal/engine/open.go`); `.kopicode/lock` holds one session per working tree
(`internal/lock`); and `kopicode sessions` (KAN-941) lists sessions to find a
`--resume`/`--fork` id.

Do not add a test count here — it goes stale on the next PR. `make test` prints the
real number; a red suite, not a changed count, is the signal something is wrong.

## Module map

```
cmd/kopicode/        the front ends — main package: `repl`, `run --print`, `serve`, `mcp`, `sessions`, `version`
  session/           the session lifecycle `serve` and `mcp` share: registry, per-session queue, consent modes
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
