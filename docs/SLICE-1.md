---
shaping: true
slice: 1
---

# kopicode — SLICE 1: One model, one harness, end to end

The headline proof: kopicode lands a correct diff in a real repository **and** the
headless runner benchmarks it. Both halves are required, because they are what force
the architecture — a benchmark cannot drive a REPL, so the engine has to be usable by
two front ends from the first commit ([ADR-0003](adr/0003-single-repo-internal-engine.md)).

One model (`qwen/qwen3-coder-next`), one registered harness configuration, no plugin
system. **That is a scope limit on this slice, not a property of the product**: the
binary selects its model at run time and resolves a harness configuration from the model
id, per [ADR-0007](adr/0007-model-selection-and-harness-config-shape.md) — slice 1 simply
registers one configuration and measures one model against it. Every decision this slice
rests on is in [`docs/adr/`](adr/) 0001–0007.

The riskiest mechanism is confronted first, deliberately: **weak-model tool calling**.
Everything else in the loop is well-understood engineering; whether a cheap open model
can be made to reliably emit and apply edits is the open question the project exists to
answer, so it is task 1, not task 9.

---

## Affordances

| ID | Affordance | Scope in slice 1 |
|----|------------|------------------|
| E1 | Agent loop: prompt → provider → parse → dispatch → observe → repeat, bounded | Full, single-turn-tree (no subagents, no planner) |
| E2 | Context assembly: system prompt, history, tool results | Naive full-history; **no compaction** (see risks) |
| P1 | OpenRouter provider over `net/http`, streaming | Full, with pinned provider + quantization |
| P2 | Mock/replay provider serving recorded `ProviderResponse` events | Full — the primary test seam |
| R1 | Tool-call extraction, multi-format | Native JSON tool calls **and** a fenced-XML fallback |
| R2 | Tool-call repair: structured error back to the model, bounded retries | Full, max 2 repairs per call |
| T1 | `read_file` (with per-line anchors), `list_dir`, `grep` | Full |
| T2 | `write_file` | Full |
| T3 | `edit_file` — **hash-anchored primary**, fuzzy-anchor fallback (ADR-0006) | Full, fails closed on anchor drift |
| T4 | `run_shell` — process group, timeout, captured streams | Full |
| V1 | Forced verification: the project's test/lint command after a modifying turn | Full, command discovered or configured |
| V2 | Post-edit syntax gate: language-native check immediately after each edit | Full for Go, Python, JS/TS; honest no-op elsewhere |
| J1 | Append-only tagged-union event log (JSONL) with blob spill | Full for the slice-1 event set |
| G1 | `TurnSnapshot` — git shadow ref per turn via `commit-tree` | **Write only** — snapshots are recorded; restore/fork is slice 2 |
| M1 | Permission gate: engine decides *that* asking is required | Full for `run_shell` and writes outside the repo root |
| U1 | Line-oriented REPL, TTY-aware, `Ctrl-C` cancels the turn | Full |
| U2 | `kopicode run --print` — JSON events on stdout, meaningful exit codes | Full |
| B1 | Headless bench runner: worktree per task, unit-test oracle, scored report | Full for a single arm |
| B2 | Three-bucket failure attribution (`model` / `harness` / `unattributed`) | Full — ADR-0006 §3 |
| B3 | Paired A/B scoring (McNemar) | **Scaffold only** — the scorer exists and is unit-tested; there is no second arm to compare yet |

**Explicitly deferred, named so the boundary stays honest.**

Slice 2 or later: fork and `--resume` (G1 restore), a second registered harness
configuration and the plugin axes it earns (ADR-0005 §7), the `ask` tool (agent asks on
genuine ambiguity), semantic model roles (cheap model for grunt work, strong for
planning — expands the A/B matrix, so it waits for the second arm), and a JSON-RPC
surface for editor integration.

Not planned at all: container/VM isolation for bench (slice 3, and named as a gap
below), context compaction or retrieval, MCP, subagents, planning modes,
prefix-cache exploitation, the SWE-bench-Verified anchor run, `glob` as a distinct
tool (grep + `list_dir` cover it), and any hosted or shared journal.

Explicit non-goals, per the README: LSP, DAP debugging, browser or desktop control,
voice, image tools, a memory system, in-process coreutils, and AST-structural editing
(ADR-0006 §6).

---

## Detailed-design items resolved in this slice

### 1. Journal event schema (ADR-0002 §2)

JSONL at `.kopicode/sessions/<session-id>/events.jsonl`, one event per line, appended
and `fsync`ed. Shared envelope, per-type payload.

