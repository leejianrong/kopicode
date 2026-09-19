# ADR-0016: Live remote consent, and an explicit consent-mode declaration, for agent-orchestrated sessions

- **Status:** Proposed
- **Date:** 2026-09-19
- **Deciders:** Jian (leejianrong2@gmail.com)

Drafted by an agent during the same 2026-09-19 planning session as
[ADR-0015](0015-mcp-server-front-end.md). Pending Jian's review.

Amends [ADR-0009](0009-ask-tool-contract.md) (the `Consenter`/`ConsentMode` machinery)
and [ADR-0011](0011-unattended-invocation-policy-gate.md) (the declared-allowlist
policy). Builds on ADR-0015's MCP front end as its first concrete caller, but the
`Consenter` this ADR adds is engine-level and usable by any front end with a
bidirectional channel — today that means `serve` and `mcp`. It changes nothing about
the REPL's human-prompt consenter, `run --print`'s `denyHeadless`, or `kopibench`'s
`BenchPolicy`.

## Context

**KAN-1404 already found the concrete gap.** A dogfood run driving `gh` through
`run_shell` under ADR-0011's policy showed the allowlist's exact-match discipline
essentially never matches real exploratory work: the model issued four distinct `gh`
invocations for one task, none matching any pre-declared exact string, so all four were
refused. The allowlist works precisely for a fixed, known command set decided ahead of
time — cuttlefish's imagined shape — and works badly for "spin up kopicode and let it
run off and do some tasks," which is this planning session's stated goal.

**This ADR does not revisit exact-match.** ADR-0008 and ADR-0011 have each,
deliberately, rejected loosening the allowlist into prefix or structural matching, on
the same argument both times: "the version of this feature that quietly becomes allow
everything." That argument has not changed and this ADR does not reopen it. Instead it
asks a different question: what if consent does not need to be *declared ahead of
time* at all, because the caller answering it is a live process already on the other
end of the same channel driving the session?

**The abstraction this needs already exists.** ADR-0009 split
`internal/permission`'s policy decisions from the terminal-specific prompt flow
specifically so a non-human caller could answer consent questions.
`engine.Options.Consent` is already a pluggable `Consenter` func value; `BenchPolicy`
and the REPL's human-prompt consenter are already two different implementations of it.
What is missing is a third: one that asks over a wire instead of a terminal or a static
file, and gets a live answer back.

**The trust-model duality this ADR's own scoping surfaced needs an explicit interface
signal, not an implicit one.** An MCP- or serve-spawned kopicode session can sit in at
least two distinguishable postures:

- **(a) A human is supervising the parent agent, one hop removed** — a developer
  running Claude Code delegates a coding subtask to a locally spawned kopicode against
  the same repo they already trust. This is ADR-0008's existing single-trusted-operator
  mode, proxied through one more process rather than replaced.
- **(b) The parent orchestrator itself runs unattended, no human anywhere in the
  loop** — squarely ADR-0011's cuttlefish-shaped case, where real containment is
  mandatory and, per ADR-0011 decision 4, is never kopicode's own job.

Nothing in the current interface forces a caller to say which posture it's in.
`serve`'s `--policy-file` is a process-level flag set once by whoever starts the
process, which already half-encodes "I intend unattended mode" by its mere presence —
but there is no explicit declaration, and a future MCP client could start a session
without ever being made to read ADR-0008 or ADR-0011, silently taking on the risk
allocation those documents assign to the caller.

## Decision

**1. A new `Consenter` implementation, `RemoteConsenter`**, satisfying exactly the
func-value contract ADR-0009 already established — no new engine-level mechanism, a
third implementation of an existing one. On a permission-requiring action, instead of
prompting a terminal or consulting a static allowlist, it emits a request over the same
channel the front end is already using to drive the session, and blocks the turn until
an answer arrives or a bounded timeout elapses (decision 5).

The exact wire-level shape is deliberately **not** pinned by this ADR:
- For `mcp`, this needs a concrete MCP primitive capable of a server-initiated,
  blocking client round-trip (elicitation is the leading candidate at the time of this
  writing). Verifying which primitive actually fits, against MCP's specification as it
  stands when this is implemented, is an explicit research task for that card — not an
  assumption this ADR bakes in. MCP's surface has moved quickly; asserting a mechanism
  here that turns out to be wrong would cost more than deferring the choice.
- For `serve`, this is a new server-to-client JSON-RPC **request** (today's wire only
  has server-to-client *notifications* — `session.event` — and client-to-server
  requests). `docs/kopicode-serve-protocol.md` needs a new section for it.

**2. A new attribution source, distinct from both `SourceUser` and `SourcePolicy`.**
kopicode cannot verify whether a human or another model answered on the far end of a
`RemoteConsenter` request, so recording the decision as `SourceUser` would overclaim a
guarantee the mechanism cannot check, and recording it as `SourcePolicy` would
misrepresent a live, ad hoc answer as a pre-declared rule match. `PermissionDecided`
gains a new `Source` value (naming left to implementation — `SourceRemote` reads
naturally) for exactly this case, so the three-bucket failure classifier and any future
audit of a session's history can tell the three apart honestly.

