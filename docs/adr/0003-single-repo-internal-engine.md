# ADR-0003: One repo — the engine is an internal package, not a library

- **Status:** Accepted
- **Date:** 2026-08-11
- **Deciders:** Jian (leejianrong2@gmail.com)

Retires the `kopi-engine` repository. Supersedes the two-repo layout described in
the original kopicode and kopi-engine READMEs.

## Context

The original plan split the work across two repos: `kopi-engine` (agent loop, tool
dispatch, permission model, session state, model routing, context management) and
`kopicode` (the terminal surface). The justification was a second consumer, cuttlefish,
which would share the engine but need a stricter safety model.

Two things changed. [ADR-0002](0002-no-durable-runtime-own-journal.md) removed
Satay, which was the main thing kopi-engine existed to wrap. And cuttlefish is not
started and has no timeline.

That left two repos, two CI setups, two release cadences and a version-compatibility
matrix, in service of one unbuilt product. kopi-engine's own README argued against
this from the other direction — "build for kopicode first, and let the second and
third consumers show what is actually shared" — while simultaneously being the
premature split it warned about.

The engine/surface boundary itself is *not* speculative and should be kept. It has a
concrete driver: slice 1 needs **two front ends over one engine**, an interactive
REPL and a headless benchmark runner, because a benchmark cannot drive a REPL. That
is a genuine architectural requirement. But it is a *package* boundary, not a repo
boundary.

## Decision

**1. One repository: `kopicode`.** The engine lives at `internal/engine/` alongside
`internal/tools/`, `internal/provider/`, `internal/journal/`, `internal/parse/`,
`internal/repo/`, `internal/permission/` and `internal/sandbox/`.

**2. Go's `internal/` is the enforced boundary.** Nothing outside this module can
import these packages — the compiler enforces it, so the split survives without a
lint rule and without discipline.

**3. Two front ends, one engine, from the first slice.**

- `cmd/kopicode/` — the interactive REPL surface
- `cmd/kopibench/` — the headless runner

Both drive the engine through its exported interface only.

> Refined during slice-1 planning: the runner's logic lives in `internal/bench/` with
> its main package under `cmd/kopibench/`, following Go's convention that main packages
> live under `cmd/`. `bench/tasks/` holds the corpus as data. The boundary this ADR
> cares about — two front ends, one engine, neither reaching past the interface — is
> unchanged. An import-hygiene test
asserts that neither front end reaches past that interface into engine internals —
the same guard satay-runtime uses to hold its core-dependency boundary.

**4. The surfaces own UX; the engine owns policy.** Rendering, keybindings and how a
human is asked for permission belong to the front ends. The engine decides *that*
permission is required and what the decision means, never how it is presented. This
clause from the kopi-engine README is correct and carries over unchanged.

**5. Splitting later is a rename.** If cuttlefish is built and genuinely needs the
engine, promoting `internal/engine` to a published module is mechanical. Doing it
now is not recoverable effort — it is effort spent on a guess about which seams
matter.

**6. `bench/` stays in-repo for now.** It has a different lifecycle (task corpora,
container images, result data) and a third repo is defensible later. It is not
defensible before the runner exists.

## Consequences

- `github.com/leejianrong/kopi-engine` is retired. Its README's two substantive
  arguments are preserved: the transcript lesson moves into
  [ADR-0002](0002-no-durable-runtime-own-journal.md), and the two-products safety
  argument into the kopicode README.
- No version pinning between engine and surface; they move together. One CI, one
  release.
- The engine cannot be consumed externally until deliberately promoted. Intended.
- The risk is a boundary that erodes because nothing forces it across a network or a
  module edge. The import-hygiene test is the mitigation and must stay green.
