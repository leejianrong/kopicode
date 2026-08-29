# ADR-0013: An agent-controlled resident session surface over stdio

- **Status:** Proposed
- **Date:** 2026-08-29
- **Deciders:** Jian (leejianrong2@gmail.com)

Drafted by an agent during a 2026-08-29 planning session, from a direct answer Jian
gave when asked what's pulling him toward a next milestone: "I want kopicode to be
able to be called/controlled/spun up/used by another agentic harness, such as
cuttlefish (see abang-ai) or hermes/openclaw." Like [ADR-0008](0008-shell-isolation-accepted-risk.md)
and [ADR-0009](0009-ask-tool-contract.md), this is a recommendation for Jian to accept,
amend or reject, and Status stays **Proposed** until he does.

Builds directly on two pieces of work already on the board: KAN-950's scoping of a
JSON-RPC surface (originally framed around an editor/IDE consumer) and KAN-1021's
brainstorm, which deferred a dedicated external-tool mechanism pending a real trigger.
The trigger named here is different from either — not a human in an editor, not a
GitHub-CLI wrapper, but another *agentic harness* driving kopicode the way a person
drives the REPL today.

## Context

**KAN-950 already answered whether the engine boundary needs to change, and it
doesn't.** `engine.Open`'s public interface — `Options.Events`, `Options.Consent`,
`Options.Ask`, `Options.Policy` (ADR-0011), `Session.Run(ctx, prompt)` taking a
cancellable context — already generalizes to a third front end with zero changes to
`internal/engine` and zero additions to ADR-0003's three-entry import allowlist. That
finding stands unchanged by reframing the consumer from an editor to an orchestrator;
if anything it strengthens, because `Options.Policy` was built by ADR-0011 for exactly
this kind of caller ("a caller like cuttlefish that spawns kopicode with no human
present").

**What's missing is not an engine gap, it's a front-end gap, and it's precise.**
`kopicode run --print` already gives a spawning program clean NDJSON on stdout,
deterministic exit codes (`internal/engine`'s event stream, schema 1), and — via
`--policy-file` — a declared allowlist for shell/write permissions with no human
blocking (ADR-0011). Two things it does not give:

1. **No resident, multi-task session.** `runPrint` calls `sess.Run(ctx, prompt)`
   exactly once and then `sess.Close(ctx)` unconditionally — one prompt in, one run,
   exit. `--resume <id>` lets a *later, separate* invocation continue a session's
   history, but that is still one prompt, one process lifetime, per invocation. An
   orchestrator that wants to keep one kopicode instance around and feed it a sequence
   of tasks over time, without paying process-startup cost per task and without
   restarting to get the next turn, has no way to do that today.
2. **`ask` is hardcoded to always refuse headlessly, with no configuration knob.**
   `denyHeadlessAsk` unconditionally returns "no human is present to answer this
   question," regardless of `--policy-file`. Unlike shell/write permissions, which
   ADR-0011 gave an explicit opt-in escape hatch, there is no way for an orchestrator
   to tell kopicode anything about how to behave when the model gets stuck and asks —
   it always dead-ends the same way, whether or not anyone downstream could plausibly
   help.

**Whether the calling orchestrator runs kopicode on bare metal, in a container, or
inside a microVM does not change anything decided here, and is worth stating rather
than assumed.** This is exactly the shape of question ADR-0008 and ADR-0011 already
resolved once for permission gating: "real containment is the caller's job, not
kopicode's — kopicode gains no sandbox dependency from this." A process that spawns
kopicode as a child still owns its stdin/stdout/environment whether or not it does so
from inside a sandbox it manages, so nothing about the decisions below assumes a
particular deployment shape. The one place this *does* matter: whether the calling
orchestrator's isolation strategy is "one warm sandbox handling many tasks over time"
or "a fresh, disposable sandbox per task" decides whether the resident-process model
below gets used for many tasks or degenerates to exactly one — both are the same
protocol, not two.

## Decision

**1. `kopicode serve`, a new subcommand of the existing `cmd/kopicode` binary,
communicating over stdio only — no socket, no new binary.** The orchestrator spawns
`kopicode serve` as a child process and talks over its stdin/stdout, the same relationship
`cuttlefish`/`hermes`/`openclaw` (or another Claude Code session) already has with any
subprocess it starts. This buys the identical property ADR-0011 already leaned on: no
new connection-authentication surface to design, build, or get wrong, because the OS's
own process-ownership model is the access control. A local socket would let multiple
clients attach, which nothing here needs and which would need its own access-control
story precisely because a listening socket, unlike a pipe to a known child, does not
come with a trust boundary for free.

**2. Wire framing: newline-delimited JSON-RPC 2.0, one message per line.** A request
carries `id`/`method`/`params`; a response carries `id` and either `result` or `error`;
a notification (server → client, no reply expected) carries no `id`. This reuses
`run --print`'s existing NDJSON discipline — one JSON value per line, no
`Content-Length` byte-counting header the way LSP frames its messages — rather than
introducing a second, incompatible framing convention onto the same binary for no
reason grounded in this project's own needs.

**3. Session model: one resident process, N concurrent `engine.Open` sessions, keyed
by a caller-supplied session id.** `session.start` (params: working tree path, prompt,
model/harness overrides, an optional `Options.Policy`/ask-policy reference — see
decision 6) opens a session and returns its id; `session.submit` sends the next turn's
prompt to an already-open session; `session.cancel` cancels its `context.Context` —
the identical mechanism the REPL's `Ctrl-C` already drives, `Session.Run` needs nothing
new to support it. `Options.Events` tees as JSON-RPC notifications carrying the
session id, exactly generalizing the same callback the REPL and `--print` already each
consume for two different presentations — this is the existing proof (KAN-950) that a
third consumer generalizes, not a new claim.

`internal/lock`'s one-session-per-working-tree rule is **untouched, and is exactly
correct here too.** Two sessions against the same working tree — whether from two
REPLs, a REPL and a `serve` session, or two tasks the orchestrator tried to run
concurrently through one `serve` process — must collide, not silently queue around
each other; a snapshot covers the whole tree, and ADR-0002's whole point is that two
sessions cannot describe one filesystem without drift. `serve` returns that failure
over the wire as an ordinary JSON-RPC error, the same way the lock refusal already
surfaces as exit 4 in the CLI.

`internal/engine/context.go`'s `Assembler` is documented as belonging to one loop, not
safe for concurrent use (KAN-950's own finding). So: requests against the *same*
session serialize through a per-session queue that `serve` owns — a front-end
concern, not a new engine feature, per ADR-0003 decision 4 ("the engine decides
policy, surfaces decide presentation" extends here to "surfaces decide their own
concurrency, too"). Requests against *different* sessions run truly concurrently
(goroutines in one process), the same way `kopibench` already runs many sessions
concurrently across worktrees in one process today — this is not a new concurrency
model, it is the one already proven at scale by the bench runner, pointed at a
different trigger.

**4. Credentials: unchanged, no wire field, no per-task override.**
`OPENROUTER_API_KEY` is read once from `kopicode serve`'s process environment at
startup, exactly as every other kopicode invocation reads it today, and every session
that resident process ever opens shares it for the process's whole lifetime. The wire
protocol carries no credential of any kind. A credential value that travels over
`session.start`'s params would immediately collide with ADR-0007 decision 6's
hash-preimage discipline (a value that silently varies per run is exactly what that
decision exists to prevent from entering anything hashed) and would reopen, for a
brand-new code path, the exact redaction discipline `OPENROUTER_API_KEY` already has
tested end to end (never journaled, redacted in every serialization path, proven
absent by a dedicated leak test) — work with zero evidence of need behind it today.
Mirrors KAN-1021's identical deferral of a dedicated GitHub tool's auth question until
a real trigger exists. If a genuine multi-tenant, per-task-credential need ever
appears, the answer is already implied by that precedent: keep it out of anything
hashed, treat it as a per-request runtime parameter, and give it the same redaction
tests `APIKey` already has — not decided here, because there is nothing to decide
without a real caller who needs it.

**5. `kopicode serve` gains its own declared-policy flags, reusing ADR-0011's shape
rather than inventing a second one for permissions.** `--policy-file` works exactly as
it does under `run --print` today — same `AllowlistFile` grammar, same
`AllowlistPolicy`, same fail-closed default when omitted. Nothing about permission
gating changes for this surface; it inherits ADR-0011 wholesale.

**6. A new, independent, opt-in mechanism answers `ask` for an unattended caller —
`--ask-policy-file`, deliberately not a new key on `AllowlistFile`.**
`internal/permission/allowlist_file.go`'s own doc comment already states the
precedent this decision follows: "docs/adr/0009-ask-tool-contract.md already declined
the analogous merge for `ask` ... two mechanisms that only coincidentally look
similar." That declined merge was `internal/permission` gaining `ask` semantics
(`OperationAsk`/`KindAsk`/a `Verdict` that carries prose) — a *runtime type system*
merge, not a *file format* one — but folding an `ask`-answering key into
`AllowlistFile` would still read, to a future maintainer, as exactly the kind of
crossing this codebase has twice now declared out of bounds for `ask`. So: a small,
independent, hand-rolled flat-key file (matching `allowlist_file.go`'s own precedent
for why this codebase does not reach for a TOML library over two keys), loaded into a
new `engine.Options.AskPolicy *AskPolicyFile` field that resolves directly to
`Options.Ask`/`Options.AskMode` — nowhere near `internal/permission`, exactly the
"sibling mechanism, at the same layer" shape ADR-0009 decision 2 already established
for `ask`, extended one layer further to its declared-configuration form.

Grammar, one required key:

```text
note = "When ask is called and nobody can answer, assume the most conservative
interpretation of the task consistent with what has been read so far, proceed, and
record the assumption made in the final summary."
```

When `--ask-policy-file` is supplied, the resulting `Answerer` returns an
`AskAnswer{Text: "No human is present. Policy note from the invoking orchestrator: " +
note}` with `Refused: true` — **not** a change to `AskAnswered`'s shape (ADR-0009's
`Refused` field already means exactly this: nobody actually answered *this specific
question*; `Answer` already carries a reason shown to the model, not a person's
words). What changes is only the *content* of that reason: today's fixed "no human is
present to answer this question" becomes an orchestrator-authored instruction the
model can actually act on, rather than a dead end. `Source` stays `"policy"`, the
existing vocabulary, with no new value.

**No pattern-matching or exact-match lookup of question text to canned answers.**
Considered and rejected: `AllowlistPolicy.commandAllowed`'s exact-match-only,
no-heuristic discipline works for shell argv because an orchestrator can genuinely
enumerate the finite set of commands its own project's tooling will ever run. A
model's `ask` question is free natural-language text generated on the spot precisely
*because* the situation wasn't anticipated — there is no bounded vocabulary to
exact-match against, and reaching for fuzzy or substring matching here would be
exactly the "looks like a security control, isn't one" shape ADR-0008 already rejected
for shell commands, applied to a case with even less structure to match on.

**7. This does not obligate "one resident process, many tasks" as the only supported
shape.** A `serve` process that only ever receives one `session.start` before its
sandbox is torn down is not a degraded case of this design, it is the same protocol
with N=1 — the orchestrator's own sandbox lifecycle decides which shape it is, and
`kopicode serve` does not need to know or care which.

## Alternatives rejected

- **A local TCP or Unix-domain socket instead of stdio.** Rejected in decision 1:
  nothing named here needs multiple clients attached to one running kopicode, and a
  listening socket would need its own connection-level access control that a pipe to
  a known child process gets for free.
- **LSP-style `Content-Length`-prefixed framing.** Rejected in decision 2: this
  project already committed to NDJSON for `run --print`'s stdout, and a second,
  incompatible framing convention on the same binary would be inconsistency with no
  benefit behind it — nothing about a coding-agent control channel needs LSP's
  specific byte-counting scheme.
- **A brand-new `cmd/` binary instead of a `kopicode serve` subcommand.** Rejected:
  `cmd/kopicode` already hosts three surfaces (`repl`, `run --print`, `version`)
  behind one binary resolving one harness config from one model id (ADR-0007); a
  fourth subcommand costs nothing structurally that a fourth binary would not also
  cost, and a fourth binary would need its own cross-compile target in `make xbuild`
  for no reason tied to what this surface actually needs.
- **Folding `ask`'s declared answer into `AllowlistFile` as a third key.** Rejected in
  decision 6, at length, because the codebase has twice now declared this exact
  crossing out of bounds (ADR-0009 decision 2; `allowlist_file.go`'s own doc comment)
  and a third instance of "these look similar, so share the file" is the same mistake
  ADR-0009 already named, one layer down.
- **Structured per-task credential passing over the wire protocol.** Rejected in
  decision 4: no evidence of a real multi-tenant need, and it would reopen
  ADR-0007's hash-preimage discipline and the API-key redaction discipline for a
  brand-new code path with nothing behind it.
- **Pattern-matched or fuzzy question-to-answer tables for `ask`.** Rejected in
  decision 6: free-text questions have no bounded vocabulary to match against, and
  approximate matching here would be exactly the "looks like a control, isn't one"
  failure ADR-0008 already rejected for shell commands.
- **Requiring cuttlefish/hermes to keep re-spawning `kopicode run --print` per task
  instead of building a resident surface at all.** Considered seriously — `--print`
  plus `--policy-file` already covers a large fraction of "spawn kopicode headlessly,
  get structured results" — but rejected as the whole answer: it cannot avoid
  per-task process-startup cost, cannot hold a session open across a sequence of
  turns without a separate `--resume` invocation per turn, and does nothing for the
  `ask` gap. `run --print` is not superseded by this ADR; it remains the right choice
  for genuinely one-shot, fire-and-forget invocations, and `serve` is additive.

## Consequences

- `internal/engine`'s public interface is unchanged except for one addition:
  `Options.AskPolicy *AskPolicyFile`, resolved the same place `Options.Policy` already
  is. `internal/permission` gains nothing from this ADR — restated because the
  instinctive move ("ask is a permission concept too") is exactly what ADR-0009
  already argued against, and decision 6 restates why that holds here as well.
- `run --print` and the REPL are both unaffected. Neither changes behavior, neither
  gains a new flag it didn't already have (`--ask-policy-file` is `serve`-only unless a
  later, separate decision extends it to `--print`, which this ADR does not propose).
- A new epic and cards are needed for: the `AskPolicyFile` grammar and loader
  (mirroring `allowlist_file.go`'s shape), wiring it into `Options.Ask`/`AskMode`,
  `cmd/kopicode/serve.go`'s NDJSON JSON-RPC framing and dispatch, the per-session
  request queue, and an integration test driving `serve` end to end with a scripted
  client — the primary test seam this project already uses (mock/replay provider,
  observable outcomes, no internal loop state asserted) extends here with a real
  stdio client in place of a human or `--print`'s single call.
- `ADR-0003`'s three-entry import allowlist is unaffected — `serve` imports
  `internal/engine` exactly as `repl` and `print` do, and needs no new entry.
- `internal/lock`'s behavior and `internal/bench`'s worktree model are both unaffected
  and unextended by this ADR; `serve` is a third caller of `engine.Open`, not a new
  execution model.
- If accepted, this ADR should be read alongside ADR-0009 and ADR-0011 rather than
  read as amending either: ADR-0009's `ask` contract is used, not changed (only what
  a headless `Answerer` implementation returns, per decision 6, which ADR-0009 already
  said is a surface-level wording decision it deliberately left open); ADR-0011's
  `AllowlistFile`/`AllowlistPolicy` are reused verbatim, not modified.
