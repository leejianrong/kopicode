# ADR-0031: Observation masking in the model's view

*Proposed.* Reopens, on different grounds, the direction [ADR-0012](0012-context-compaction-strategy.md) decision 2 rejected, and competes with [ADR-0026](0026-handoff.md) for the same job. It amends nothing until it is accepted. Whether to reopen it is the owner's call, and the Context says why it is a fair question.

## Context

**What was rejected before, and why.** ADR-0012 decision 2 proposed redacting a stale `read_file` result once a later edit and a passing verification had superseded it. It was rejected as a shape: piecemeal redaction of history already appended is the wrong thing to spend effort on, and a handoff document plus a fresh context was preferred. That preference became ADR-0026. Nothing in the rejection disputed the reasoning about *when* a result is safe to hide.

**What has changed.**

- [ADR-0029](0029-positioning-capable-harness.md) retired the "tunable, proven by bench" thesis. The stated order is now: be cheap to run under a fleet, then follow what Claude Code, Codex CLI, opencode and oh-my-pi have settled. Hiding old tool output is the one context technique the survey (`docs/research/2026-10-harness-survey.md` §2) found with a published number: JetBrains (arXiv 2508.21433, Qwen3-Coder on SWE-bench Verified) hid old observations and matched or slightly beat LLM summarisation at about half the cost. SWE-agent's ablation (GPT-4 Turbo, SWE-bench Lite) showed the last five observations scoring 18.0 against 15.0 for full history. Both are single studies on other models and other harnesses, not on kopicode's corpus or on a weak open-weight model.
- ADR-0026's own context says the benefit in that literature is not the summary, it is hiding the old output. Handoff and masking are two answers to one problem and nobody has measured either here.
- Prompt caching (KOP-172) is in: a stable `session_id` and a byte-stable prefix, with `cache_read`/`cache_write` recorded per request. Masking edits the prefix, so it now has a price that did not exist when ADR-0012 was written.

**What the code does today.** `Assembler` ([`internal/engine/context.go`](../../internal/engine/context.go)) sends every message, oldest first, unmodified. Its doc comment names "nothing here truncates" as a settled property, held by `TestAssemblerNeverTruncates`. `docs/token-growth.md` measured two real sessions, and growth was concentrated in a few large early `read_file` results, not spread by age.

**The cost, worked through.** The survey's cache table gives DeepSeek reads at 0.1x input with writes at normal input price. Take a context of S tokens in which a batch of M tokens starting at position p is masked. Every token from p onward changes, so it is a cache miss and is billed at full input price once instead of at 0.1x: an extra cost of about 0.9 (S - p - M) token-prices, where p is where the first masked result sits. Afterwards each turn saves 0.1 M. Masking the *oldest* results puts p near the start, so S - p is nearly the whole context. With M half of S, break-even is about nine more turns. This is my arithmetic from the survey's table, not a measurement, and it gives the shape of the answer: **on a provider with cheap cache reads, masking saves little money per turn and pays only over many later turns; on a provider with no working cache, or when the window is the binding limit, it pays at once.** The JetBrains figure should not be read as a number for a cached DeepSeek route until it is measured on one.

## Decision

