# ADR-0008: Model-authored shell isolation is an accepted risk, not a sandbox

- **Status:** Proposed
- **Date:** 2026-08-19
- **Deciders:** Jian (leejianrong2@gmail.com)

Proposed by an agent under KAN-949, not decided by one. Every other ADR in this
directory records a choice Jian made; this one is a scoping recommendation for him to
accept, amend or reject, and its Status stays **Proposed** until he does — an agent
does not get to declare its own risk-acceptance document Accepted on the project
owner's behalf.

Answers [SLICE-1](../SLICE-1.md) risk 6 ("The bench runner is not a security
boundary... Named, not solved, until slice 3") and the [PRD](../PRD.md)'s "container
isolation for the bench runner" deferral. Both name the gap; neither says what fills
it. This ADR is the concrete answer slice 3 will need, or the concrete argument for why
slice 3 should not spend effort here yet.

Amended by [ADR-0011](0011-unattended-invocation-policy-gate.md), which answers
trigger condition 1 below (now fired) with a second, opt-in trust-model mode —
policy-gated, unattended, containment provided by the invoking orchestrator — read
alongside, not in place of, the single-trusted-developer model stated below.

## Context

**What `run_shell` contains today, read from the code rather than assumed.**

`internal/tools/shell.go`'s `RunShell` resolves `req.Dir` through `Root.Resolve`,
starts the command in its own process group (`internal/procgroup`), enforces a timeout
and reacts to context cancellation by signalling the negative pid so the whole tree
dies rather than orphaning a `sleep` reparented to init, and captures stdout/stderr
whole with no truncation. `childEnv` strips exactly one variable —
`OPENROUTER_API_KEY` — and substitutes `HOME`/`USERPROFILE` when the caller supplied
one (the bench path); everything else, `PATH` included, passes through unfiltered. The
package's own doc comment says why: "the command itself is arbitrary, so this is not a
sandbox and pretending otherwise would be the dishonest version." `Dir` is a check on
where the command *starts*, stated explicitly as "not a containment guarantee: a shell
can cd anywhere the user can."

So what exists is: a bounded-lifetime, cleanly-killable subprocess whose working
directory and starting environment are known, running with the full privilege of
whatever process invoked kopicode. Nothing sits between that process and the
filesystem, the network, or any other process on the machine.

**What the permission gate contains, and what it does not.** `internal/permission`
decides *whether to start* the command, not what a started command can do.
`Gate.classify` routes every `OperationShell` through `KindRunShell`, which "always
asks" — the one operation with no location that makes it routine. `AllowSession` is
exact-match on `(kind, detail)`, deliberately: `permission.go`'s own comment calls a
prefix or directory grant "the version of this feature that quietly becomes 'allow
everything'" and refuses to offer one. The gate fails closed on every axis — the zero
`Outcome` denies, a non-allow always wraps `ErrDenied` — which is real and valuable.
None of it bears on what happens *after* a yes. Consent is a decision-time gate on
whether `/bin/sh -c <argv>` gets a `Start()` call; it has no runtime relationship to
that process once it exists.

**The bench runner's worktree is a blast-radius reducer, not a boundary, and says so
itself.** Every task gets its own git worktree and a temp `HOME` (`internal/bench`).
`BenchPolicy.confine` (`internal/permission/policy.go`) checks that a shell action's
*declared* working directory resolves inside the task's worktree before approving it —
but that is the same starting-directory check `run_shell` already makes, re-derived for
a different root. Its own doc comment: "It is not a sandbox. It gates the action the
model asked for; a shell command approved because it starts in the worktree can still
walk out of it. Real isolation is a container." `internal/bench`'s package doc is
blunter still: "This is not a security boundary... Do not point this at untrusted tasks
or run it on a machine you care about." The worktree exists to keep ten parallel tasks'
*accidents* from touching each other and to make cleanup mechanical
(`Worktrees.Reclaim`), not to contain an adversary.

**The REPL has none of that, and by design.** The interactive surface roots its tool
set at the user's real working tree (`repo.WorkTreeRoot`, used by `engine.Open`'s
locking) — there is no worktree, no throwaway `HOME`, no `BenchPolicy`. A shell command
the model runs in the REPL runs directly against the directory the developer is
actually working in. This is not an oversight to fix; it is the product. A REPL that
edited a *copy* of your repo would not be the coding agent CLAUDE.md describes. So the
REPL and the bench runner are two different exposure surfaces with two different
justifications for their current shape, and any isolation proposal has to say which one
it is talking about.

