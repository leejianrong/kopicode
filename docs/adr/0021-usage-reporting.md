# ADR-0021: Usage reporting

- **Status:** Accepted (2026-10-07)
- **Date:** 2026-10-07
- **Deciders:** Jian (leejianrong2@gmail.com)

Answers [issue #189](https://github.com/leejianrong/kopicode/issues/189) and adds a
`/context` command to the REPL.

## Context

cuttlefish-crew drives `kopicode serve` and cannot show a person what a run has cost or enforce a
dollar ceiling: the stream carries only `provider_response.size`, the total. The prompt/completion
split is in the journal and never leaves it. A person at the REPL has the same gap, and there is a
second, more important one for long runs: **the number that says how full the context is does not
exist anywhere.** The engine sums usage across requests for the token budget, but each request
resends the whole history, so that sum grows much faster than the context and says nothing about the
model's window.

Recorded OpenRouter traffic already in the repo shows the provider returns `usage.cost` and
`prompt_tokens_details.cached_tokens` in the streamed usage block with no request change.

## Decision

**1. Two numbers, never conflated.** *Context in use* is the prompt tokens of the latest request.
*Spent* is the sum over every request. Both are reported, named differently, and documented as
different. A client deciding when to checkpoint reads the first; one enforcing a budget reads the
second.

**2. Unknown is absent, not zero.** The provider's `cost` is recorded when present and omitted when
not; a session sum is reported only if *every* request reported one, so a gap makes the total
unknown instead of quietly partial. The model's context window is a field on the registry row,
filled only from `docs/provider-pin.md`, `0` meaning unknown, and a surface prints "not known"
rather than guess. Neither is estimated, and no price table is built.

**3. No request change.** The decode reads fields the provider already sends. The harness config
hash is untouched: `ContextWindow` is model metadata on `Entry`/`Selection`, not behaviour, and the
registry test that pins the default hash still passes.

**4. Journal, additive.** `TokenCounts` gains `cache_read` and `cache_write`, `ProviderResponse`
gains `cost_usd`, all omitted when absent. Schema version unchanged.

**5. Wire.** `usage` on each `provider_response` event (the split; `size` stays the total), on every
turn result, and as the result of a new `session.usage` method that answers while a turn is running.
`mcp` turn outcomes carry the same object. Features: `session.usage`, `usage.tokens_split`,
`usage.context`, `usage.context_window`, `usage.cost`. The engine's usage numbers are guarded by a
mutex, because `session.usage` is the first reader on another goroutine.

**6. REPL.** `/context` prints the report from the same engine accessor, so the two surfaces cannot
disagree. It is a loop command and never reaches the model.

## Not decided here

Per-session `max_turns` and `token_budget` on `session.start`, and the interactive default turn cap,
are a separate change. They are reported here (read-only) so a client sees the bounds it is under.
Compaction is out of scope (ADR-0012).

## Consequences

A client can show spend and cap it whenever the route reports a cost, and can checkpoint at a
fraction of a known window. For a model whose window or cost is unknown the fields are simply absent,
which a client has to handle; that is the cost of not guessing. `deepseek/deepseek-v3.2` and
`z-ai/glm-5.2` report no window until `docs/provider-pin.md` records one.
