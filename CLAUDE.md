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

## Build status — slice 1 is all but whole

**Trust the code over the docs.** `docs/` describes the intended system; where they
disagree, the code is the truth for what exists today — verify before you believe a
docstring, including this section. The paragraph after the list is this section's own
drift record, and it is there because the instruction above keeps being needed.

What is real now (2026-08-17):

- **Module + toolchain.** Go 1.26.5. `Makefile` with the gates below, `.golangci.yml`
  (v2 schema, validated), `.github/workflows/ci.yml` with seven parallel jobs, and a
  pre-push hook mirroring the cheap ones. Two dependencies, both sanctioned in advance:
  `github.com/google/go-cmp` in tests only, and `golang.org/x/term` for the line
  editor (ADR-0004 decision 2). Pure Go, no CGo, and `golang.org/x/sys` comes with
  it as an indirect.
- **Cross-compilation proven.** `make xbuild` builds both binaries for linux/amd64,
  linux/arm64, darwin/amd64, darwin/arm64 and windows/amd64 with `CGO_ENABLED=0` — the
  ADR-0001 distribution promise, checked rather than asserted. The load-bearing half is
  the `go vet ./...` it runs **per platform** first: back when `cmd/` was two stubs
  importing nothing, cross-compiling them proved nothing about the code that actually
  has platform-specific behaviour, and vet typechecks `_test.go` files too — so a test
  that will not build under `GOOS=windows` fails here rather than on the one machine
  that runs it.
- **Boundary guards, proven red.** `internal/arch` is three static suites over the whole
  tree, and none of them is a convention. Import hygiene enforces ADR-0003: a front end
  may import `internal/engine`, `internal/bench` and `internal/build` and nothing else,
  the engine may not import a surface, and a second test holds `internal/build` to being
  a leaf — the day it imports something, the allowlist entry stops being defensible and
  every front end inherits a path into the engine's internals without a line of `cmd/`
  changing. The ldflags check reads the Makefile and requires every `-X` target to name
  a real string variable, because `go build` does not report a bogus one, it silently
  ships the zero value. The subprocess guard is described under "Boundaries" below. Each
  failure names the file, the import or the call site, and the rule it broke — and each
  guard was broken on purpose and watched fail before it landed, which is this repo's
  standing requirement for any safety check and the reason to trust these rather than
  the comments beside them.
- **`internal/journal`** — the event tagged union, and the record behind it. Envelope
  plus 20 payload types, with explicit `MarshalJSON`/`UnmarshalJSON` so an unknown
  future type survives a round trip as bytes rather than being dropped. Two guards, both
  seen red, make it structurally impossible for a payload *field* to hold a credential:
  no free-form map anywhere, and no field named for one. Use `journal.Marshal` and not
  `json.Marshal` — the latter re-escapes HTML in whatever `MarshalJSON` returns and
  fills the record with `\uXXXX` noise. `FileJournal` writes JSONL under
  `.kopicode/sessions/<id>/`, 0600 because the record holds what the user typed and what
  the model read; any `Text` field over 64 KiB spills to a content-addressed blob rather
  than being clipped, found by one reflective traversal instead of a method per payload
  type, since the failure mode of twenty hand-written lists is a twenty-first that
  spills nothing and nobody finds out until a 4 MB line lands in the record. A redactor
  removes known secret *values* from the encoded line at append time: no type can stop a
  model from running `env` and a tool result from carrying the answer.
- **`internal/parse`** — tool-call extraction *and* repair. Extraction runs three routes
  (native `tool_calls`, fenced JSON, XML-tagged). First success wins, a route that finds
  its marker commits to it rather than falling through, and ambiguity is a typed error
  rather than a guess. Every failure carries a `Kind`, and the repair loop turns that
  into a specific message back to the model — two attempts per call, then the call fails
  and the turn continues with the failure as an observation rather than aborting the
  session. It returns the route it took as a typed value, because *which* route a model
  reaches for is a finding rather than a detail. It does **not** import the journal:
  what it owes instead is that every outcome maps onto exactly one event without the
  engine guessing, which is what makes parse-success rate and repair-recovery rate
  readable off the record rather than anecdotal. The schemas it repairs against are not
  written here either — `parse.Tools` is a two-method interface declared at the
  consumer, and the engine's catalogue satisfies it.
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
- **`internal/tools`** — `read_file`, `list_dir`, `grep`, `write_file`, `edit_file`,
  `edit_file_fuzzy` and `run_shell`, on one root and one set of limits. Three rules
  shape all of it. **Anchors come from `read_file` and nowhere else** — `grep`
  deliberately emits none, because ADR-0006 rests on an anchor being obtainable only
  from a read, which is what makes an edit into a region the model was never shown
  structurally impossible rather than merely discouraged; a test holds that across the
  whole set, so a tool added later cannot leak one by accident. **Bounds are declared,
  never silent**: where a bound is unavoidable the tool either refuses outright with a
  message naming the size, or returns a window and *says in its output* that it did,
  with the argument that fetches the rest. **Every path argument goes through
  `Root.Resolve`**, which resolves symlinks *before* cleaning the string — `foo/../bar`
  is only lexically inside the root if `foo` is not a link elsewhere — and every open
  then goes through `os.Root`, which refuses to traverse out at the syscall level and
  closes the window between the check and the open. That window matters because the
  thing driving these tools is model output. `run_shell` is the stated exception and the
  reason the permission gate exists: its working directory is resolved like any other
  path, but a shell goes where it likes, so containment is not claimed.