- **Envelope:** `schema_version`, `session_id`, `seq` (per-session monotonic, 1-based),
  `turn`, `type`, `ts` (RFC 3339 UTC), `payload`.
- **Event types:** `SessionStarted` (cwd, repo head, model id, pinned provider,
  harness config hash, **build identity** — version, commit, tree state and where those
  came from; the hash deliberately excludes the code, so this is what stops two
  incomparable binaries pooling as one arm, ADR-0007 decisions 6 and 7) · `UserMessage` · `AssistantMessage` · `ThinkingBlock` ·
  `ProviderRequest` (model, provider pin, quantization, sampling params, token counts
  — **never the API key**) · `ProviderResponse` (raw response body ref, token counts,
  finish reason) · `ToolCallRequested` (raw text as emitted) · `ToolCallParsed` (tool,
  args) · `ToolCallRepaired` (attempt number, the error fed back) · `ToolCallFailed`
  (reason) · `ToolResult` (full output, never clipped) · `EditApplied` (mode used, the
  anchors referenced, the unified diff actually applied) · `EditRejected` (reason:
  anchor drift, below floor, ambiguous) · `SyntaxGateRun` (checker, exit code, output
  ref) · `PermissionRequested` · `PermissionDecided` · `TurnSnapshot` (git ref, tree
  sha) · `VerificationRun` (command, exit code, output ref) · `SessionEnded` (reason).
- **Blob spill:** any payload field over **64 KiB** is written to
  `.kopicode/blobs/<sha256>` and replaced by a ref. The threshold is arbitrary and
  tunable; what is not negotiable is that nothing is *truncated*
  ([ADR-0002](adr/0002-no-durable-runtime-own-journal.md)).
- **One record.** Everything the REPL prints and everything `--print` emits is derived
  from these events. There is no second transcript.
- **`slog` versus the journal — the line.** Structured logging is still wanted, but a
  logger that duplicates session content *is* the parallel transcript ADR-0002 forbids.
  So: the journal owns anything about the session (messages, tool calls, results,
  edits, permissions). `slog` owns engine-internal diagnostics that are **not** session
  content — startup, config resolution, provider connection and TLS, lock acquisition,
  worktree lifecycle. Off by default, `--debug` to stderr, never to stdout (which
  `--print` owns). If a fact belongs in both, it belongs in the journal only.

### 2. Turn snapshots via git shadow refs (ADR-0002 §3)

After any turn that touched the working tree:

1. `GIT_INDEX_FILE` points at a throwaway index under `.kopicode/`, so the user's index
   is never touched.
2. `git add -A` into that index — untracked files created by the model must be captured
   or the snapshot is a lie.
3. `git write-tree`, then `git commit-tree` with the previous snapshot as parent.
4. `git update-ref refs/kopicode/<session-id>/<turn>`.

The user's branch, HEAD, history and stashes are untouched. `.kopicode/` is added to
`.git/info/exclude` rather than `.gitignore`, so no tracked file changes.

### 3. Tool-call parse-and-repair (priority 1)

