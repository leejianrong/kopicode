# ADR-0011: A declared policy gate for unattended invocation, and the containment it requires

- **Status:** Accepted
- **Date:** 2026-08-23
- **Deciders:** Jian (leejianrong2@gmail.com)

Drafted by an agent during a 2026-08-23 portfolio-strategy session and accepted by
Jian the same day.

Answers the trigger condition [ADR-0008](0008-shell-isolation-accepted-risk.md) names
and asks to be reopened for, rather than silently retrofitted, when it fires: "kopicode
acquires multi-user or shared-service exposure — a mode where the person running the
binary and the person whose task is being executed are not the same trusted
individual." Builds on the `Consenter`/`ConsentMode` machinery
[ADR-0009](0009-ask-tool-contract.md) describes rather than replacing it.

## Context

**The trigger has fired, by design intent rather than by accident.** cuttlefish-agent
(the always-on, unattended, triggered assistant ADR-0008 already names as kopicode's
sibling, not itself) is planned to spawn kopicode instances against real repositories
with nobody at a terminal to answer a consent prompt. That is exactly ADR-0008's named
trigger condition. ADR-0008 says plainly what to do when it fires — "reopen this
ADR — do not silently retrofit" — and this document is that reopening, scoped to the
new surface cuttlefish introduces.

