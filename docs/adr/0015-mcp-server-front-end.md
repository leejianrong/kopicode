# ADR-0015: An MCP server front end for agent-orchestrated sessions

- **Status:** Proposed
- **Date:** 2026-09-19
- **Deciders:** Jian (leejianrong2@gmail.com)

Drafted by an agent during a 2026-09-19 planning session on kopicode's adoption goal —
profit is explicitly not the goal, adoption is — and a concrete shape for it: a Claude
Code, Hermes, or OpenClaw agent should be able to spin up a kopicode session directly
and let it run a coding task on its own. Pending Jian's review.

Companion to [ADR-0016](0016-live-remote-consent-for-agent-orchestrated-sessions.md),
which is the consent half of the same goal. This ADR is the transport/front-end half
only; read them together.

## Context

**The substrate for "an orchestrator spawns kopicode and drives it" already
exists.** [ADR-0013](0013-agent-controlled-resident-session-surface.md) built exactly
this: `kopicode serve` holds a process open, drives N concurrent `engine.Open` sessions,
and speaks a documented wire
([`docs/kopicode-serve-protocol.md`](../kopicode-serve-protocol.md)). It was designed
with cuttlefish — an in-house sibling product, not yet started — as the imagined
caller.

**The caller this ADR targets is different, and the gap that matters is protocol
interop, not session mechanics.** cuttlefish can be built to speak whatever wire
kopicode already offers, because it's kopicode's own sibling project. A third-party
agent orchestrator — Claude Code, or any other MCP-capable framework — cannot; its
maintainers would have to read `docs/kopicode-serve-protocol.md` and hand-write a
client against a protocol only kopicode speaks. That is a real integration cost, and it
is the single biggest thing standing between "kopicode can technically be driven
headlessly" and "kopicode is a capability another agent can pick up with zero bespoke
code."

**MCP (Model Context Protocol) is the interoperability layer most agent frameworks
already speak.** A kopicode MCP server converts "another team writes a client against
our bespoke JSON-RPC wire" into "another team adds one line to their MCP server list."
This is squarely a `docs/PRD.md` "developers running open-weight models" / adoption
question, not a capability question — the engine does not need to change for this, only
how it is reached.

**serve's protocol-level work already proves the framing is right.** MCP's local
transport is itself newline-framed JSON-RPC 2.0 over stdio — the same shape
`docs/kopicode-serve-protocol.md` already documents and the same shape `run --print`'s
NDJSON discipline established first. The session-management core `serve` already has
(a worker goroutine per session draining a FIFO queue, `engine.Open` on the read loop,
event teeing from journal to wire) is reusable almost as-is; what differs is the
message vocabulary on top, not the transport underneath.

## Decision

**1. A fourth `cmd/kopicode` subcommand, `mcp`**, alongside `repl`, `run --print`, and
`serve`. It implements an MCP server over stdio, using the same OS-process-ownership
access-control model `serve` already uses (the orchestrator owns the child's
stdin/stdout; there is nothing to authenticate).

**2. Additive, not a replacement.** `serve`'s NDJSON JSON-RPC protocol is unchanged —
nothing in `docs/kopicode-serve-protocol.md` is deprecated or altered by this ADR. A
caller that already speaks that wire (cuttlefish, or anyone else) keeps using it
exactly as documented. `mcp` is a second protocol skin over the same session engine,
not a migration.

**3. Extract the protocol-independent session core into a shared package
`cmd/kopicode/session`**, parallel to the existing `cmd/kopicode/lineedit` and
`cmd/kopicode/repl` subpackages. Both `serve.go` and the new `mcp.go` import it. This
is the part of `serve.go` that has nothing to do with JSON-RPC framing: the
per-session registry, the FIFO worker goroutine, start/submit/cancel semantics against
`engine.Open`, and projecting journal events into the schema-1 shape `run --print`
already emits. Splitting this out keeps one session-lifecycle implementation instead of
two independently maintained ones that can drift.