Extraction tries, in order: (a) native OpenAI-style `tool_calls`, (b) a fenced
```` ```tool ```` JSON block, (c) an XML-tagged block. First success wins and the
route taken is journaled, because *which* route a model actually uses is a finding.

Repair, on a malformed or unknown call:

- The failure is classified. The taxonomy lives in `internal/parse` and is finer than
  this list was originally written to be: extraction failures (an unlabelled fence, an
  unclosed fence or tag, invalid JSON, a missing name, an ambiguous call) are each
  distinct from one another and from the semantic ones (unknown tool, missing required
  arg, wrong type, unknown enum value). The distinction is the point — each earns a
  different sentence back to the model, and a journal that records them identically
  cannot measure whether the wording worked.
- A **specific** error goes back as the tool result: what was wrong, what was expected,
  and the correct shape for that one tool. Not the whole schema again.
- Max **2** repair attempts per **reply**, then `ToolCallFailed` and the turn continues
  with the failure as an observation rather than aborting. This said "per call" until
  the loop was built; the unit is the reply because a reply carrying one good call and
  one malformed one cannot have the good half dispatched. A weak model asked to fix the
  malformed call will usually re-emit both, and the good one would then apply twice —
  so nothing in a reply is dispatched until all of it parses. The budget is a parameter,
  not a constant, because running with none is how you measure what repair buys.

Every attempt is journaled, so parse-success rate and repair-recovery rate are
measurable rather than anecdotal.

### 4. Hash-anchored edits (priority 2 — [ADR-0006](adr/0006-hash-anchored-edits-and-failure-attribution.md))

**The model never reproduces file content.** `read_file` returns each line prefixed with
a short content-derived anchor; `edit_file` references anchors.

- **Primary mode:** `edit_file(path, anchor_start, anchor_end, new_text)`. Every
  referenced anchor is re-derived from the file on disk at apply time. **Any mismatch is
  a hard rejection**, returning the region's current anchors so the model can retry
  against reality. A drifted file cannot be silently corrupted.
- Anchors are obtainable only from a read, so editing a region the model was never shown
  is structurally impossible rather than merely discouraged.
- **Fallback mode:** fuzzy `before`/`after` anchor matching with normalised whitespace
  and a similarity floor, for when the model cannot produce a usable anchor. Below the
  floor → refuse and return the top 3 near-misses with line numbers. More than one match
  above the floor → **refuse as ambiguous**, never pick. Every fuzzy-mode use is
  journaled as such, because fuzzy fallback rate per model is itself a finding.
- Both modes return the unified diff actually applied, and it is journaled on
  `EditApplied`.
- **Anchor format is a model-facing contract.** Version it; changing it invalidates
  recorded provider traffic and is an experiment-series boundary.

### 5. Forced verification (priority 3)

Configured per project (`.kopicode/config.toml`) or discovered (`Makefile` target,
`go test ./...`, `npm test`, `uv run pytest`). Runs after any turn that modified files.
`VerificationRun` is journaled with the full output. The engine will not report success
on a non-zero exit; the failure becomes the next turn's observation.

### 6. Post-edit syntax gate (ADR-0006 §4)

A language-native check immediately after each edit, selected by file extension:
`gofmt -e` then `go build` for Go, `python -m py_compile`, `node --check`,
`tsc --noEmit` where a tsconfig exists. **Shelled out, never linked** — the same pattern
as `git` and diff rendering.

A failure *directly after an edit* is a hard `harness` signal in §9's classifier: it
means the edit produced code that does not parse, which a correct edit into the intended
region almost never does. It also improves the product independently of the metric, so
it runs in interactive mode too. Where no checker is available for a language, the gate
records honestly that it did not run rather than reporting success.

### 7. Provider pinning (ADR-0005 §2)

Every request sets `provider.order` to a single slug, `allow_fallbacks: false`, and a
fixed `quantizations`. The resolved pin is journaled on `ProviderRequest`. A bench
result whose pin does not match the experiment's declared pin is **discarded, not
adjusted**.

### 8. One session per repo

Two kopicode sessions in one working tree would interleave edits and produce two
session records describing one filesystem — which is the drift ADR-0002 exists to
prevent, arriving through the back door. Slice 1 takes an advisory lock at
`.kopicode/lock` (`flock` on Linux/macOS, degrading to a no-op on Windows, matching
satay-runtime's posture) and **refuses to start** a second session in the same repo,
naming the holder. Concurrent sessions in separate worktrees are fine and are how bench
achieves parallelism.

### 9. Bench execution model and three-bucket attribution

One `git worktree` per task off a frozen corpus commit, plus a temp `HOME`. The oracle
is the task's own unit tests, run in a subprocess with a timeout. Report includes, per
task: pass/fail, turns used, tokens in/out, wall clock, and a failure classification.

**Worktrees must be reclaimed.** A worktree per task per corpus run means 10 full
checkouts per arm, and an A/B series is many arms — left alone this silently fills the
disk, and an agent harness that auto-creates worktrees is the textbook offender. So:
`git worktree remove` in a deferred cleanup on every path including panic and
cancellation, `git worktree prune` at run start to reclaim anything a previous crash
orphaned, and a `--keep-worktrees` flag for when a failure needs post-mortem inspection.
The runner reports how many it removed and how many it kept, because silent cleanup and
silent accumulation look identical from outside.

The classification is **derived from journal events, never judged**, or the acceptance
criteria below are not checkable. Three buckets, per ADR-0006 §3:

- **`harness`** — any of: a `ToolCallFailed` after repairs were exhausted; a tool
  returning an internal error rather than a task-level one; a `SyntaxGateRun` failure
  immediately following an `EditApplied`; the max-turns cap reached; a provider error
  surviving retries; a panic.
- **`unattributed`** — the session used the **fuzzy fallback** edit mode at any point.
  This does not detect a misapplication, it refuses to launder one. It will absorb some
  genuine model failures and make the harness look worse than it is; that is the correct
  direction to be wrong in, and the bucket shrinking over time is a quality signal.
- **`model`** — everything else: the loop ran to a clean stop, every edit was
  hash-anchored, the syntax gate passed, and the oracle still failed.

The residual hole, stated in every report rather than left implicit: a syntactically
valid edit into a region the model *did* read, with correct anchors, can still be
semantically wrong. Nothing here catches that. A sampled review of `EditApplied` diffs
for `model`-classified failures is the backstop, run when the numbers look surprising —
not on every run.

**This is not a security boundary.** Slice 1 runs model-authored shell in a worktree,
not a container. Do not point it at untrusted tasks or run it on a machine you care
about. Isolation is slice 3.

---

## Build Plan

Ordered so each step produces something the next depends on.

1. **Scaffold.** Go module `github.com/leejianrong/kopicode`, Apache-2.0. Makefile with
   the targets in `CLAUDE.md`. `golangci-lint`, `go vet`, `staticcheck`. Pre-push hook.
   Dependencies held to stdlib plus `golang.org/x/term` (line editing) and
   `github.com/google/go-cmp` (tests only). Git is shelled out to, not linked. **Tree**
   diffs meant for a human are rendered by `git diff --no-index` — zero deps, exact
   fidelity. A diff that is **journaled** is built in-process instead, because §Test Plan
   requires a replayed session to produce a byte-identical journal and `git diff` output
   varies with the installed git and the user's config; see `internal/tools/edit.go`.
   Either way, no diff library.

2. **Journal (J1).** Event types as a tagged union: an envelope struct plus a
   `Payload` interface with a `type` discriminator, JSON round-tripping through
   explicit `MarshalJSON`/`UnmarshalJSON` so an unknown future type is preserved
   rather than dropped. `journal.Journal` interface (`Append`, `Read`, `Session`), a
   `FileJournal` implementation, blob spill. Standalone and testable first — and it is
   the seam ADR-0002 §4 exists to protect, so it comes before anything that writes to
   it.

3. **Mock/replay provider (P2).** Reads recorded `ProviderResponse` events and serves
   them in order, keyed by turn. This lands **before** the real provider so every
   subsequent step is testable at zero token cost.

   **The recorder scrubs at write time.** Recorded fixtures are real HTTP traffic, and
   real request headers carry `OPENROUTER_API_KEY`. Scrubbing on read, or relying on
   gitleaks to catch it after the fact, is too late — the key is already in a file
   somebody might commit. An allowlist of recorded headers (not a denylist) is the only
   version of this that stays correct when a provider adds a new auth header.

4. **Tool-call parse-and-repair (R1/R2).** The priority-1 mechanism, built against
   recorded real-world malformed output. Table-driven tests over a corpus of actual
   bad calls collected from the model.

5. **Tools (T1/T2/T4).** `read_file` **with per-line anchors**, `list_dir`, `grep`,
   `write_file`, `run_shell`. `run_shell` uses a process group, a 120s default timeout,
   and kills the group on cancel. Path arguments are resolved and rejected if they escape
   the repo root without permission.

6. **Hash-anchored edit tool (T3).** Priority-2. Anchor derivation shared with
   `read_file`, apply-time re-derivation and hard rejection on drift, the fuzzy fallback
   with its floor and ambiguity refusal, near-miss reporting, applied-diff return, and
   `EditApplied`/`EditRejected` journaling.

7. **Post-edit syntax gate (V2).** Per-language checker selection, shelled out, honest
   no-op where unavailable, `SyntaxGateRun` journaled.

8. **OpenRouter provider (P1).** Streaming SSE over `net/http`, pinned provider and
   quantization, retry with capped exponential backoff and full jitter on 429/5xx off
   an **injected** clock and RNG (so tests are deterministic), API key from
   `OPENROUTER_API_KEY` and never journaled.

9. **Engine loop (E1/E2).** Bounded turn loop with a max-turns cap and a token budget.
   Naive full-history context assembly. Cancellation via `context.Context` threaded
   everywhere. Every provider call and tool dispatch writes its journal events.

10. **Permission gate (M1).** Engine-side policy: `run_shell` always asks; writes
    outside the repo root always ask; reads never ask. Returns a decision request the
    surface renders. Bench mode supplies a non-interactive policy that auto-approves
    within the worktree.

11. **Turn snapshots (G1).** `commit-tree` over a throwaway index, per §2. Write-only.

12. **Forced verification (V1).** Discovery, configuration, journaling, and the rule
    that a non-zero exit blocks a success report.

13. **REPL surface (U1).** Line-oriented streaming, `golang.org/x/term` raw mode with a
    minimal editor (history, arrows, `Ctrl-A/E/K/U`), TTY detection that strips escapes
    when piped, `Ctrl-C` cancels the turn without killing the session.

14. **Headless surface (U2).** `kopicode run --print` emitting journal-derived JSON on
    stdout. Exit codes: `0` success, `1` task not completed, `2` usage error,
    `3` provider error, `4` harness error.

15. **Task corpus.** 10 tasks, frozen at a commit, versioned. Each is a small repo with
    a fast deterministic suite, a task statement, and an oracle command completing in
    ≤20 turns. Include at least two that require reading before editing and one that
    requires a multi-file change.

16. **Bench runner (B1/B2).** Worktree per task, parallelism capped, per-task report
    with the three-bucket classification from §9, and a summary table.

17. **Paired scorer scaffold (B3).** McNemar over discordant pairs, unit-tested against
    known contingency tables. No second arm yet.

18. **Real-repo demo.** Point kopicode at satay-runtime or sibei-flow, give it a real
    small task, land the diff.

---

## Demo

Two demonstrations, both required.

**Product:** in a real repository, a single natural-language task produces a correct
diff that passes that repository's existing test suite, with the REPL streaming
readable output, one permission prompt for the test command, and `Ctrl-C` proven to
cancel cleanly mid-stream.

**Bench:** `kopibench run --arm baseline` executes the frozen 10-task corpus against
`qwen/qwen3-coder-next` on a pinned provider, prints a scored report with per-task
token cost and three-bucket classification, and completes for under **$2**. The same
corpus runs against the mock provider in CI at **zero** token cost.

---

## Test Plan

The primary seam is **the engine driven through its interface by the mock/replay
provider against a temp git-repo fixture**, with an injected clock, seeded RNG and
injected session id. Assert observable outcomes — the journal, the resulting tree, exit
codes, captured stdout — never internal loop state.

### End-to-end

These are the acceptance criteria for the slice.

- A real task in a real repository yields a diff that passes that repository's existing
  suite, unassisted.
- The full corpus run produces a scored report, and **zero failures classify as
  `harness`** under the mechanical rule in §9. This is the slice-1 bar: the model's
  capability is not being claimed, the harness's reliability is.
- The `unattributed` bucket is reported with every result, and its size is stated
  rather than folded into either other bucket.
- The mock-provider plumbing suite is green in CI and spends zero tokens.
- A recorded session replayed through the mock provider produces a **byte-identical
  journal**, proving the loop is deterministic given fixed provider output. This
  requires three injected sources — clock, RNG and session id — otherwise the
  comparison is only structural and the criterion is weaker than it reads.
- An edit whose anchors no longer match the file on disk is **rejected**, the rejection
  is journaled with the region's current anchors, and the file is unchanged. This is the
  criterion the whole edit design exists to satisfy.
- `kopicode run --print` emits journal-derived JSON and the documented exit code for
  each of: success, task-not-completed, usage error, provider error, harness error.
- Every turn that touched files has a `TurnSnapshot` whose tree matches the working
  tree at that point, **including untracked files**, with the user's branch, HEAD,
  index and stashes unmodified.
- Piped output (no TTY) contains no ANSI escapes and is valid line-oriented text.
- `Ctrl-C` mid-stream cancels the turn, kills the shell process group, appends the
  cancellation to the journal, and returns to the prompt with the session alive.
- The API key appears nowhere in the journal, the blobs, or any log at any level.
- No tool output is ever truncated in the journal; oversized payloads are present via
  blob refs.
- A malformed tool call is repaired and the turn proceeds; after 2 failed repairs the
  turn continues with the failure as an observation rather than aborting the session.
- A bench result recorded under a different provider pin than the experiment declares
  is discarded, and the discard is reported rather than silent.

### Integration

- `FileJournal` append is atomic and `seq` is monotonic across a session; a truncated
  final line from a crash is detected on read rather than parsed as valid.
- An unknown future event type survives a read/write round trip instead of being
  dropped.
- Blob spill triggers above 64 KiB, rehydrates transparently, and is content-addressed.
- The parser routes native `tool_calls`, fenced JSON, and XML-tagged calls correctly,
  and journals which route was taken.
- Each repair classification produces a distinct, specific error message, and the
  message names only the failing tool.
- Anchors returned by `read_file` are stable across repeated reads of an unchanged file
  and change when the line changes.
- `edit_file` in anchored mode applies correctly at start-of-file, end-of-file, a single
  line, and a multi-line region; rejects on a drifted anchor; and rejects on an anchor
  the model invented rather than read.
- `edit_file` in fuzzy fallback refuses below the similarity floor with 3 near-misses,
  refuses on two matches above the floor, and journals that fallback was used.
- The syntax gate selects the right checker per extension, records a failure after a
  bad edit, and records "no checker available" honestly for an unsupported language.
- `run_shell` kills the whole process group on timeout and on cancellation, leaving no
  orphans.
- Path resolution rejects `../` escapes and symlinks pointing outside the repo root.
- OpenRouter retry backs off on 429 and 5xx off the injected clock with seeded jitter,
  and gives up after the cap.
- The permission gate asks for `run_shell` and for out-of-root writes, never for reads,
  and the bench policy auto-approves only inside the worktree.
- The bench runner isolates tasks in separate worktrees with no cross-task leakage, and
  a task that hangs is killed at its timeout and scored as a failure.
- The three-bucket classifier assigns each bucket correctly from seeded journals,
  including the `unattributed` trigger on a fuzzy-fallback session.

### Unit

- Event envelope and every payload type round-trip through JSON with the discriminator
  preserved.
- The tagged-union unmarshaller errors clearly on a missing or unknown discriminator.
- Anchor derivation is deterministic, collision-resistant enough over a single file, and
  stable under unrelated edits elsewhere in the file.
- Similarity scoring is symmetric, whitespace-normalising, and monotonic in edit
  distance.
- McNemar's statistic matches known contingency tables, including the zero-discordant
  and small-sample cases.
- Token-cost arithmetic matches published per-million pricing for a known usage record.
- TTY detection strips escapes for a non-TTY writer and keeps them for a TTY.
- Verification-command discovery picks the right command for a Makefile, a Go module, a
  Node project, and a `uv` project, and reports honestly when it finds none.

---

## Risks

1. **The model may not be capable enough.** `qwen3-coder-next` might not land diffs
   reliably regardless of harness quality. Mitigated by the acceptance criteria
   separating harness reliability (zero `harness` failures — in scope) from capability
   (pass rate — measured, not asserted). If capability is the blocker, that is a
   finding, not a slice failure.
2. **The model may not reliably copy anchors back.** This is the new central unknown,
   and it is why the fuzzy fallback exists. If anchored mode has a low uptake rate, the
   `unattributed` bucket swells and slice-1 numbers become uninformative. Visible by
   construction rather than silent, which is the design intent — but it would make the
   anchor scheme itself the thing slice 2 has to fix.
3. **Anchors cost input tokens on every read.** Expected to be repaid by eliminated
   retry loops (oh-my-pi reports 61% output-token reduction on one model, a vendor
   number). Must be measured against a no-anchor arm, not assumed.
4. **The residual semantic-misapplication hole is not closed.** A correctly anchored,
   syntactically valid edit can still be semantically wrong. Bounded and visible via the
   `unattributed` bucket and sampled diff review, not eliminated.
5. **No context compaction.** Naive full history will hit the 262K window on longer
   tasks and cost grows quadratically with turns. Accepted for slice 1 because a
   compaction strategy chosen without session data is a guess; the journal is exactly
   the data needed to choose one. The corpus's ≤20-turn bound keeps it survivable.
6. **The bench runner is not a security boundary.** Model-authored shell runs in a
   worktree. Named, not solved, until slice 3.
7. **Provider pins are not permanent.** A pinned provider can disappear and invalidate
   an experiment series mid-flight. Mitigated by recording the pin per result so the
   break is loud.
8. **Corpus overfitting.** 10 in-house tasks will flatter whatever the harness happens
   to do well. Mitigated by freezing and versioning the corpus, and by the deferred
   SWE-bench-Verified anchor — which means slice-1 numbers are internal-only and must
   not be quoted as comparable.
9. **Windows.** Process groups, `flock`-style locking and shadow refs all differ.
   Best-effort, matching satay-runtime's posture.

---

## Dependencies

- **Upstream:** none. Slice 1 is the root of the graph.
- **Downstream:** slice 2 (fork and `--resume` over G1's snapshots; the `ask` tool; the
  second registered harness configuration, which is what turns B3's scaffold into a real
  paired experiment and what earns the plugin axes ADR-0005 §7 defers) and slice 3 (bench
  isolation, semantic model roles, a JSON-RPC surface, and context management informed by
  real session data).
