# ADR-0019: Read-only sessions

- **Status:** Proposed (2026-10-06)
- **Date:** 2026-10-06
- **Deciders:** Jian (leejianrong2@gmail.com)

Answers [issue #176](https://github.com/leejianrong/kopicode/issues/176). Builds on
[ADR-0016](0016-live-remote-consent-for-agent-orchestrated-sessions.md) and
[ADR-0017](0017-auto-consent-mode.md).

## Context

cuttlefish-crew has read-only roles (a reviewer, a planner). On other agents it enforces that
with the agent's own mode; on kopicode it cannot, because `internal/permission`'s gate treats
a write that resolves inside the root as needing no consent (`Gate.classify`, the
`OperationWrite` branch returns "not required" when the path is inside the root). So there is
nothing for a client to refuse, and a read-only role is held only by its prompt.

The issue offers two shapes: a `session.start` option that denies edits, or a new consent kind
(`write_in_root`) sent to the client under `remote_interactive`.

## Decision

**1. A `read_only: true` option on `session.start`**, enforced in the gate: an in-root write
is denied outright, with a reason the model sees, and never routed to a consenter or policy.
It is not a new consent kind.

**2. `read_only` is refused under `consent_mode: "auto"`**, as a usage error. Auto allows shell
inside the root unasked, and a shell line can write (`sed -i`, `>`, `go generate`). A session
called read-only whose shell can write is the claim this ADR exists not to make.

**3. Shell is not made read-only, and the docs say so.** Under `remote_interactive` the client
sees every shell line and can refuse a writer; under `unattended_policy` the declared
allowlist is the author's statement of what may run. `read_only` guarantees the *file tools*
cannot change the tree. It is not a sandbox (ADR-0008, ADR-0017), and a role that must be
physically unable to write needs process containment from its caller.

**4. A feature name**, `session.read_only`, in `server.hello` (#175), so a client can require it.

## Why not a `write_in_root` consent kind

It is the more flexible shape and the wrong guarantee. Under `unattended_policy` and `auto`
nobody is asked, so the option would silently not apply in two of three modes, and under
`remote_interactive` correctness would depend on the client answering "deny" every time. An
enforced refusal in the gate holds in every mode that accepts it and fails closed in the one
that cannot.

## Consequences

- One new field on `session.start`, one new `Gate` option, one new denial reason. Existing
  clients and every existing mode are unchanged.
- The gate gains a rule a reader can verify in one place, beside the containment rule.
- A client wanting "ask me before each in-root edit" is not served; that is a different
  feature and can be argued separately.

## Open question for review

Whether decision 2 should instead *narrow* auto under `read_only` (deny shell that the
tokenizer sees redirecting in-root) rather than refuse the pair. This ADR refuses, because the
tokenizer cannot see what `go test` or a script writes, so narrowing would still overstate.