- **The edit tool, which is the mechanism ADR-0006 is about.** `edit_file` re-derives
  every anchor from disk before it writes anything, and a mismatch is a **rejection**
  carrying a typed reason — `anchor_malformed`, `anchor_drift`, `anchor_ambiguous` —
  because each implies a different recovery and one error string would make the
  distinction unrecoverable after the fact, as well as sending the model to re-read a
  file that had not changed. A drift rejection carries the file's *current* anchors, so
  recovering costs no turn. `edit_file_fuzzy` is a separate tool and not a second
  argument shape on `edit_file`, because one tool taking either pair would have to infer
  which mode a call meant from which fields arrived. Its `ModeFuzzy` is the
  `unattributed` trigger: a fuzzy match above the floor and in the wrong place applies
  cleanly and emits no error, so the mode is recorded on every result the file produces
  and `EditResult.Fuzzy` carries it a second time — a caller has to miss two independent
  signals to under-count the bucket. The floor is 0.90 and its doc comment argues the
  number rather than asserting it, including the part that matters most: the floor is
  *not* what makes this safe. Two near-duplicate regions both clear it, and the
  ambiguity refusal is what stops the guess.
- **`internal/syntax`** — the post-edit gate: a language-native check run immediately
  after an edit, selected by extension, shelled out to. Its honesty matters more than
  its coverage, because SLICE-1 §9 charges a gate failure following an `EditApplied`
  straight to the `harness` bucket, which makes the gate a measurement instrument as
  much as a feature. So the whole weight sits on one distinction: a gate that did not
  run must never read as a gate that passed. `NotRun` is `Outcome`'s zero value, there
  is no bool anywhere in `Result` that could be mistaken for "ok", `ExitCode` is -1
  when nothing ran, and `Skip` says *why* — "no checker for .md" and "node is not
  installed" call for different responses. Steps are split: a **verdict** step has the
  edited file alone as its subject (`gofmt -e`, `node --check`, `py_compile`) and sets
  the outcome; an **advisory** step has a package as its subject (`go build`), runs, is
  journaled in full, and does not. The split is the acceptance criterion made
  mechanical — a correct edit can legitimately leave a package failing to typecheck
  mid-refactor, and charging that to the edit fills the one bucket whose bar is zero
  with noise.
- **`internal/procgroup`** — one answer to "how do we kill a subprocess", rather than
  one per package that happens to spawn something. A command is started as a group
  leader so everything it spawns inherits the group, and a timeout or a cancellation
  signals the *negative* pid so the signal reaches the whole tree; killing by pid
  instead leaves `sh -c 'sleep 300 & wait'` with `sleep` reparented to init and still
  running, and in a bench run leaves a machine that slowly fills with orphans. Graceful
  then forceful, with a bounded grace period, because a compiler given the chance to
  exit removes its own temporary files — and nothing signals on the ordinary exit path,
  since a group signalled after its leader is reaped can reach a stranger whose pid was
  recycled. It holds no policy: three packages use it and each owns its own timeout.
- **`internal/permission`** — the policy half of consent, and nothing else. It formats
  no prompt, reads no key, and does not know a terminal exists: the REPL renders a
  `Request` however it likes, the bench runner answers one without a human, and both sit
  on a `Gate` that cannot tell which of them it is talking to. The failure that
  separation prevents is concrete rather than aesthetic — a classifier living next to
  the prompt hands the headless runner an interactive dependency it cannot satisfy, and
  the usual fix is a "non-interactive mode" flag that approves everything, which is how
  a corpus run escapes its worktree and contaminates the next task's result. It fails
  closed twice over: the zero `Outcome` is a denial, so a caller who drops the value on
  an error path still denies, and every non-allow returns an error wrapping `ErrDenied`,
  so a caller who checks only the error still denies. `allow_session` is exact-match and
  nothing wider; a prefix or directory grant is the version of this that quietly becomes
  "allow everything", and the package does not offer one.
- **`internal/verify`** — forced verification (KAN-787): the project's own command, run
  after any turn that could have changed the tree, journaled whole, and holding the
  right to report success. The hard part is the honest-none case. A harness that quietly
  skipped verification on a project it did not recognise would report every session as
  verified, which is the flattering direction and the one that corrupts the
  measurement — so `SourceNone` is a value, `NotRun` is `Outcome`'s zero value rather
  than `Passed`, and only `Failed` (a command that ran and exited non-zero) blocks a
  success report. A missing toolchain or a timeout is recorded and does not block: it is
  not evidence the model's change is wrong, and treating it as such would make the loop
  unable to finish a task on any project without a Makefile. Discovery **executes
  nothing** — a makefile target, `go.mod`, a non-empty `scripts.test`, a uv project, in
  that fixed order — because probing a tree by building it turns discovery into a side
  effect, and one that happens before the user has consented to anything.
