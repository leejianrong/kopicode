# ADR-0012: A supersession-based compaction strategy, and a smaller fix that ships first

- **Status:** Proposed
- **Date:** 2026-08-23
- **Deciders:** Jian (leejianrong2@gmail.com) — pending explicit accept

Drafted by an agent from [`docs/token-growth.md`](../token-growth.md)'s two real dogfood
sessions (KAN-935/936/947). Left **Proposed** rather than **Accepted**, unlike
ADR-0010/0011: those were reviewed and accepted by Jian in the same sitting they were
drafted, and this one has not been. It amends nothing by itself — see decision 4 for the
one existing guarantee it proposes narrowing, which does not take effect until this ADR
does.

## Context

**`internal/engine/context.go`'s `Assembler` is naive full history, on purpose, and it
says so.** Its own doc comment: "every message the session has produced goes into every
request, oldest first, and nothing is dropped, summarised, windowed or clipped... a
compaction strategy chosen without session data is a guess, and the journal is exactly
the data needed to choose one later." KAN-947 is that data, for the first time from
something other than the frozen corpus. This ADR is the "later."

**The data rules out the simplest fix.** `docs/token-growth.md`'s headline finding: growth
is bursty and concentrated in a small number of large `read_file` results, not spread
evenly across turns. In the bytes.js session, two file reads (5,998 and 7,197 bytes)
accounted for over a third of the session's final ~20.5k-token context. A flat
turn-age cap ("keep the last N turns") would keep those two early reads and discard
small, currently-relevant results instead — the wrong lever, confirmed by measurement
rather than assumed.

**A second, unrelated inflation showed up in the same data and needs to be pulled apart
from the first, not folded into the same fix.** The bytes.js session's fix was correct
and verified passing by round-trip 9 of 22; the remaining 13 round-trips were the model
retrying an already-denied, byte-identical `run_shell rm test_bug.js`, growing the
session's prompt tokens by ~16% with zero new information added. That is a repeat-call
problem, not a history-size problem — trimming history wouldn't have stopped it, and a
compaction strategy built to address it would be solving the wrong failure with the
wrong tool. Filed separately as KAN-988; a narrow slice of it is addressed here (decision
1) because it turns out to be the same mechanism as part of decision 2, not because this
ADR set out to fix it.

**Real compaction, not just deciding not to grow a request further, requires touching
something already in history — which is exactly what `Assembler` currently guarantees it
never does.** `context.go`'s doc comment states two properties as settled, each held by a
test: "**Nothing here truncates**... a context assembler that drops a large tool result to
save tokens is the same boundary crossed from the inside, and it removes the diagnostic
output that justified a fix from the one place the model could still act on it"
(`TestAssemblerNeverTruncates`), and `Messages()` returns "the assembled conversation...
every message appended, **unmodified**." Both exist for a real reason — a model mid-fix
needs to keep seeing the failure it hasn't resolved yet — and this ADR does not propose
discarding either one wholesale. What it proposes is a single, narrow, provable exception
to the first, argued precisely because the case it covers is not the case that guarantee
was written to protect (decision 4).

**The journal is not the thing at risk here, and this ADR does not touch it.**
Compaction is a question about what the *next provider request* carries, never about what
the journal records. `internal/tools`' 64 KiB blob-spill and the "never truncate" rule for
tool output are about the permanent record and the diagnostic surface a human or a later
session inspects — nothing in this ADR proposes recording less than today. `Assembler`
already keeps the wire's view separate from the journal's (the loop journals from what
the tool returned, `context.go` only reads what gets fed to the model) — this ADR uses
that existing seam rather than opening a new one.

## Decision

**1. Ship the cheap, uncontroversial half now: collapse a run of byte-identical, already-
denied tool calls into one entry, going forward.** When the loop is about to append a
tool-result message for a `run_shell` (or any) call whose `(tool name, arguments,
decision)` triple is identical to the immediately preceding one **and that one was
already denied**, replace the would-be Nth identical entry with a short count
("...denied 4 more times, identically") rather than repeating the full denial reason
text verbatim. This is not the exception decision 4 argues for: nothing here is
"dropped" in the sense that guarantee cares about, because a repeated identical denial
carries zero information beyond the first occurrence — the model already has the
complete reason from the first one, and every faithful repetition after that is a wire
cost with no corresponding gain in what the model can act on. **This is not a scored
"design a compaction strategy" fix by itself** — it does not touch the read-file-driven
growth `docs/token-growth.md` measured — but it is small, safe, immediately actionable,
and closes the specific waste that KAN-988's dogfood run measured directly (~16% of that
session's final context, from turns that had already finished the actual task). Filed as
its own follow-up card rather than folded into KAN-988's tool-only PR, since it touches
`internal/engine`'s loop rather than `internal/tools`.

**2. The design direction for the harder problem: supersession-based redaction of a
stale `read_file` result, once specific, provable conditions all hold.** Not implemented
by this ADR — decision 5 explains why a follow-up card, not this document, is where that
happens. The shape:

A `read_file` (or `grep`/`list_dir`) tool-result message already in history becomes
eligible for redaction when **all** of the following are true, checked against the
journal's own event stream rather than guessed at:

- A **later** `edit_file` or `write_file` call targeted the exact same resolved path.
- A **later** `verification` event with outcome `passed` has occurred since that edit —
  not merely "the syntax gate passed," which only proves the file parses, but a real
  forced-verification pass, which is this project's own bar for "the change is
  finished" (`internal/verify`'s doc comment: `NotRun` is the zero value, not `Passed`).
- The read is not among the small fixed number of most recent turns (a cheap guard
  against redacting something the model is still actively reasoning about in the
  current exchange, independent of the two conditions above).

