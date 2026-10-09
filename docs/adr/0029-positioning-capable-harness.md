# ADR-0029: A capable coding harness for the models people use, fed first by cuttlefish-crew

*Accepted.* Supersedes the thesis in [`PRD.md`](../PRD.md) ("a harness you can tune per model, proven by the benchmark") and its "Not planned" list. It does not touch ADR-0008's, -0011's, -0016's or -0017's consent rules, or any rule in AGENTS.md's boundaries section other than the one it names below.

## Context

The project began as a bet: harness quality moves a cheap open-weight model's measured ability by a large margin, and a benchmark rig proves it. Two things since then. The 13-task corpus does not separate the models it was meant to compare, so the bet is still unmeasured. And the product has two consumers that care about something else: cuttlefish-crew, which runs coding agents across 20 to 100 projects and needs kopicode to be dependable under that load, and a person at a keyboard who compares it with Claude Code, Codex CLI, opencode and oh-my-pi.

The "Not planned" list (LSP, IDE integration, image tools, browser control, memory, and others) was justified by one sentence: each item is orthogonal to making a cheap model code reliably. That is the wrong test for the product described above.

## Decision

1. **Positioning.** kopicode is a capable coding harness that performs well across the major coding models (Claude, GPT/Codex, GLM, DeepSeek, MiniMax, Qwen and similar), reachable through several surfaces: the REPL, `run --print`, `serve`, `mcp`. The multiple surfaces, and `serve` in particular, are part of the pitch. "Tunable and proven by bench" is no longer the pitch.
2. **Two goals, in this order.** (a) Be what cuttlefish-crew needs: dependable under a fleet, drivable over `serve`, cheap to run (prompt caching, output caps, handoff). (b) Close the gap to Claude Code first, then Codex CLI, opencode and oh-my-pi, following what those tools have already settled rather than reinventing it. When the two compete for time, (a) goes first.
3. **The non-goals list is retired.** A feature is admitted by whether users of the reference harnesses value it and whether it can be built within the structural rules below, not by whether it moves a benchmark score. Each still gets its own ADR when it changes a surface or the consent model.
4. **The structural rules stay.** The journal is the only record; edits fail closed; tool output is never truncated in the journal; consent is never loosened into prefix matching and `auto` is not a sandbox; every subprocess names its directory and builds its environment; the user's git state is never touched; `internal/` is the engine boundary and front ends use only the three allowed packages; a `serve` wire addition ships with a feature name. Dependencies stay few, with a reason in the PR, and no diff library.
5. **The benchmark rig stays, with a smaller job.** `kopibench` is the regression and A/B check for harness changes. The rule "pin the provider on every benchmark request" and "do not claim gains that were not measured" stay. Neither is a reason to refuse a feature people value. A claim of improvement still needs a measurement, and a feature that has none is described as unmeasured.
6. **Providers.** OpenRouter and a custom OpenAI-compatible URL exist (ADR-0027). Direct Anthropic, Z.ai and Codex OAuth are wanted; each is a spike or ADR of its own, and none feeds a benchmark number.

## Consequences

- PRD scope and AGENTS.md's opening paragraph are updated to match; the README's tagline follows when it is next edited.
- The board gets two epics: cuttlefish readiness, and parity with the reference harnesses (starting with an audit of what Claude Code does that kopicode cannot yet do).
- Work already in flight (handoff, stall detector, caching, custom provider) stays; it serves both goals.
- Some features duplicate things cuttlefish built for itself (stuck detection, handover). That is accepted: kopicode's REPL needs them, and cuttlefish can adopt them when they are measured to be at least as good.