- **`internal/provider/fixture`** — provider traffic as data, plus the loader and
  validator for it. Enough to drive a two-turn session end to end, one fixture per
  extraction route. **Every fixture in it is hand-authored and says so in the file**
  (`"origin": "hand_authored"`). That is no longer because the live client does not
  exist — it landed with KAN-776 — but because the **recorder** does not: KAN-774 owns
  it, and until it lands there is nothing that can scrub an API key out of real traffic
  at write time. That is a deliberate, temporary violation of the test-seam rule below,
  and the drift risk is real: a
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
- **`internal/repo`** — turn snapshots via git shadow refs (ADR-0002 §3). `git add -A`
  into a throwaway `GIT_INDEX_FILE`, `write-tree`, `commit-tree`, `update-ref
  refs/kopicode/<session>/<turn>`. Two guards hold "never touch the user's git
  state": the environment is **verified** to redirect the index rather than assumed
  to, and the read-only path **refuses** any index-writing subcommand outright
  (`status` included — it rewrites the index while reporting). Seen red both ways.
  The exclude goes in `$GIT_COMMON_DIR/info/exclude`, not `$GIT_DIR`'s — a linked
  worktree does not read the latter, so writing there works everywhere except under
  the bench runner. Commits carry a fixed `kopicode` identity and an injected clock,
  so a snapshot never depends on the user having configured git and the same tree
  yields the same sha. **`Repo.Restore` reads one back out** (KAN-938, G1's read
  half): `git archive --format=tar <tree>` through the same guarded `Repo.git` path —
  `archive` is not in `indexWriting`, because it has no index to write, no HEAD to
  move and no ref to touch — decoded in-process rather than piped into a second `tar`
  or `git` subprocess. Every entry is written through an `os.Root` opened on the
  destination, the same mechanism `internal/tools` already rests `Root.Resolve` on, so
  a write is refused at the syscall level if any path component — including one that
  is a symlink already sitting in the destination before the call ran — would leave
  it; two checks Root does not make on its own catch what it has no reason to refuse,
  a ".git" path component and a symlink whose own target escapes the destination
  (`os.Root.Symlink` documents that it "does not validate oldname"). The destination
  is the caller's choice with one refusal — inside the repository's own git
  directory — and Restore never deletes: a path the tree contains is overwritten, a
  path the tree does not mention is left alone, on the theory that turning this into
  an exact checkout is a materially bigger promise than "read a tree back out safely"
  and belongs to the caller. `NewResumingSnapshotter` (KAN-939) is the other
  half a resumed session needs from this package: it attaches to a session id's
  existing shadow-ref chain instead of `NewSnapshotter`'s refusal, seeding its
  parent and last-turn state from the highest turn number the id already has a
  ref for — read off the refs themselves, not the journal's account of them —
  so the next snapshot chains on rather than starting a second, colliding
  chain. `NewSnapshotter`'s refusal is otherwise unchanged: nothing routes to
  the attach path except a caller that says explicitly it is resuming, so a
  fresh session's id colliding with an old one by accident still refuses.
  `NewForkingSnapshotter` (KAN-940) is the third and last: a **new**, never-
  before-seen session id's first snapshot parents onto a *different* session's
  commit at (or at the highest turn not exceeding) a chosen turn — the ref
  lookup `latest` used is generalised into `refAtOrBefore` so both share one
  for-each-ref parse rather than two that could disagree. It is deliberately
  the asymmetric case: the parent is seeded so the new chain has correct git
  ancestry, but `snapped`/`lastTurn` are **not** seeded from the source's turn
  number, because the forked session's own turn numbering is independent (a
  fresh `Engine` always starts counting from wherever `engine.New` seeds it,
  never from the source's), and pre-loading `lastTurn` with a high source turn
  would refuse the new session's own low-numbered first snapshot with
  `ErrTurnNotIncreasing` for no reason connected to anything that chain has
  actually recorded. `NewSnapshotter`'s own refusal is unrelaxed for the new
  id: a fork mints a session id that must never already have refs of its own,
  exactly the same guarantee `NewResumingSnapshotter` leaves untouched for its
  case.
- **`internal/engine`'s `Fork`, and one design decision this file used to
  leave open** (KAN-940). A new entry point, not a third bit on `Open`'s
  `Options.Resume`: forking mints a brand-new session id, a brand-new journal
  file and a chain seeded from someone else's ref, which is materially more
  divergent from an ordinary session than resuming ever is, and threading all
  three through one function's internals would mean every step re-deciding
  which of two unrelated operations it was mid-way through. `Options.Fork`
  names the source session and the turn; `Fork` shares `Open`'s plumbing
  (the credential check, the lock, the tool set and gate, the provider, the
  final `New`/`Start`) and diverges exactly where the two operations
  genuinely differ. Two questions this card had to settle, both argued in
  `fork.go`'s package comment and `journal.SessionForked`'s own doc comment
  rather than merely decided:
  **the tree is restored automatically**, unlike resume, because forking's
  entire premise — try a different continuation from turn N — requires the
  tree to actually *be* at turn N's state, not wherever it has since moved to;
  Resume's own "the ordinary case needs no restore" argument has no analogue
  here, so `Fork` calls `Repo.Restore` unconditionally when the source has a
  snapshot at or before the chosen turn, after first clearing the working
  tree of anything a later turn left behind (`clearWorkingTreeForFork`) —
  `Restore` alone never removes a file the target tree does not mention, by
  its own explicit design, so a caller wanting an exact checkout does that
  itself, and forking is exactly the caller that needs to. **The source's
  history is duplicated into the fork's own journal, not referenced**: a new
  `journal.SessionForked` marker event names the source and the turn, and the
  source's own events for turns 1..N are copied — verbatim, Turn numbers and
  all — immediately after it, so the forked session's own record is a
  complete, self-contained account rather than one that only resolves by
  chasing back into another session's directory, which CLAUDE.md's "one
  session record" boundary reads as a promise about *this* file. Turn
  numbering carries the seam: the forked session's own first new turn is
  numbered N+1, not 1, because `engine.New` now seeds `Engine.turn` from the
  highest `Turn` value in `Config.ResumeHistory` (a fix that also closes a
  latent gap in KAN-939's own resume path, where the counter previously
  always restarted at 0). The source session's own record and refs are never
  written to — `journal.ReadSessionBlobs` reads a *different* session's
  events with blob rehydration, read-only, and nothing in this path ever
  opens the source's journal for append.
- **`internal/lock`** — one session per **working tree** (docs/SLICE-1.md §8), `flock`
  on unix and a documented no-op on Windows. The word "repo" in that section is the
  trap: a linked worktree shares `.git/config`, the object store and the ref store with
  its parent, so keying on the git directory would serialise the bench runner's ten
  concurrent task worktrees. The root is `repo.WorkTreeRoot` — `rev-parse
  --show-toplevel`, falling back to the directory outside a repository — so a session in
  a subdirectory still collides with one at the root, which it must, because a snapshot
  covers the whole tree. `engine.Open` takes it before the journal exists, so a refusal
  leaves no half-session record; a refused invocation creates nothing, because the only
  way to be refused is for a holder to have created `.kopicode/lock` already. The
  refusal names the holder — pid, host, start time, program, session id and the path to
  its record, since a pid alone is reused and says nothing. **No staleness heuristic,
  deliberately:** flock lives on the open file description, so the kernel releases it on
  every exit including panic, SIGKILL and power loss, and `Release` does not unlink the
  file because that reopens the two-inodes race. `kopibench` locks each task's worktree
  and never the tree it was launched from.