**Why "just add a sandbox" is not free here.** ADR-0001 chose Go specifically for "a
single static binary, no runtime on the host, no dependency resolution at install
time," and rejected the alternatives partly on that distribution story. `make xbuild`
cross-compiles both binaries to linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
and windows/amd64 with `CGO_ENABLED=0`. The module has two dependencies total
(`golang.org/x/term`, `github.com/google/go-cmp` in tests) and CLAUDE.md states
"dependencies stay near-zero" as a boundary, with "shell out, do not link" as the rule
for every piece of language tooling the syntax gate touches. Any isolation mechanism
proposed here has to be argued against that shape, not against a generic "add a
sandbox" backlog item.

## Decision

**Adopt (c): an explicit accepted-risk trust model, now — this document — with two
named trigger conditions for revisiting (a) or (b). Do not build a container or
namespace boundary in this slice, and do not build dangerous-command detection either.**

### The trust model, stated plainly

kopicode's threat model today assumes **one trusted developer, running the binary
against their own machine, their own repository, and their own API key.**
`internal/permission`'s consent gate answers one question — *should this specific
command be allowed to start* — and it answers it honestly and fails closed. It does not
answer, and nothing in the system answers, *what a command is allowed to do once it has
started.* A consented `run_shell` call has the developer's full ambient privilege:
their filesystem, their network, their credentials outside `OPENROUTER_API_KEY`, their
other running processes. This is true on both surfaces. The bench runner's worktree
narrows the *accident* surface between tasks; it narrows nothing about the *adversary*
surface, and its own doc comments already say so.

This is not a new posture invented for this ADR. It is the posture the code already
has, made explicit in one place instead of four independently worded callouts
(`internal/tools/shell.go`'s `childEnv` comment, `internal/bench`'s package doc,
`internal/permission/policy.go`'s `BenchPolicy` comment, SLICE-1 risk 6, and the PRD's
"not a security boundary in v1" bullet). CLAUDE.md's own "declared, never silent"
discipline is the argument for writing it down rather than leaving it as five separate
hedges a reader has to assemble.

It also matches where kopicode sits relative to its sibling project. CLAUDE.md states
outright: "Satay's natural consumer in this suite is cuttlefish (unattended, triggered,
credential-holding, not started), not kopicode." kopicode is the supervised,
interactive tool; a human is present, watching, and asked before every shell command.
The risk this ADR accepts — a consented command has full privilege — is the risk that
belongs to a supervised tool whose operator is also its sole trust boundary. It stops
being the right risk to accept the moment kopicode's usage looks like cuttlefish's.

### Why not (a) now

A real container or namespace boundary was scoped honestly, not dismissed by name only:

- **Docker/OCI.** Requires a container runtime present on the host that the static
  binary does not provide and cannot install. That is a second runtime dependency for a
  tool whose entire ADR-0001 pitch is "no runtime on the host." Docker Desktop on macOS
  and Windows is a multi-gigabyte VM-backed install, which is a worse "casual install"
  story than the Python interpreter ADR-0001 rejected Python partly for requiring.
  Bind-mounting the very worktree the container is meant to protect back into it, so the
  agent's edits land on disk, also re-opens most of the escape surface a container
  exists to close.
- **Lighter primitives — Linux namespaces, seccomp, Landlock.** Go's
  `syscall.SysProcAttr` can `unshare` namespaces without CGo, which is real and worth
  naming honestly. But it is Linux-only. macOS's nearest equivalent (Seatbelt/App
  Sandbox) and Windows's (Job Objects/AppContainer) are different APIs with different
  guarantees, so "one mechanism" becomes three bespoke, independently-maintained
  sandboxes for a project that currently treats Windows as best-effort and has no
  dedicated security engineering capacity. A namespace boundary shipped only on Linux is
  **worse than no boundary at all** if it is marketed as "kopicode sandboxes shell": it
  would be true on one of three supported platforms and false on the other two, which is
  exactly the kind of claim that isn't uniformly true that CLAUDE.md's whole "declared,
  never silent" posture exists to rule out.
- **A permissive-enough policy for a coding agent is close to unsolved in general.** The
  model needs to run compilers, package managers, git, and arbitrary project tooling —
  `childEnv`'s own comment says an allowlist "would break real builds for no gain."
  Restricting syscalls or filesystem reachability enough to matter while leaving real
  builds working is a research-grade scoping problem, not a slice-sized card.

None of this means containment is impossible forever. It means the cost is a
multi-platform security-engineering effort disproportionate to the audience this ADR's
trust model describes, and nothing in the project today needs it paid.

### Why not (b) now

Tighter permission-gate scoping was the other option on the table, and it fails for a
different reason: **it does not change the boundary, only the decision in front of it.**