1. **Mask in the wire view only. The `Assembler` keeps every message whole, and the journal is untouched.** The request builder produces the model's view from the assembler's messages through a pure function of those messages and the mask boundary. Nothing is written back, so ADR-0012's rule that compaction is a question about the next request, never about the record, holds. A replayed session builds the same view.
2. **The "never truncate" question.** The rule exists to protect the record and the diagnostic output a human or a later session inspects ([AGENTS.md](../../AGENTS.md)). The record stays whole, so the rule stands for it. `context.go`'s doc comment and `TestAssemblerNeverTruncates` are narrowed in the same PR that builds this, to say that the *assembler* never truncates and that the request view may stub a result under decision 4, with the exclusions in decision 3. This is the narrowing ADR-0012 decision 4 proposed and nobody made.
3. **What is masked, and what never is.** Only tool *results* are masked. Assistant messages, tool calls and user messages stay whole, so the model keeps the story of what it did and why and loses only the bulk it read. Never masked:
   - results from the newest K turns (default 10; SWE-agent's ablation used five, and the right K is a thing to measure, not a figure taken from either paper);
   - **an unresolved diagnostic**: the latest failing `run_shell` result, and the latest failing verification, stay whole until a later passing one exists. This is ADR-0012 decision 4's concern kept, drawn at "could still justify an unresolved change", not at "looks old";
   - any result of `edit_file`, `edit_file_fuzzy` and `write_file` (small, and they are the model's record of what it changed).
   Age alone triggers masking, not size alone and not a guess at relevance. The measurement in `docs/token-growth.md` says age alone keeps the wrong things; the K-turn window and the diagnostic exclusion are the answer to that, and the measurement below is where it is checked.
4. **The stub names the call and how to get it back.** A masked result becomes one line: the tool, its arguments, the original size in bytes, the journal sequence number, and the sentence "output omitted from this view; repeat the call to see it again". The model cannot read `.kopicode` (a recursive `list_dir` skips it by design, `internal/tools/list.go`), so a pointer to a blob path would be a pointer it cannot follow. The recipe it can follow is to re-run the call; the sequence number is for the human and for a later session. Nothing about the mechanism discourages a re-read.
5. **Batching protects the cache.** Between steps the view is append-only, so the prefix is byte-stable. The mask boundary advances only when the results it would newly hide total at least B tokens (default a quarter of the model's window when known, else 20,000 estimated tokens), and then in one step to the newest turn that leaves K whole. The boundary only moves forward: a result once masked stays masked, which is what makes the view deterministic and replayable. Each step is journaled as an event naming the boundary, the results masked and the estimated tokens removed, so cost and cache hits can be read against it. One step costs one cache rewrite, as computed above.
6. **Opt-in, and in the hash only when set.** `Config.MaskAfter` (the K), 0 = off, enters the harness config hash only when non-zero, as ADR-0028 did for `StallThreshold`. Every registered benchmark arm stays 0, so no recorded hash moves. The REPL, `serve` and `mcp` stay off until a measurement says otherwise. Flag and wire names are settled in the build card, with a `serve` feature name per AGENTS.md.
7. **It competes with handoff, and they are measured as separate arms.** Masking delays the day the window fills; handoff ends the session when it does. They may turn out to be complements or substitutes, and this ADR does not choose. cuttlefish hands off at 75% of the window by its own policy, which is the caller's to keep.

## The measurement that decides

No claim of a gain is made. The build ships the mechanism off, and a paired run decides whether it turns on anywhere.

- **Arms:** full history, masked, handoff, masked plus handoff; same model, same pinned provider route, same tasks, per [ADR-0007](0007-model-selection-and-harness-config-shape.md).
- **Metrics:** solve rate; **billed cost computed from `cache_read` and `cache_write`**, not from prompt tokens, because the point of decision 5 is that the two differ; peak context; turns; count of masked results the model later re-requested (a high count says the window was too small or the exclusions too narrow).
- **The corpus does not exercise it.** The 13 tasks run 2 to about 8 turns at the benchmark cap, and K is 10, so masking would never fire. A long-task set is needed first. The mock provider can prove the mechanics at zero token cost (determinism, boundary monotonic, stub text, hash unchanged when off) and cannot say anything about benefit.
- **Cost of the real runs:** the OpenRouter account is out of credit as of this writing. Nothing here is run until that is topped up.

## Rejected

- **Masking in the assembler, or in the journal.** The journal is the record; the assembler is what ADR-0012 and ADR-0026 call untouched. A view built on the way out keeps both.
- **Masking on every turn.** Each mask rewrites the prefix from the first changed message. Doing it per turn is a permanent cache miss, the one outcome the batching exists to avoid.
- **Summarising the masked result with a model call.** The survey's finding is that the summary is not what carries the benefit; a second model call adds cost and breaks replay's determinism (ADR-0012's reason, unchanged).
- **A pointer to a blob path as the stub's recovery step.** The model cannot read it. See decision 4.
- **Masking by size alone.** ADR-0012 rejected it for stripping the largest, most relevant context at the worst time. The newest-K and unresolved-diagnostic exclusions are there for the same reason.

## Consequences

- When built, the card touches `internal/engine` (a view function beside `Assembler`, a new journal event for a mask step, the config field and its hash rule) and the docs for the event and the wire, and rewrites two sentences of `context.go` and one test's scope. It needs the event type added to the event schema's compatibility surface, versioned like the rest.
- ADR-0012 decision 2's status line is updated to point here if this is accepted. It stays rejected as a design for *in-place* redaction; this ADR is a different shape (a wire view).
- KOP-179 (cap what the model sees of a huge tool output) is the same family and the same "never truncate" question. This ADR's decision 2 settles the question for both; KOP-179 stays its own card, because capping a result *as it is produced* and masking one *as it ages* differ in when the cost is paid.
- KOP-181 (send Qwen `cache_control` after a live check) changes what a prefix rewrite costs on that route. It lands before any measurement here.