- **`internal/provider` + `internal/provider/mock`** — the wire format the loop
  exchanges with a model, and the replay provider that serves recorded traffic through
  it. **This is the primary test seam** (SLICE-1 affordance P2), so the shape it settles
  is the one every engine test inherits. The interface is one method,
  `Complete(ctx, Request) (*Stream, error)`, and it is declared in **`internal/engine`**
  because an interface belongs where it is consumed — the live client satisfies the same
  one without importing the engine, which is the claim now checked rather than
  predicted. A reply arrives as a `Stream`
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
- **`internal/harness`** — ADR-0007 made mechanical: the built-in registry (model id →
  harness configuration name + default provider pin), the harness configuration itself,
  the hash on `SessionStarted`, the `.kopicode/config.toml` reader and the precedence
  between them. `--model` and `--harness` beat the file, the file beats the built-in
  default, and **no environment variable is in the chain at any position** — a test sets
  six plausible spellings and requires the resolution not to move. An unrecognised id is
  a usage error naming the supported set, exit **2**, and the front ends' integration
  test proves the *ordering* rather than the code: a refused invocation leaves the
  working tree byte-for-byte as it found it, so no half-session record exists for a
  session that never started. The pin now has one source of truth in code and a test
  holds it to the shipped fixtures, which is what stops a registry/fixture drift from
  surfacing later as a mock-provider bug. The hash is the fragile part and is guarded
  accordingly: the preimage is written out field by field with length prefixes,
  `Config` may hold **no map** (Go randomises iteration order, so a ranged preimage
  would hash differently per process and nothing would ever pool), a reflection test
  varies every field and requires the hash to move, and a committed golden hash catches
  an encoding change that no configuration value explains. `internal/engine/selection.go`
  is the thin seam the two front ends reach it through, because ADR-0003's allowlist
  has three entries and widening it needs an ADR.
- **The system prompt is a harness value, not a detail of the loop** (KAN-843).
  `Config.SystemPrompt` holds `prompt_default.md`, `go:embed`ed rather than read at run
  time — ADR-0007 decision 5 requires a harness configuration to be a value in the
  binary and not something a user can edit underneath a recorded result — and it is in
  the hash preimage by digest, so editing the prose opens a new arm rather than quietly
  changing what every number is relative to. It is a markdown file rather than a Go
  string because the prose *is* the product: a diff over it reads as prose to somebody
  judging wording, and it can use backticks. What holds it to the code is tests rather
  than care: every tool in the arm's tool set gets a section, in that set's order; every
  `json` argument name on that tool's struct appears in its section as a JSON key; every
  rejection reason `internal/tools` can refuse an edit with has a documented recovery;
  and the prompt states the anchor version. Add a tool on one side only and the suite
  fails. It is also held under an 8 KiB budget, because naive full-history assembly
  re-sends it on every turn and the cost that matters is the share of a cheap model's
  attention spent on instructions rather than on the repository. The golden hash moved
  to `07251b5de390…` when the prompt landed — a configuration *value* changing, so
  `PreimageVersion` stayed at 1, and nothing was invalidated because no bench run has
  happened.