When eligible, the original message's content is replaced — never deleted outright —
with a short, honest note: the path, its original byte size, and that it was elided
because a verified edit has since superseded it, plus how to get it back (`read_file`
the path again; nothing about the mechanism makes a re-read impossible or even
discouraged). This mirrors the house style CLAUDE.md states for `internal/tools`
generally — "bounds are declared, never silent" — applied to a compaction action instead
of a containment refusal.

**3. What is deliberately excluded from decision 2's eligibility, and stays excluded
without a future amendment reopening it case by case.** A `run_shell` result is never
redacted by this mechanism, regardless of what came after it — a shell result is
frequently the diagnostic evidence a `verification: passed` event is confirming, and
conflating "the read that preceded a fix" with "the test output that proved the fix"
is exactly the distinction decision 4 rests on. Nothing before the corresponding
`verification: passed` event is ever eligible — an in-progress fix's own supporting
reads stay whole for as long as the fix is in progress, which is the entire point.

**4. The narrow exception to `context.go`'s "nothing here truncates," stated precisely
so it does not become a precedent for anything wider.** The existing guarantee protects
diagnostic output that could still justify an unresolved problem. A `read_file` result
redacted under decision 2's conditions is not that: by construction, a verified
`verification: passed` event has already confirmed the problem it was read for is
resolved, so the content is not evidence for anything still open — it is a stale
snapshot of a file that has since changed underneath it. This ADR proposes the
distinction be written into `context.go`'s own doc comment and `TestAssemblerNeverTruncates`'s
scope explicitly — "never truncates *content that could still justify an unresolved
change*" — rather than silently narrowing a test that currently asserts something
broader. The line is drawn at "provably superseded and verified," not at "looks old" or
"looks big": no other trigger (turn age, byte size alone, a heuristic guess at
relevance) qualifies.

**5. This ADR settles the direction; it does not land the mechanism.** SLICE-1's own
argument for why `Assembler` started naive applies again here in miniature: a
compaction mechanism chosen without watching it run against real sessions is a second
guess dressed as a fix for the first one. Decision 2's shape should be built behind a
mechanism that can be turned off, exercised against the mock/replay provider first
(zero token cost, per this project's existing test-seam discipline), and dogfooded
before it ever runs against a live billed session — the same staged caution ADR-0005
already applies to a harness change generally. A follow-up implementation card is filed
for this (see Consequences), separate from decision 1's smaller, low-risk fix.

## Alternatives rejected

- **A flat turn-age window (keep the last N turns' tool results).** Rejected on the
  measured evidence itself: `docs/token-growth.md` shows growth concentrated in a small
  number of large early reads, not spread by age, so an age-based cutoff keeps the
  expensive content and discards the cheap, currently-relevant content instead.
- **Summarizing tool output with a second LLM call.** Rejected for the same reason
  ADR-0010 rejects free-text prompt mutation as a search dimension: a second model call
  is its own reliability and cost problem, adds nondeterminism to a system whose replay
  guarantee depends on there being none, and this ADR has no evidence it is even
  net-beneficial over the much simpler redaction-on-supersession rule.
- **Compacting inside `internal/journal`.** Rejected outright — the journal is the
  permanent record and the diagnostic surface a human inspects later; this is a
  question about what the *next request* carries, and conflating the two is the
  exact failure mode `internal/journal`'s "never truncate" rule already exists to
  prevent for a different reason (the diagnostic-output rule, not this one).
  `Assembler` reads from what the loop already journaled; it does not, and under this
  ADR still does not, change what gets journaled.
- **Byte-size-only redaction, with no supersession or verification check.** Rejected:
  a large read that has *not* yet been acted on is exactly the content a model needs
  most, and redacting by size alone would strip the biggest, most relevant context at
  the worst time — the mistake decision 4 is written to rule out by name.
- **Doing nothing until a third and fourth dogfood run produce more data.** Considered
  seriously — n=2 is explicitly not a curve (`docs/token-growth.md`'s own caveat) — but
  rejected as the ADR's outcome because decision 1 needs no more data to be worth
  doing (it fixes a measured, unambiguous waste with no downside), and decision 2's
  design can be argued and reviewed now while its landing waits on the mechanism being
  built behind a flag and dogfooded, per decision 5. Waiting to write anything down
  would just mean re-deriving this same argument from the same two sessions later.

## Consequences

- No code changes ship with this ADR. Decision 1 (identical-denial collapse) and
  decision 2 (supersession redaction) are each filed as their own follow-up
  implementation card, the same shape ADR-0011 became KAN-987 — decision 1 is
  immediately actionable; decision 2 is not, until it exists behind a flag per
  decision 5.
- `internal/engine/context.go`'s doc comment and `TestAssemblerNeverTruncates` will need
  a stated, narrow amendment when decision 2 lands — "never truncates content that could
  still justify an unresolved change" rather than an unqualified "nothing here
  truncates" — and that amendment is part of decision 2's implementation card, not a
  separate one.
- Nothing about the journal's completeness changes. Every tool result is recorded in
  full exactly as it is today; only what a later request re-sends to the provider can
  ever be affected, and only for messages meeting decision 2's conditions.
- This ADR does not amend ADR-0007's hash-preimage discipline: whether compaction is
  active, and any of its parameters, would itself need to be a per-arm harness-config
  value in the hash preimage if it ever varies between arms — decision 2's
  implementation card inherits that requirement rather than this ADR re-deriving it.
- Until Jian reviews and this ADR's Status moves to Accepted, no implementation card
  spawned from it should be treated as authorizing a change to `context.go`'s stated
  guarantees — decision 1 does not touch those guarantees and is not blocked on that
  review; decision 2 is.
