# ADR-0020: Live `ask` over serve

- **Status:** Proposed (2026-10-06)
- **Date:** 2026-10-06
- **Deciders:** Jian (leejianrong2@gmail.com)

Answers [issue #173](https://github.com/leejianrong/kopicode/issues/173). Amends
[ADR-0009](0009-ask-tool-contract.md) and [ADR-0013](0013-agent-controlled-resident-session-surface.md) in the same way
[ADR-0016](0016-live-remote-consent-for-agent-orchestrated-sessions.md) amended the consent side.

## Context

`kopicode serve` can bubble a permission decision to the client live (ADR-0016) but not the
model's `ask` tool. `--ask-policy-file` is per process and its one note answers every `ask` of
every session; unset, the model is told no human is present. An orchestrator that has a person
to hand cannot give the model their answer.

## Decision

**1. `ask.request`, a server-to-client request shaped like `consent.request`:**
`{id, method: "ask.request", params: {session, question, context}}`, answered with
`{result: {text}}`. It reuses the consent waiter machinery, so a reply that arrives after a
timeout or a cancel is dropped the same way.

**2. Opt-in per session**, `ask_mode: "remote"` on `session.start`, valid only under
`consent_mode: "remote_interactive"` (a session that has no live client for permissions has none
for questions). Absent, today's behaviour is unchanged: the process ask policy, else the fixed
refusal.

**3. The wait is bounded, by the same timeout as consent** (`--consent-timeout`, overridable by
`consent_timeout`). One knob for "how long may a person take", not two.

**4. Expiry is not a denial.** Consent's timeout is a deny because denying is the safe default.
An unanswered question has no safe default (ADR-0009): expiry returns the existing "nobody could
answer" text to the model, journalled as such, so the model can proceed or stop on its own
judgement rather than read silence as a no.

**5. Attribution.** The journal's `AskAnswered.Source` gains a value, `remote`, beside `user` and
`policy`, for the same reason `permission.SourceRemote` exists: the surface does not get to
assert it is a human.

**6. A feature name**, `ask.request`, in `server.hello` (#175).

## Alternatives rejected

- **Make `--ask-policy-file` per session.** Still a pre-declared note, not an answer to the
  question actually asked.
- **Route `ask` through `internal/permission`.** ADR-0009 decision 2 keeps it a sibling: an ask
  returns text, never a `Verdict`.

## Consequences

- A new journal source value is a compatibility surface (AGENTS.md: events are versioned and
  unknown values preserved); readers that switch on `Source` need to tolerate `remote`.
- `kopicode mcp` is untouched: MCP has its own elicitation path for asks.
- The question text is model output and reaches a person; it is untrusted, as `detail` is.

## Open question for review

Whether decision 4's "nobody could answer" should be a distinct, machine-readable result the
model is told about, or the same prose as the headless case. This ADR keeps the prose identical
so the model has one thing to learn.