- **A tool catalogue reaches the wire, and the per-argument description gap is closed**
  (KAN-844). `provider.Request.Tools` carries `[]parse.Schema`, translated at the
  wire-building boundary (`internal/provider/client.go`'s `encode`, via the new
  `provider.RenderTools`) into OpenAI's documented `tools: [{type: "function", function:
  {name, description, parameters}}]` shape — the same contract
  `internal/provider/fixture` already commits to for the streamed tool-call delta, so this
  is a second use of a convention already chosen rather than a second dialect. `parse.Schema`
  and `parse.Param` each gained a `Description`; `internal/engine/catalogue.go`'s argument
  structs gained a `kopicode_desc` struct tag beside `kopicode:"required"` (a distinct key
  because a description is free prose that may contain a comma), and a tool's own
  description is a parameter to `entryOf`/`schemaOf` rather than a struct field, since it
  describes the whole call and not one argument. `engine.schemaOf` panics at init on a
  field or tool with no description, the same "declared, never silent" discipline as its
  existing panic on an unmapped type — so the gap CLAUDE.md used to name here cannot
  reopen quietly. Whether the catalogue is advertised at all is
  `harness.Config.AdvertiseNativeTools`, true for the one configuration slice 1 registers
  and false by default nowhere — SLICE-1 §3's three extraction routes exist because not
  every model reliably uses the native one, so whether an arm *offers* it is itself an arm
  variable (KAN-855 is what varies it). Both `Config.ToolCatalogue` (the full rendered
  catalogue: names, descriptions, per-argument types/required/descriptions) and
  `AdvertiseNativeTools` are in the harness config hash preimage — `ToolCatalogue` is a
  literal in `internal/harness/harness.go`, duplicated from the struct tags for the same
  reason `ToolSet`'s names already were (`internal/harness` cannot import
  `internal/engine`; the dependency runs the other way), and held equal to the real
  rendered catalogue by `internal/engine/toolcatalogue_test.go` rather than by trust. No
  map anywhere in the type graph under `Config`: JSON Schema's `properties` keyword wants
  an object, so `provider.ToolParameters` renders one by hand in a custom `MarshalJSON`
  over an ordered `[]ToolProperty`, and `TestConfigHoldsNoMap` (which walks the whole type
  graph, not just `Config`'s direct fields) is what proves that holds. Adding fields the
  preimage did not cover before is an *encoding* change and not a configuration-value
  change, so `PreimageVersion` moved from 1 to 2 — unlike the prompt's own landing above,
  which only moved a value — and the golden hash moved to `5d7c99ef4bd3…` with it; no bench
  run had happened under either hash, so nothing pools incorrectly across the move. The
  mock/replay provider ignores `Request.Tools` entirely (a canned reply does not change
  because of what was advertised), so this reaches the wire only through the live client —
  `make bench-smoke` stayed green across the change, which is the check that the mock path
  still runs end to end with a `Request` shape it does not otherwise look at.
- **The live OpenRouter client** (`internal/provider/client.go`, KAN-776) — one POST to
  `/chat/completions` with `stream=true`, decoded by the *same* SSE reader the replay
  provider drives, and driven in tests by an `httptest` server replaying the fixtures'
  own frames. The **pin is an input**, taken from `Request.Pin` and written to the wire
  verbatim: a client with its own copy would be a fourth place the pin is written down
  and the first nobody checks against `docs/provider-pin.md`. The value comes from
  `internal/harness`'s registry, which stores it as a `provider.Pin` — so the two halves
  meet with nothing to adapt. An unpinned request is refused before it is billable. Retry is capped exponential backoff with **full**
  jitter — `delay = uniform[0, min(cap, base·2ⁿ)]` — on 429 and 5xx and on a transport
  failure only; a 4xx that is not 429 describes the request and is reported at once. The
  clock and the RNG are injected, so the delays are asserted exactly rather than waited
  out, and the attempt budget is a cap the tests drive to the end. Streaming has no
  assembled body, so `Reply.Raw` is nil on the live path and `Stream.Transcript()` is the
  verbatim record of what arrived — building a body out of the chunks would put a
  re-encoding in the record under the one field whose point is that it is not one.
  `OPENROUTER_API_KEY` lives in an `APIKey` type whose `String`, `GoString`,
  `MarshalText`, `MarshalJSON` and `LogValue` all render `[redacted]`; `Client` has its
  own `String` too, because fmt cannot reach an *unexported* field's Stringer and
  `%v` on the client printed the credential in full until it did. Proven absent from the
  journal, the blobs and the log by `internal/engine/keyleak_test.go`, with redaction
  deliberately switched **off** — a leak test run with the journal's redactor on passes
  whatever the client does.
- **`internal/engine` — the bounded turn loop, and it exists** (KAN-789). `Run` is one
  exchange: the user's message in, then prompt → provider → parse → dispatch → observe
  → repeat. A single turn tree — no subagents, no planner, no second loop. It ends on a
  prose reply that asks for no tool (the clean stop), the turn cap, reported usage
  reaching the token budget, a cancelled context, or a provider or harness failure. A
  malformed tool call is explicitly **not** an ending: repair is bounded per reply and a
  call that exhausts its budget continues the turn with the failure as an observation.
  **Every bound comes off `Selection.Config`** and none is a constant in the package —
  a bound the loop held itself would be a bound *outside* the arm, so two runs differing
  in it would compare as identical, which is the hash describing something that is not
  happening. `Stop` is the loop's own vocabulary, finer than the journal's five reasons
  because two distinct stops map onto `error`, and `Stop.ExitCode` is the one mapping
  onto the process exit codes, so no surface re-decides an outcome the engine settled.
  Context assembly is **naive full history** — a decision, not a gap, because a
  compaction strategy chosen without session data is a guess and the journal is the data
  needed to choose one later — and nothing in it truncates: an assembler that dropped a
  large tool result to save tokens is the never-truncate rule crossed from the inside,
  at exactly the point the model could still act on the output. There is no map anywhere
  in the assembler, for the reason there is none in the harness config: a replayed
  journal has to be byte-identical and Go randomises map iteration.
- **Cancellation is on the record, not an absence** (KAN-857). Cancelling a turn already
  worked — the context reaches the provider stream, the consent gate, the tools and the
  shell's process group — but the journal used to say nothing, so "the user pressed
  Ctrl-C", "the process was killed" and "the disk filled up mid-write" were the same
  bytes. `TurnCancelled` names what was *in flight* — `provider_stream`, `tool_call`,
  `verification`, `between_steps` — and the **first** observation wins, because the loop
  checks its context at the top of every turn and every attempt, so a cancellation that
  interrupted a shell command would otherwise be filed as "between_steps" one step
  later: true about the loop, useless about the session. The bench classifier reads
  cancellation off this event rather than off the runner's in-memory summary, which is
  the whole reason it exists.
- **`engine.Open` and a presentation-neutral event stream** (KAN-865). `Config` is a
  struct of `journal.Journal`, `*tools.Set`, `*permission.Gate`, `*syntax.Gate` and
  `parse.Tools` — every one a type `cmd/` cannot *name*, since Go needs an import to
  write a type in a struct literal — so the exported interface was not reachable from
  the surface it was exported for. `Open` is the fix and it satisfies ADR-0003 rather
  than amending it: the allowlist is unchanged, and what changes is that a front end now
  asks for a session instead of assembling one. What `Options` deliberately does **not**
  offer is the argument: the verifier (nil means the default, never "skip"), the two
  gates, the tool set, the journal and the harness configuration are all built inside,
  because a surface able to substitute one would be a surface able to disable a gate by
  forgetting a field. `Options.Events` receives one `engine.Event` per journal event —
  the append happens first and only what the record accepted is announced — plus
  `EventDelta`, the one kind with no payload behind it, marked by a zero `Seq` so a
  fragment that is *happening* cannot be mistaken for a thing that *happened*.
  `ConsentRequest` and `ConsentAnswer` cross the same boundary, so a surface can be
  asked without importing `internal/permission`.
- **`Options.Resume` and `--resume <id>`: reopen a session and continue its turn
  loop** (KAN-939). One caller-supplied bit does three things rather than three
  independent flags a caller could set out of agreement with each other: the
  journal opens in append mode (already unconditional, since any existing
  session id resumes there — `Resume` only makes the intent explicit); the
  session's prior events are read back through the *opened* journal, not
  `journal.ReadSession` — the distinction matters because only the former
  rehydrates blob-spilled fields, and a resumed session missing a large tool
  result's actual content would be the never-truncate rule broken across a
  process boundary — and replayed into a fresh `Assembler` (`resume.go`'s
  `replayHistory`) before `New` returns, so the first new turn's request
  carries the whole prior conversation; and, inside a repository, the turn
  snapshotter attaches via `repo.NewResumingSnapshotter` rather than refusing.
  The replay maps each journal event kind onto the exact `Assembler` call
  `Engine.Run` itself would have made — a dispatched call's result echoes its
  own id when the call was native and lands as a user turn when it was a
  text-route call with no id to echo, matching `Assembler.AppendToolResult`'s
  own rule; a repair, a failure or a blocking verification's feedback follows
  `Engine.observe`'s choice between the two exactly. **One case is a
  documented, un-closed gap**: a reply that failed repair while carrying
  well-formed *native* tool calls (an unknown tool name, a schema mismatch)
  never gets a `ToolCallParsed` written for it, so its real call ids are not
  recoverable from the journal as it is shaped today — nothing in this
  codebase's corpus has been observed to hit this, and closing it needs a
  schema change, not a cleverer replay. **Resume does not restore the working
  tree**, and this is argued rather than deferred: the ordinary reason to
  resume is an interrupted process whose tree is already where it was left,
  and the case that is not ordinary — the user touched the tree by hand in
  between — is exactly the case a silent force-restore would destroy work in
  with no consent asked, which is the direction ADR-0006 spends real effort
  refusing to be wrong in. `Repo.Restore` (KAN-938) is available to a caller
  that wants the tree rewound first, as its own explicit step before `Open`.
  A collision on an id that was never asked to be resumed still refuses
  exactly as before `Resume` existed — the attach path is reachable only
  through the flag, never through an id merely matching one on disk. The CLI
  surface is deliberately thin: `--resume <session-id>` and nothing to list or
  find one, which is KAN-941's job to build on top of this.
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
- **`cmd/kopicode` — two surfaces over one engine.** `repl` is the interactive one
  (KAN-793, ADR-0004): lines to stdout, never the screen, so the terminal keeps its own
  scrollback and its own selection. Streamed model text is treated as an **optimistic
  prefix of the record** — tokens reach the terminal before the journal has an
  `AssistantMessage` to derive from, so when the message lands the surface prints the
  part of the recorded text that was not streamed, and prints the *divergence* rather
  than hiding it if the streamed bytes were not a prefix. The user therefore sees
  exactly what the journal holds. Piped output emits no escape byte at all by
  construction and not by stripping: the non-interactive writer has no code path that
  produces one, progress is a total no-op, and a test drives a whole session through it
  and asserts on the captured bytes. `Ctrl-C` at the prompt discards the line; during a
  turn it cancels that turn's context — which is what reaches the shell tool's process
  group — and prompts again with the session open. The `[cancelled]` line is rendered
  from the journal's `TurnCancelled` like every other line, not from what the surface
  happened to see.
- **`kopicode run --print` — the headless surface, schema 1** (KAN-794). Newline-
  delimited JSON: line 1 is a `{"kind":"stream","schema":1,"record":…}` header, the only
  line that is not an event, and every other line is one `engine.Event` with the journal
  payload's field names. It accumulates nothing and holds no session state, so it has no
  way to print something the record does not hold; a translation table would be exactly
  where the two accounts drift. Deltas are the one thing deliberately dropped — the
  `AssistantMessage` that follows is the recorded text, and emitting both would put the
  same words on the stream twice with only one of them in the record. All five exit
  codes are live and live in one file, because two front ends and a bench harness read
  them: `0` success, `1` task not completed, `2` usage (nothing opened, locked or
  written), `3` provider, `4` harness — and 4 is also the fail-closed default for an
  outcome nobody mapped.
- **`internal/bench` + `cmd/kopibench run`** (KAN-796) — the headless benchmark, a
  first-class front end and not a test utility. One invocation is one arm over the
  frozen corpus; a paired A/B is two invocations of one binary. Every task gets a git
  worktree of its own, detached at the frozen commit, plus a temp HOME, and the task's
  own suite decides the verdict. **Worktrees are reclaimed and the reclamation is
  reported**: removal is deferred on every path including panic and cancellation
  (`Release` detaches from the run's context before it runs a single git command), every
  run reclaims what a previous crash orphaned before it starts, and the counts land in
  the result — because silent cleanup and silent accumulation look identical from
  outside. The checkouts live at a stable path under `.kopicode/bench/worktrees`, which
  is what makes reclamation structural rather than a heuristic and what puts a
  developer's own worktrees and another agent's leases out of reach by construction.
  Exit codes are read for a *benchmark*: a task the model simply failed is `0`, because
  that is the measurement and not an error — if a failing oracle were a failing command
  then `make bench-smoke`, which replays traffic that solves nothing, would be red by
  construction and would measure nothing. **It is not a security boundary**:
  model-authored shell runs in a worktree, not a container, and isolation is slice 3.
- **Three-bucket failure attribution** (KAN-797), derived from journal events and never
  judged — it asks no model, matches no prose, and every rule reads a typed field of a
  typed event. SLICE-1 §9 does not say which rule wins when a session trips several, so
  the order is decided in one place and argued there: nothing-to-attribute first (a task
  whose oracle passed did not fail, and a cancelled one was abandoned rather than
  answered, so charging either would put rows into a tally meant to count failures),
  then `harness`, then `unattributed`, then `model`. `harness` outranks `unattributed`
  because the two make opposite-strength claims — one says we know the failure was ours,
  the other says nobody can tell — and letting the taint swallow a known harness failure
  moves it out of the only bucket with an acceptance bar of zero, which is the
  flattering direction. A failing task whose journal cannot be read is
  `BucketUnclassified` and an error, not a bucket: every bucket is a claim, and an
  unreadable record supports none of them. The paired McNemar scorer takes outcomes as
  data and knows nothing about how they were produced, so the statistics are driven
  against known contingency tables with no provider, corpus or git repository in sight;
  it is still scaffold, because slice 1 registers one harness configuration and there is
  no second arm to feed it.
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

**Both halves of §Demo landed 2026-08-18.** KAN-799 (a real diff in a real repo) and
KAN-800 (a full corpus run) were the last two cards of EPIC-98, and running them is what
turned every number in this file from arithmetic into a measurement. KAN-799 drove the
REPL against a real target repo (`kopicode-eval`, a Minesweeper core) with
`qwen/qwen3-coder-next`: a correct one-line fix, the existing suite stayed green, one
consent prompt captured, and `Ctrl-C` proven to cancel cleanly via a real
`TurnCancelled{phase:"provider_stream"}` journal event — cost ~$0.04. KAN-800 ran the
frozen 10-task corpus against the same pinned arm: **9/10 passed**, real cost **$0.0846**
(well under the $2 estimate — the corpus is small and a stuck task's cost is capped by
the turn limit, not by token volume). The one failure (`go-config-retries`) stopped on
`max_turns` and is classified `harness` per KAN-797's mechanical rule, and it was traced
all the way through the journal rather than accepted at face value: the model twice
copied `read_file`'s anchor-decorated display (`<hash> <line>| <content>`) verbatim into
`edit_file`'s `new_text`, producing syntax errors the post-edit gate caught immediately
and reported precisely — and the model then read the file, including via a direct
`cat config.go` that put the broken line in plain stdout, and repeatedly concluded "the
file looks correct" rather than seeing it. Every kopicode mechanism (anchors, the syntax
gate, verification, permission, the per-task temp `HOME` from KAN-874) behaved exactly as
designed; the failure is a model self-correction limitation, not a harness defect, and
KAN-797's classifier bucketed it as `harness` anyway because that is its deliberately
conservative design (CLAUDE.md's own words: "it will absorb some real model failures...
the correct direction to be wrong in"). Full journal-level root-cause trace is in the
KAN-800 card's comments. This is the harness's first honest number, not a clean one — and
that is the point of running it before claiming anything about model capability.

What does **not** exist, with a card behind it: the fixture **recorder** (KAN-774), which
is why every fixture is still hand-authored; a second registered model (KAN-850), and
therefore no second arm for the scorer; `slog` inside the engine (KAN-771) — the
`--debug` flag and the stderr handler are wired on both front ends, but the engine
itself logs nothing yet. Absent with **no** card: the `.kopicode/lock` advisory lock
SLICE-1 §8 describes.
Nothing stops two sessions in one repository today; `internal/journal/file.go` carries
the forward reference to it, so the gap is known rather than overlooked. `Repo.Restore`
(KAN-938) landed the read primitive, `Options.Resume`/`--resume` (KAN-939) landed
reopening a session's journal, replaying it into the turn loop, and attaching the
snapshotter to its existing chain — deliberately without restoring the working tree,
which stays the caller's own explicit step — and `engine.Fork` (KAN-940) landed
branching a brand-new session from a copy of an existing one's history, which *does*
restore the working tree automatically, for the reason argued under `internal/engine`
above. The CLI surface for either — `--resume`/`--fork` on the REPL, listing sessions
worth resuming or forking from — is KAN-941's job, not a gap in either card.

**How this section goes wrong, which is worth more than the list above.** It described
"foundations landed, no loop yet" on 2026-08-11. By 2026-08-14 KAN-776 found it wrong on
four counts. By 2026-08-17 KAN-846 found it wrong about the loop, the REPL, both
binaries, forced verification, the bench runner, attribution, cancellation, the system
prompt and `make ci` — and six separate agents had been misled by it first. The pattern
never varies: this list describes the repository as of whenever somebody last had a
reason to look at it, and cards land faster than that. So `ls internal/`, `git log
--oneline -30` and the package's own doc comment beat any paragraph here, including this
one. Most packages open with a doc comment that argues its design; it is faster to read
than this section and it cannot be stale relative to the code it sits in.

Build order is [`docs/SLICE-1.md`](docs/SLICE-1.md) §Build Plan. All 18 steps are landed,
including step 18 (the real-repo demo, KAN-799) and the full corpus run (KAN-800).

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
make fmt          # gofmt -w over the package directories `go list` reports
make fmtcheck     # the same set, read-only; fails on any unformatted file
make lint         # golangci-lint run  (staticcheck is one of its linters — not a separate tool)
make vet          # go vet ./...
make tidycheck    # fails if go.mod/go.sum are not tidy, without leaving them changed
make check        # fmtcheck + vet + lint + tidycheck — every cheap static gate
make test         # go test -short -race -count=1 ./...  — the fast inner loop, mock provider only
make test-all     # the FULL suite as CI runs it: -race, integration build tag, e2e git fixtures
make xbuild       # cross-compile AND vet every GOOS/GOARCH target — platform breaks surface here
make bench-smoke  # the 10-task corpus against the MOCK provider — zero tokens
make bench        # the corpus against the real pinned provider — COSTS MONEY
make smoke-live   # ONE 16-token request against the pinned endpoint — costs cents, behind `live`
make secrets      # gitleaks over history + tree
make vuln         # govulncheck over the module
make ci           # check + test-all + bench-smoke + xbuild
make install-hooks
```

