# ADR-0002: No durable-execution runtime — own event log, git for repo state

- **Status:** Accepted
- **Date:** 2026-08-11
- **Deciders:** Jian (leejianrong2@gmail.com)

Reverses the central bet of the original kopicode README ("a kopicode session is a
journal") and the original kopi-engine README's dependency on Satay. Satay's
[ADR-0025](https://github.com/leejianrong/satay-runtime/blob/main/docs/adr/0025-positioning-agents-first.md)
is unaffected — its no-agent-abstraction non-goal never required kopicode to be a
consumer.

## Context

The original bet: kopicode runs on Satay, so a session is a journal — replay it,
fork it at the turn where it went wrong, diff two attempts. The claim was that no
other terminal coding agent can reconstruct *why* the agent did something, because
their sessions are scrollback rather than a record.

The reconstruction half of that is true and worth keeping. The fork half has a hole.

**Satay forks by replaying a prefix and reusing recorded results.** A fork at turn 7
replays the prefix's `edit_file` calls as recorded *return values*; it does not
re-execute them. The journal records what the agent decided and what it was told.
**It does not record the repo.** So a forked session hands you a rewound
conversation sitting on top of a working tree that never moved — strictly worse than
not forking, because the model's context now disagrees with disk.

For a coding agent, the state that matters is the filesystem, and the thing that
versions filesystems is git. git does the load-bearing half of fork-at-turn-N; a
journal is an index over it.

Feature by feature, for a *supervised, interactive* agent:

| Satay capability | Value to kopicode |
| --- | --- |
| Replay from the top / crash recovery | Low. Users want `--resume`, which is a message list plus repo state. |
| Fork at turn N | High value, but git does the hard part. Commit-per-turn plus message truncation gets ~90% of it. |
| Call-by-call compare | Real — but the consumer is harness debugging and the benchmark gate, not the daily user. |
| Retries, idempotency keys | Provider retry on 429/5xx is ~30 lines. Idempotency matters for *unattended* work. |

And the tax is not zero. Satay's nondeterminism policy is strict by default, which
requires every tool dispatch to be a durable call, `provider.complete` to be a
`@satay.task`, and no clock/env/RNG read in the workflow body. A terminal agent
streams tokens to a screen and stops mid-flight to ask permission — so streamed
partials must stay outside durable results, and every human answer becomes a
`wait_for_event`. Real friction, paid in week two, for benefits kopicode largely
does not collect.

The one place the heavy machinery does pay is **cheap A/B**: if a config change only
alters behaviour after turn 7, replaying turns 1–6 from the record means paying
tokens for turn 7 onward only. Note who that serves — the benchmark matrix, not the
product.

## Decision

**1. kopicode depends on no durable-execution runtime.** Not Satay, and not a
reimplementation of one. No replay-from-the-top, no nondeterminism detection, no
idempotency keys.

**2. One session record, owned by kopicode.** An append-only event log written by
the engine as a **tagged union of typed events**, never truncated:

- `UserMessage`, `AssistantMessage`, `ThinkingBlock`
- `ToolCallRequested`, `ToolCallParsed`, `ToolCallRepaired`, `ToolCallFailed`
- `ToolResult` (full output; large payloads spill to content-addressed files rather
  than being clipped)
- `PermissionRequested`, `PermissionDecided`
- `TurnSnapshot` (git ref, see 3)
- `ProviderRequest`, `ProviderResponse` (token counts, model id, pinned provider)

Everything a user or reviewer sees is **derived** from this log. There is no second
record. This is what sibei-flow's
[ADR-0013](https://github.com/leejianrong/sibei-flow/blob/main/docs/design/adr/0013-transcript-tagged-union.md)
lesson actually demands — the sin there was a lossy `list[str]` clipped at 1200
characters, not the absence of a durable runtime.

**3. Repo state is versioned with git shadow refs.** After every turn that touched
the working tree, the engine writes a snapshot to a ref under
`refs/kopicode/<session>/<turn>` using `git commit-tree`, so the user's branch,
history, index and stashes are untouched. Fork-at-turn-N is then: truncate the event
log at turn N, restore that turn's tree, continue. This works in Go, in slice 1, and
does the thing the original bet promised but could not deliver.

**4. The journal sits behind an interface.** `journal.Journal` with a
`FileJournal` implementation in slice 1. If real use later shows replay and
call-by-call compare are worth their cost, a second implementation can be written
against a durable runtime without touching the engine. Deferring is cheap; the
seam is what keeps it cheap.

**5. Satay's natural consumer in this suite is cuttlefish.** Unattended, triggered,
long-running, credential-holding, with no human watching. Replay from the top is
designed for that workload.

## Consequences

- Crash mid-turn loses that turn's in-flight provider call. Accepted: the human is
  present, the tokens are one turn's worth, and the previous turn's tree snapshot
  is intact.
- `--resume` is implemented by replaying the event log into a message list and
  checking out the last `TurnSnapshot` — not by re-executing anything.
- The cheap-A/B replay trick is unavailable at first. If the benchmark matrix cost
  becomes the binding constraint, the mitigation is a **replay provider** that
  serves recorded `ProviderResponse` events for an unchanged prefix — which the
  event log already supports and which ADR-0005 requires for a different reason.
- The kopicode README's headline differentiator changes from "durable session" to
  "the harness is the product, and the record is auditable." Weaker as a novelty
  claim, honest as an engineering one.
- Journal event types are a compatibility surface from the first commit. Version
  them.
