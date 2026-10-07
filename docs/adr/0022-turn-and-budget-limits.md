# ADR-0022: Turn and budget limits

*Accepted.* Answers the long-run questions in the v0.3.0 feedback; builds on ADR-0005 §6, ADR-0007 and ADR-0010.

## Context

`MaxTurns` (20) and `TokenBudget` (2,000,000) live in the harness configuration and so in the hash. 20 is the benchmark corpus's cap (ADR-0005 §6) and must not move. Two things were found in use:

- The cap counts turns **per prompt**, not per session. Each new prompt starts at zero, so a person can always continue. That is fine for the REPL, but 20 is a low leash for real work.
- The budget is **cumulative** over the session and never resets. Every later prompt stops with `budget_exhausted`. A long-lived `serve` session driven by cuttlefish will hit it, and the only remedy was a declared config file, which `session.start` cannot name per session without writing files.

## Decision

1. **Two defaults.** The built-in configuration keeps `MaxTurns = 20` for the corpus, `run --print`, `serve`, `mcp` and `kopibench`. The REPL defaults to 100 turns per prompt (`harness.InteractiveMaxTurns`), unless `--max-turns` is given or the harness is a declared config, which chose its own.
2. **Per-session overrides.** `--max-turns` and `--token-budget` (REPL, `run --print`) and `max_turns` / `token_budget` on `session.start` and `kopicode_start`. `token_budget: 0` means unbounded; an unset budget keeps the default. `max_turns` must be positive. kopibench has no such flags: its arms are fixed.
3. **The override is honest.** It amends the resolved configuration and the hash is recomputed, so a session with a longer leash is a different arm and never pools with the corpus runs. The REPL's 100 is therefore a different arm from the default, which is correct.
4. **The budget stays per session, not per prompt.** A per-prompt budget would let a looping session spend without bound across prompts. A caller that wants a fresh allowance starts a new session or sets a larger one.
5. **Stops are documented as recoverable or not.** `max_turns`: the session is intact; send another prompt. `budget_exhausted`: the session cannot continue; start a new one.
6. **`serve` and `mcp` do not change default.** Changing it silently would alter cuttlefish's behaviour. They opt in with the new fields. Feature name: `session.limits`.

## Rejected

- **Compaction** to stretch long sessions: rejected by ADR-0012 and still so.
- **Raising the built-in default to 100**: moves every benchmark arm.

## Consequences

Long unattended runs need a plan: set `max_turns` high, treat `budget_exhausted` as end-of-session, and read `session.usage` (ADR-0021) to see how much is left before it happens.