**`fmt` and `fmtcheck` are fed `go list` output rather than `.`, and that is not
cosmetic.** gofmt takes directories literally and walks all of them, while `go build`,
`go vet`, `go test` and golangci-lint all skip directories whose name starts with `.` or
`_`. On a machine running parallel agents that difference means every sibling checkout
under `.claude/worktrees/` — so another agent's half-written file failed *your* pre-push
hook, with a message giving no hint where the file came from. Both recipes also refuse
to run on an empty list, because `go list` fails whenever the module does not load and
`gofmt -l` handed nothing reads stdin, finds nothing and exits 0: a gate that reports
success having checked no files is worse than the failure it was covering for.

**What actually runs where, because three agents have now reasoned from a wrong version
of this.** `make ci` is `check test-all bench-smoke xbuild`. `bench-smoke` joined it in
KAN-801, once KAN-796 landed the runner and `cmd/kopibench` stopped being the stub that
exits 4; it is a **required** CI check now, one of seven jobs. It is the
**mock**-provider target — no network, no tokens, a couple of seconds — and `make bench`,
the paid one, is in neither `ci` nor the hook. `make ci` is still not a literal mirror of
the workflow: the `secrets` and `vuln` jobs stay out of it, because the hook already runs
`secrets` and both want a tool the target does not install. The **pre-push hook** runs
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

