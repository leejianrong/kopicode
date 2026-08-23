# ADR-0001: Go as the implementation language

- **Status:** Accepted
- **Date:** 2026-08-11
- **Deciders:** Jian (leejianrong2@gmail.com)

Depends on [ADR-0002](0002-no-durable-runtime-own-journal.md), which removes the
constraint that forced Python, and on [ADR-0004](0004-line-oriented-repl.md), which
removes the TUI framework from the comparison.

## Context

Two documents in this project disagreed about the language.

The original kopicode and kopi-engine READMEs put the agent loop on **Satay**, a
durable-execution runtime that is **async-Python-only**. That pins the whole product
to Python by transitive necessity.

`agentic-harness-ideas.md` argued the opposite: Go for compilation safety and process
control, TypeScript on Bun for streaming and ecosystem fit, and *avoid* Python because
dynamically typed code lets LLM-authored agents introduce silent runtime typos that
crash multi-step loops.

That Python argument is the weakest claim in the document, and satay-runtime — sitting
in the same working directory, built by an LLM agent — is the counterexample:
`mypy --strict`, ruff, a layered suite, pre-push gates. The failure mode described is
a *toolchain* failure and the toolchain already solves it. So while Satay was
load-bearing, Python was correct, because losing replay/fork/compare to gain
compile-time types was a bad trade.

ADR-0002 removes Satay. That collapses the only argument for Python, and ADR-0004
removes the only strong argument for Go that was framework-shaped (Bubble Tea).
What remains is a straight comparison on the properties this product needs.

Performance deserves an honest framing: it is nearly irrelevant to the language
choice. A turn is dominated by seconds of model latency, and no in-process work
competes with that. Where the runtime genuinely shows is process startup, scanning
and grepping a large repo, streaming without stutter, and subprocess/sandbox
control.

## Decision

**kopicode is written in Go.** The reasons, in the order they actually matter:

1. **Distribution.** A single static binary, no runtime on the host, no dependency
   resolution at install time. For a terminal tool someone installs casually and
   runs every day, this is the difference between adoption and friction. Python's
   story here (`uv tool install`, requires an interpreter) is the worst of the three.
2. **Robustness under LLM authorship.** Compile-time types and a small syntax
   surface mean a class of errors never reaches runtime. This is the harness-ideas
   argument, and with Satay gone there is nothing paying to override it.
3. **Process and sandbox control.** `os/exec`, context cancellation, and process
   groups are first-class. The agent's job is largely running other programs and
   killing them cleanly.
4. **Startup and streaming.** Fast cold start; goroutines and channels fit
   token streaming and concurrent tool execution without a framework.

OpenRouter is OpenAI-compatible HTTP, so `net/http` and `encoding/json` cover the
provider seam. No SDK is given up by not choosing Python or TypeScript.

**Rejected: TypeScript on Bun.** The strongest runner-up, and it has the most prior
art for this exact product (Claude Code on Ink, opencode). Built-in `readline` is a
genuine convenience given ADR-0004. Lost on looser runtime typing and a heavier
dependency surface — the two things Go is being chosen for.

**Rejected: Python.** Would keep one toolchain across the abang-ai suite and make a
later Satay adoption nearly free. Both are real, and both are outweighed by the
install story for a daily-driver CLI.

**Rejected: Rust.** Async lifetime bounds and borrow-checker interaction push LLM
coding agents into cyclic compilation-error loops. This is the one language claim in
`agentic-harness-ideas.md` that survives review intact.

**Not decided here: porting Satay to Go.** That is a separate and much larger
question — a runtime with replay, forking, nondeterminism detection and 400+ tests.
Do not conflate "kopicode is Go" with "Satay gets ported." If cuttlefish later proves
durability is essential *and* Python is the wrong host, that is when the port gets
priced.

## Consequences

- No code reuse from satay-runtime or sibei-flow; kopicode carries its own
  toolchain (`go vet`, `staticcheck`, `golangci-lint`, `go test`) and CI.
- Adopting Satay later means an out-of-process boundary or a port, not an import.
  ADR-0002's journal seam is what keeps that door open.
- The suite now spans Python, Rust (sibei-flow core) and Go. Accepted: the products
  share no runtime, only ideas.
- Gate parity with the rest of the suite is the requirement, not tool parity:
  lint + vet + type-adjacent static analysis + layered tests, run pre-push.