- **Dangerous-command detection** (regex or heuristic matching on `rm -rf /`,
  `curl | sh`, and similar) is trivially defeated by anything that doesn't match the
  pattern — base64, a different shell, a helper script, a second command chained after
  the scary one gets blocked. Shipping it would put a control in the product that looks
  like a security feature and is not one, which is the opposite of the honesty this
  project's other guards insist on (compare ADR-0006's refusal to call the fuzzy-anchor
  floor "what makes this safe" — the floor is a UX aid, and the ambiguity refusal is the
  actual guard; a dangerous-command list has no equivalent actual guard behind it).
- **Narrower `allow_session` grants** are already as narrow as the mechanism goes —
  exact match on kind and detail, explicitly chosen over a prefix or directory grant.
  There is no meaningfully tighter version of "may this exact argv run" left to build;
  the next increment would be *removing* session grants entirely and re-asking every
  call, which is a usability regression for a security property (blocking what a
  consented command can do) that this change would not deliver.
- **Restricting paths/network reachable by a consented command, short of a full
  container**, runs into the same cross-platform asymmetry as (a): Landlock is a Linux
  5.13+ kernel feature with no portable macOS/Windows analogue, and a Linux-only path
  restriction has the identical "true on one platform, false on two" problem argued
  above. `bubblewrap` (`bwrap`) is a genuine option on Linux but is an external binary
  the project does not ship or control, which is the same category of dependency
  ADR-0001 and CLAUDE.md's near-zero-dependency rule already weigh against.

(b) is real, incremental UX hardening for the surface presented to a *cooperative*
model that occasionally proposes something careless. It is not a security boundary
against an adversarial one, and packaging it as an "isolation model" answer would
misstate what it does.

### Trigger conditions for revisiting

Reopen this ADR — do not silently retrofit — when either becomes true:

1. **kopicode acquires multi-user or shared-service exposure** — a mode where the
   person running the binary and the person whose task is being executed are not the
   same trusted individual (a hosted instance, a CI-triggered run against someone else's
   PR, a shared machine). The trust model's premise ("one trusted developer, their own
   machine") is what breaks first, not the code.
2. **kopicode is pointed at untrusted task input** rather than the operator's own
   repository and their own request — for instance, a bench corpus or task description
   contributed by someone other than the operator, where the *content* being acted on
   (not just the person running the tool) is not trusted.

If either fires, **the bench runner is the right first target for containment, not the
REPL.** The bench runner is the surface that plausibly runs many tasks unattended and at
someone else's request; a container boundary there does not touch the interactive
binary's single-static-binary distribution promise, because the bench runner can depend
on an operator-provisioned container runtime as an opt-in worker mode without that
becoming a dependency of `cmd/kopicode`. The REPL should stay uncontained even after a
trigger fires, because its entire value proposition — edit the developer's actual
tree, live — is incompatible with running against a copy.

## Consequences

- **One authoritative statement of the trust model exists**, and the four scattered
  callouts that stated pieces of it before (`internal/tools/shell.go`, `internal/bench`'s
  package doc, `internal/permission/policy.go`'s `BenchPolicy` comment, SLICE-1 risk 6,
  the PRD's threat-model bullet) are all consistent with it and none contradicts it.
  **Suggested follow-up card, not implemented here:** point those doc comments at this
  ADR by reference (`// see ADR-0008`) instead of independently restating the doctrine,
  so a future edit to the trust model has one place to change rather than five to find.
  This is genuinely small — a comment-only diff — but it is still a code change and this
  card is scoping, not implementation, so it is named rather than done.
- **Nobody should run kopicode against a repository, task input, or machine they do not
  fully trust**, on the strength of `internal/permission`'s consent gate alone. The gate
  is real and correctly fails closed on the question it answers; it does not answer the
  question "is it safe to let this model's shell commands run here at all."
- **The bench runner's worktree-per-task and `BenchPolicy.confine` are unchanged and
  still worth having.** They bound the cost of a well-behaved model going wrong (writing
  into the wrong task's checkout, say) and make cleanup mechanical. This ADR does not ask
  for them to be removed, and does not ask for them to be represented as more than they
  are — their own doc comments already get that right.
- **If a trigger condition fires, the next artifact is a new ADR sizing a specific
  container or worker-process mechanism for the bench runner against the platform matrix
  and dependency posture at that time** — not a quiet addition of Docker calls to
  `internal/bench`. Revisiting the decision is cheap; the decision itself should not be
  reversed by a PR that never named the reversal.
- **This ADR makes no claim about cuttlefish.** Its unattended, credential-holding, "not
  started" workload is exactly the shape that would fail this ADR's trigger conditions on
  day one, and CLAUDE.md is explicit that cuttlefish — not kopicode — is where that
  problem belongs. Nothing here should be read as a template for cuttlefish's isolation
  story; it would need its own, harder, ADR.