**`make bench` spends real money.** It is never in `ci` and never in the hook. The first
real run (KAN-800, 2026-08-18) cost **$0.0846** for the full 10-task corpus at the pinned
`qwen/qwen3-coder-next` arm — a measurement now, not the "~$2" arithmetic this line used
to carry forward from a published price list. Treat $0.0846 as this corpus's actual cost
under normal conditions rather than a ceiling: a run where more tasks get stuck (as
`go-config-retries` did here, running to the `max_turns` cap) costs more per stuck task,
so the old ~$2 order-of-magnitude figure is still the safer number to budget against, even
though the observed run came in far under it. See
[`docs/provider-pin.md`](docs/provider-pin.md) for the reported usage in full.
`make bench-smoke` is the free equivalent and is what gates a PR.

`make smoke-live` is the other target that spends money, and it spends cents rather than
dollars: one 16-token request against the pinned endpoint, behind the `live` build tag so
that neither `test` nor `test-all` can pick it up and the default suite stays hermetic
and free. Its output is the point rather than its pass — it exists to record what the
real API actually returns, including the exact casing of the response's `provider` field,
which OpenRouter documents nowhere and `docs/provider-pin.md` has to record rather than
guess. It has not been run yet (KAN-852).

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
  can merge. Wait for all seven checks to be *created and resolved*, not merely
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