This does not touch the import-hygiene allowlist
([`internal/arch`](../../internal/arch)'s three entries): `cmd/kopicode/session`, like
`serve.go` today, reaches only `internal/engine` (and `internal/build` for
`--version`). It is a new subpackage *within* `cmd/kopicode`, the same kind of split
`lineedit`/`repl` already are — not a new front end reaching past the engine's
interface.

**4. Three MCP tools, mirroring `serve`'s three methods**: a session-start tool
(working directory, prompt, optional model/harness overrides), a session-continue tool
(an open session's id, the next prompt), and a session-cancel tool (an open session's
id). Exact tool names and parameter shapes are an implementation detail for the card
that builds this, not a decision this ADR pins — the methods they must cover are fixed;
their MCP-idiomatic naming is not.

**5. Session events surface as MCP progress notifications, tied to the originating
tool call.** They carry the same per-event schema-1 projection `run --print` and
`serve`'s `session.event` already emit — no new event vocabulary, and no second
transcript. This is the same "one session record, everything else is derived from it"
boundary `CLAUDE.md` already states, applied to a third surface rather than relaxed for
it.

**6. No engine-boundary change.** `mcp.go` talks to `engine.Open` exactly as `serve.go`
does. `internal/permission` and `internal/journal` are unchanged by this ADR — the
consent story for what an MCP-spawned session may do is
[ADR-0016](0016-live-remote-consent-for-agent-orchestrated-sessions.md)'s subject, not
this one's.

**7. Credentials follow the existing rule exactly.** `OPENROUTER_API_KEY` is read once
from the process environment by `engine.Open`, the same as every other surface. The MCP
wire carries no credential and no per-session override — restating an already-settled
decision because this is a new place it must keep holding, not because it is new here.

**8. Until ADR-0016 lands, `mcp` inherits `serve`'s exact consent defaults.** With no
`--policy-file`/`--ask-policy-file`, an MCP-spawned session refuses shell and
out-of-project writes exactly as an unconfigured `serve` process does today. Shipping
this ADR alone widens nothing; it only adds a new door to a room whose lock hasn't
changed.

## Alternatives rejected

- **Replace `serve`'s protocol with MCP outright.** Rejected: `serve` is barely landed
  (EPIC-131), and MCP does not obviously cover every property `serve`'s wire has
  today — `--policy-file`/`--ask-policy-file` are process-level flags, and ADR-0016 may
  find that some of what it needs fits a per-session MCP parameter better than a
  process-level flag. Committing the whole surface to MCP before that shakes out is a
  bigger bet than this ADR needs to make.
- **A separate process that spawns `kopicode serve` as a subprocess and translates
  NDJSON ↔ MCP.** Rejected: this doubles the process-supervision surface (the
  translation layer now owns lifecycle for a child process it didn't design) and adds
  a serialization/framing hop for no benefit over calling `engine.Open` directly from a
  second front end in the same binary — which `serve.go` already demonstrates is
  workable and is exactly decision 3 above.
- **A generic pluggable-protocol/adapter system**, so any future protocol slots in
  without a new ADR. Rejected on the same reasoning
  [ADR-0005](0005-benchmark-and-ab-methodology.md) §7 already gives for deferring a
  plugin catalogue: inventing the abstraction before a second protocol exists to
  compare MCP against is a guess, not a design. Two protocols (serve's wire, MCP) is a
  fine number to hand-maintain; a plugin system is not yet earned.

## Consequences

- A fourth `cmd/kopicode` subcommand exists: `repl`, `run --print`, `serve`, `mcp`.
- No import-hygiene change: `mcp.go` reaches `internal/engine` exactly as `serve.go`
  does, via the new shared `cmd/kopicode/session` package.
- `serve`'s protocol is not deprecated. Two protocol skins now sit over one shared
  session core — more code than one skin, but it avoids two independently-implemented
  session lifecycles drifting apart, which is the failure this ADR is written to avoid.
- Until [ADR-0016](0016-live-remote-consent-for-agent-orchestrated-sessions.md) lands,
  an MCP client wanting shell or write actions to succeed must configure
  `--policy-file`/`--ask-policy-file` exactly as a `serve` client does today, with all
  of that mechanism's existing ergonomics gap (KAN-1404) unresolved. ADR-0016 is what
  actually makes open-ended autonomous work practical over this surface.
- This is the concrete adoption channel `docs/PRD.md`'s "developers running
  open-weight models" success measure has been missing: an MCP-capable coding agent —
  Claude Code named as the motivating example, not the only one — can add kopicode as
  an MCP server with no bespoke client code.