**3. Every `mcp`/`serve` session must declare its consent mode explicitly at
session-start.** There is no default that grants capability — matching ADR-0011
decision 3's "an unconfigured invocation is exactly as safe after this ADR as before
it" shape, extended with a second mode:

- **`remote_interactive`** — every permission-requiring decision bubbles live to the
  calling agent via `RemoteConsenter`, per action, with nothing pre-declared.
- **`unattended_policy`** — the existing ADR-0011 declared-allowlist policy, mechanism
  unchanged, but the session-start call must also carry an explicit, required
  acknowledgment field (e.g. `containment_provided: true`) stating the caller is
  supplying real containment per ADR-0011 decision 4. Omitting the consent mode
  entirely, or omitting the acknowledgment field while requesting `unattended_policy`,
  is a startup usage error — refused, not defaulted.

No third "refuse everything" mode needs adding for `mcp`/`serve` specifically: that is
already what happens today with neither flag configured, and this decision only adds
the two modes that grant new capability.

**4. `remote_interactive` requires the front end's channel to support a
server-initiated blocking round-trip**, which is why decision 1 treats the concrete
primitive as an implementation-time question rather than something this ADR can settle
in the abstract.

**5. A bounded timeout on every `RemoteConsenter` request, denying on expiry.** The
REPL can afford to wait indefinitely for a human, who is assumed to eventually answer
or hit Ctrl-C. A remote peer that never answers — crashed, disconnected, or simply
buggy — gives the running turn no equivalent signal that something is stuck, so an
unbounded wait would hang the turn silently. A configurable timeout (a sensible default
left to implementation) resolves to `Deny`, attributed `SourceRemote` with a note that
it timed out, never treated as though granted — matching `internal/permission`'s
existing "the zero `Outcome` denies" posture.

**6. No existing default changes.** `RemoteConsenter` is reached only by a front end
that explicitly wires `consent_mode: remote_interactive` — the REPL keeps its human
prompt, `run --print` keeps `denyHeadless`, and `kopibench` keeps `BenchPolicy`,
unconditionally. This is the same opt-in-only shape ADR-0011 decision 3 already used
for its own allowlist policy.

## Alternatives rejected

- **Loosen the ADR-0011 allowlist to structural or prefix matching instead of adding a
  live consenter.** Rejected for the third time in this project's history (ADR-0008,
  ADR-0011, now here), on the same "quietly becomes allow everything" reasoning. Live
  per-action consent solves KAN-1404's actual ergonomics complaint without touching
  that property at all — it's a different mechanism, not a loosened one.
- **Make `consent_mode` optional with a default.** Rejected: any default that grants
  capability silently widens what an unconfigured caller can do — exactly what ADR-0011
  decision 3 already refused for the allowlist policy. Requiring an explicit field
  costs the caller one parameter and buys back the same principle.
- **Attribute `RemoteConsenter` decisions as `SourceUser`.** Rejected: kopicode cannot
  verify a human, rather than another model, answered on the far end, and recording it
  as `SourceUser` would make the three-bucket classifier's model-vs-harness accounting
  rest on a guarantee it cannot check. A distinct source value states only what is
  actually known.
- **Skip the timeout and wait indefinitely, matching the REPL.** Rejected: a human at a
  terminal is assumed to eventually answer or cancel; a remote peer that never responds
  has no equivalent human-in-the-loop signal, and an unbounded wait would hang the turn
  with no way for anyone to know why.
- **Fold this into ADR-0015 as one ADR.** Considered and rejected for the same reason
  ADR-0013 (the surface) and ADR-0011 (the policy gate) were kept as two separate,
  cross-referencing documents rather than one: the transport/front-end decision and the
  consent-mechanism decision are separable, reviewable, and revisable independently,
  even though the first motivates the second.

## Consequences

- `internal/permission` gains a new `Source` value, and every place that branches on
  `Source` (notably the three-bucket failure classifier) needs to decide how it treats
  a `RemoteConsenter` decision — this needs checking at implementation time rather than
  assumed to fall cleanly into an existing rule.
- `docs/kopicode-serve-protocol.md` gains a new server-to-client request method once
  `serve` grows `remote_interactive` support.
- [ADR-0015](0015-mcp-server-front-end.md)'s MCP surface gets its actual consent story
  from this ADR. Without it, ADR-0015 alone ships an MCP front end with only ADR-0011's
  existing, already-known-limited (KAN-1404) allowlist path — functional for a fixed
  task, impractical for open-ended autonomous work.
- The two trust-model postures named in Context — human-one-hop-removed, and fully
  unattended — become a property the wire forces every caller to declare, rather than
  an implicit assumption riding on which flags happen to be set.
- `engine.Options` gains whatever plumbing carries `consent_mode` and (for
  `unattended_policy`) the containment acknowledgment through to session-build time,
  alongside the existing `Consent`/`ConsentMode` options ADR-0009 already describes.