Two entries used to promise more than they hold, so they say less now. `repo/` writes
shadow refs and reads a tree back out via `Restore` (KAN-938) — nothing more: the bench
runner owns worktrees, and the one diff that is journaled is built in process by
`internal/tools/edit.go` (see the diff rule below).

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
  allowlist has three entries: `internal/engine` and `internal/bench`, which are the two
  surfaces' engines, and `internal/build`, which is the one exception, because
  `--version` is a surface concern and that package holds no behaviour. It is allowed
  only for as long as it stays a leaf, which a second test enforces. Do not add a fourth
  without an ADR — and note that `engine.Open` exists precisely so that a front end can
  start a session without needing one.
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
  follow. **A `Cmd` built as a struct literal counts** (KAN-853): `&exec.Cmd{Path: git,
  Args: …}` spawns what `exec.Command` spawns and inherits exactly the same way, and it
  passed all three rules until the guard learned to see it — including the elided form
  inside a slice of `Cmd`s, where the type name never appears. The git half was **not**
  relaxed to fit the general case: the rule is
  identical and only the failure message differs. Sites where the inherited directory or
  environment genuinely is the right one carry `//kopicode:allow-nodir: <reason>` or
  `//kopicode:allow-noenv: <reason>` in place. The two are independent, a waiver with no
  reason waives nothing, and a comment that only looks like a waiver fails the suite.

  **Assigning `Env` is not the same as deciding what it holds** (KAN-845). `cmd.Env =
  os.Environ()` satisfies the assignment rule while inheriting everything, and `cmd.Env =
  nil` is os/exec's spelling for the same thing. A third check reads the *value*, waived
  by `//kopicode:allow-ambientenv: <reason>`, independent of the other two. It decides
  exactly four syntactic forms — `os.Environ()`, an `append` over one, `nil`, and a local
  variable in the same function assigned one of those — and **says nothing about any
  other shape**, which is not a gap to be closed but the funnel: every other shape means
  a named builder, and the name and its doc comment are the review surface. The builders
  live one per package that spawns something — `repo.baseEnv`, `syntax.baseEnv` (with
  `goEnv`/`nodeEnv` over it), `tools.childEnv`, `verify.baseEnv`, `bench`'s `gitEnv`,
  `goQueryEnv` and `oracleEnv`, the `passThrough` allowlist in `internal/corpus`,
  `internal/build`'s `toolchainPassThrough` (an unexported test-file variable, since that
  package builds no binary of its own to test), and `cmd/kopicode`'s `toolchainEnv`, which
  compiles a front end to drive it in an integration test.
  Build an environment from one of those, never from the one you were handed, and note
  that they are not all the same shape by accident: an **allowlist** is right for
  secrets, where the cost of missing one is unbounded, and wrong for a toolchain, where
  `PATH`, `TMPDIR`, `SSL_CERT_FILE` and a dozen distribution-specific variables all have
  to survive or the command does not run at all. `verify.baseEnv` and `syntax.baseEnv`
  are therefore denylists, and say so where they are declared. A toolchain builder that
  runs `go build`/`go test` — `internal/build`'s, `internal/corpus`'s and
  `cmd/kopicode`'s three `go build` env-builders among them — is an **allowlist**
  instead, and for the opposite reason a secret scrub is one: a denylist over
  `os.Environ()` is a claim that every variable able to change what the toolchain
  produces or resolves has been enumerated, and the toolchain keeps adding ones that
  were not — `GOEXPERIMENT`, `GOPRIVATE`, `GOTOOLCHAIN`, `GOWORK`. KAN-854 found
  `cmd/kopicode`'s `toolchainEnv` as exactly that kind of denylist (dropping only
  `GOFLAGS`/`GOOS`/`GOARCH`, added by KAN-805) and converted it to match the other two.
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
fixtures come from actual runs and get refreshed when the provider changes. That is the
rule; today it is a rule with an exception, because the recorder is KAN-774 and every
shipped fixture is still hand-authored. Read the fixture bullet above before you rely on
a green suite as evidence about live behaviour.

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