**ADR-0008's own guidance for what to contain first does not map cleanly onto this
case.** ADR-0008 says, when its triggers fire, "the bench runner is the right first
target for containment, not the REPL" — reasoned from the two surfaces that existed at
the time it was written. A cuttlefish-driven invocation is a **third** surface: not
the REPL (a human is not present) and not the bench runner (it is not scoring a task
against a disposable, ephemeral worktree cloned from a frozen corpus — it is doing
real work against a real repository on the caller's behalf). ADR-0008's reasoning
extends to it, but its concrete recommendation does not, and this ADR has to say what
the containment target actually is for this surface.

**What already exists, read from ADR-0009's own account of the machinery, is more
than half the answer.** `internal/permission` is already split from presentation
specifically so a non-human caller can answer its questions:
`engine.Options.Consent` (a `Consenter` func value) and `Options.ConsentMode`
(`ConsentInteractive` / `ConsentUnattended`) already exist, `mustAskPolicy` already
turns them into a `permission.Policy` at session-build time, and
`permission.BenchPolicy` is already a working, shipped example of a policy that
"answers its own questions" with no human present, attributing every decision to
`permission.SourcePolicy` rather than `SourceUser`. None of this needs to be
invented.

**What exists today is not what cuttlefish needs, and the gap is precise.** Two
non-interactive answerers exist, and neither fits:

- `cmd/kopicode/print.go`'s `denyHeadless` refuses **every** consent-requiring action
  unconditionally. Correct for its purpose — `run --print` has no policy-authoring
  mechanism at all today, so refusing is the only honest default — but it means a
  cuttlefish-driven task that legitimately needs to run tests or write a file cannot
  do anything requiring permission. Headless kopicode today can read and propose; it
  cannot act.
- `permission.BenchPolicy` is a real, working non-interactive policy, but its
  `confine` check is built around the bench runner's own shape: one ephemeral
  worktree per task, cloned from a frozen corpus, torn down after. Stretching it to
  also mean "an orchestrator-declared allowlist against a real, persistent
  repository" would ask one policy to serve two jobs that only coincidentally look
  similar — the same category error ADR-0009 rejected when it declined to extend
  `internal/permission`'s `Verdict` to also carry `ask`'s free-text answers.

**Policy alone was already shown not to be a security boundary, and that argument
does not change here.** ADR-0008's "why not (b) now" section is explicit: tighter
permission-gate scoping is "real, incremental UX hardening for the surface presented
to a *cooperative* model that occasionally proposes something careless. It is not a
security boundary against an adversarial one." Nothing about cuttlefish existing
changes that argument — if anything it sharpens it, because a cuttlefish-driven
kopicode instance is by construction the case ADR-0008's trust model no longer covers
("the person running the binary and the person whose task is being executed are not
the same trusted individual"). A policy that answers correctly is necessary here, and
it is not sufficient on its own.

## Decision

**1. A new `permission.Policy` implementation: a declared allowlist policy**,
parallel to `BenchPolicy` but general-purpose and authored declaratively rather than
hardcoded in Go, since the caller (cuttlefish, or any future orchestrator) needs to
state per-invocation what is permitted rather than requiring a new kopicode build per
policy. Shape:

- An explicit, closed set of permitted shell invocations — exact-match on argv, the
  same discipline `allow_session` already uses and the same reason ADR-0008 already
  gave for rejecting a prefix or directory grant: "the version of this feature that
  quietly becomes 'allow everything.'" No regex-on-danger, no heuristic — ADR-0008's
  "why not (b) now" section already argued that class of control is trivially
  defeated and should not be shipped as though it were a real guard.
- A path scope confining writes to a declared root, using the same containment
  primitive `Root.Resolve` and the worktree mechanism already give the bench runner —
  not a new mechanism, a new caller of the existing one.
- Fail-closed by construction: anything not on the declared list is refused, with no
  exception path. Same posture as every other policy `internal/permission` has today.

**2. Every decision this policy makes is attributed `Source: SourcePolicy`.** No new
attribution value, no new ambiguity — this is the exact vocabulary
`PermissionDecided` already carries and `BenchPolicy` already uses. A
cuttlefish-driven run must never be recorded as though a human answered, for the same
reason `denyHeadless`'s refusals already are not.

**3. This does not change `run --print`'s default.** An invocation with no policy
explicitly configured keeps using `denyHeadless` exactly as it does today — refusing
everything. The new policy is reached only when a caller explicitly wires it, through
a new option on `engine.Open` (alongside `Options.Consent`/`Options.ConsentMode`,
described in ADR-0009) and a corresponding flag for `cmd/kopicode`. An unconfigured
headless invocation is exactly as safe after this ADR as before it — this widens
capability only for a caller that opts in explicitly, the same shape ADR-0007
decision 2 already uses for `--model`/`--harness` overrides.

**4. Real process or container containment is required for any unattended,
policy-gated invocation, and it is the invoking orchestrator's responsibility, not
kopicode's.** Restating decision 3's own logic one level up: a policy answers
*whether an action may start*; nothing in `internal/permission`, old or new, answers
what a started command can do once it is running with the invoking process's full
privilege. ADR-0008 already surveyed why kopicode itself should not build this in
today — Docker/OCI requires a runtime dependency the single-static-binary distribution
promise (ADR-0001) does not carry, Linux namespaces/seccomp/Landlock have no portable
macOS/Windows equivalent, and "a permissive-enough policy for a coding agent... is
close to unsolved in general." None of that calculus changes because cuttlefish now
exists. What changes is *who pays the cost*: ADR-0008's own trigger-condition section
already anticipated exactly this split — "the bench runner can depend on an
operator-provisioned container runtime as an opt-in worker mode without that becoming
a dependency of `cmd/kopicode`." The same principle applies here with cuttlefish as
the operator: a cuttlefish-driven kopicode invocation runs inside a sandbox
cuttlefish provisions (its own internal, E2B-backed `sandbox` package, per the
2026-08-23 portfolio decision to build that as an internal capability rather than a
separate product), and kopicode's own binary gains no sandbox dependency, no Docker
call, and no platform-specific containment code as a result of this ADR.

**5. This amends ADR-0008's trust-model statement, not its "why not (a)/(b) now"
reasoning.** That reasoning — why kopicode itself should not build in-process
containment — stands entirely unchanged; nothing here asks for it to be revisited.
What changes is the trust model ADR-0008 states: it described one legitimate mode
("one trusted developer, running the binary against their own machine, their own
repository, and their own API key"). This ADR adds a second, equally legitimate mode
alongside it — policy-gated, unattended, with containment provided externally by the
invoking orchestrator — rather than replacing the first. The REPL keeps the first
mode exclusively, unconditionally, per ADR-0008's own argument that a REPL editing a
copy of the developer's tree would not be the product at all.

## Alternatives rejected

- **Extending `BenchPolicy` to also serve this case.** Rejected: its `confine`
  semantics are built for one ephemeral worktree per task, cloned from a frozen
  corpus and torn down after — stretching it to mean "an orchestrator-declared
  allowlist against a persistent real repository" asks one already-narrow, tested
  policy to answer two different questions, the same shape of error ADR-0009 already
  refused to make with `internal/permission` and `ask`.
- **Making the new allowlist policy `run --print`'s default**, replacing
  `denyHeadless`. Rejected in decision 3: this would silently widen what an
  unconfigured headless invocation can do, breaking the safety property that makes
  today's default trustworthy for arbitrary CI or script usage with no policy
  authored at all.
- **Building containment inside kopicode itself** — a bundled namespace sandbox, or a
  Docker dependency gated behind a build flag — as part of this ADR. Rejected on
  exactly the grounds ADR-0008 already argued at length (cross-platform asymmetry,
  a dependency the distribution promise does not carry, a permissive-enough policy
  being close to unsolved in general). Cuttlefish's arrival does not change that
  calculus for kopicode's own binary; it only creates a caller who can reasonably be
  asked to provide containment itself.
- **Dangerous-command detection as a stand-in for an explicit allowlist.** Already
  rejected in ADR-0008 for being trivially defeated and for shipping a control that
  looks like a security feature without being one. Not revisited here; the closed
  allowlist in decision 1 is the only mechanism this ADR proposes.
- **Treating this as solved once the policy gate lands, with containment left
  implicit or "assumed handled elsewhere."** Rejected explicitly in decision 4 — the
  whole point of restating ADR-0008's "policy is not a boundary" finding here is to
  prevent exactly this reading. The requirement is stated as a requirement, not a
  footnote, because it is the one ADR-0008 already proved matters most.

## Consequences

- kopicode gains a second, opt-in `permission.Policy` implementation. Neither
  existing surface's default behaviour changes: the REPL still always asks a human,
  unconfigured `run --print` still refuses everything, `kopibench` still uses
  `BenchPolicy`.
- Every allowlist-policy decision is attributed and journaled exactly like every
  other permission decision — no new gap in provenance.
- kopicode's distribution promise (ADR-0001: single static binary, no runtime
  dependency on the host) is unaffected. This ADR adds no sandbox, no container
  runtime, and no platform-specific containment code to kopicode itself.
- cuttlefish-agent's internal, E2B-backed sandbox package becomes the first concrete
  caller obligated to satisfy decision 4's containment requirement. This ADR defines
  the kopicode-side contract that caller must meet; it does not design cuttlefish's
  sandbox, which is cuttlefish's own scope.
- ADR-0008's Status was **Proposed** as of 2026-08-19. If this ADR is accepted,
  ADR-0008 should be treated as amended by it — a second legitimate trust-model mode
  added alongside the first — rather than left describing only the single-trusted-
  developer case as though cuttlefish's arrival had not been anticipated by its own
  trigger-condition section.
