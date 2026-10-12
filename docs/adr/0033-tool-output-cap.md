# ADR-0033: Capping what the model sees of a huge tool output

*Accepted.* Uses the settlement in [ADR-0031](0031-observation-masking.md) decision 2 (never-truncate protects the record, not the wire view) without waiting for ADR-0031's masking, which is still Proposed. Builds on [ADR-0012](0012-context-compaction-strategy.md) decision 1 and [ADR-0028](0028-stall-detector.md), which also change only the wire. Amends [ADR-0007](0007-model-selection-and-harness-config-shape.md) decision 6 as ADR-0028 did: a new value enters the hash, only when set.

## Context

`run_shell` returns the whole of a command's stdout and stderr, and `grep` up to 200 matching lines however long. A test run or a build log can be hundreds of kilobytes, and every byte rides along on every later request. `read_file` already bounds itself and takes an offset; these two do not. AGENTS.md's "never truncate tool output" exists so the diagnostic that justifies a fix is never clipped from the record; ADR-0031 decision 2 settled that the record and the model's view are different things and the rule protects the record.

KOP-179 proposed "head and tail plus *N lines omitted, read blob X with offset/limit*". ADR-0031 decision 4 found that the model cannot read a blob or `.kopicode`, so a pointer to one is advice it cannot follow. The recovery it can perform is running a narrower command.

## Decision

1. **Past `Config.OutputCap` bytes, a `run_shell` or `grep` result is shown to the model as its start and its end with a note between.** The start gets a quarter of the cap and the end three quarters, because a build or test log puts its verdict last. Both cuts fall on line boundaries when there is one, and never inside a UTF-8 sequence. Output at or under the cap is untouched.
2. **The note is one line**: how many lines and bytes were left out of the middle, and what to do: for `run_shell`, re-run with a narrower command (pipe to `grep`, `head` or `tail`, or run one test); for `grep`, narrow the pattern or the path. It does not point at the journal or a blob.
3. **Only the wire changes.** `journalToolResult` writes the whole output first, as before, and the journal's `ToolResult` holds it all (spilling to a blob past 64 KiB as always). The stall detector reads the whole output. The capped string is a pure function of the output and the cap, so a replay builds the same view. No new journal event.
4. **Only `run_shell` and `grep`.** `read_file` bounds itself, and the edit tools are small and are the model's record of what it changed. A denied call's refusal text is never capped.
5. **`Config.OutputCap`, 0 = off, in the hash only when non-zero.** Every registered arm is 0, so no benchmark number and no recorded hash changes. The REPL defaults it to 32 KiB (about 8k tokens); `--output-cap N` overrides it on the REPL and `run --print`; `serve` and `mcp` stay at 0 as ADR-0028 left them, and can opt in through a declared harness config.

## Rejected

- **A blob pointer in the note**, as the card first proposed: the model cannot follow it (ADR-0031 decision 4).
- **Capping at the journal.** That is the failure AGENTS.md designs out.
- **A line-count cap.** A single minified line can be megabytes; bytes bound the cost of the request.
- **Head only, or an even split.** A failing run's summary is at the end; an even split spends half the budget on preamble.
- **Turning it on for the benchmark arms.** It would move every recorded number's hash for an effect nobody has measured.

## Measurement

No gain is claimed. The 13-task corpus rarely produces output near 32 KiB, so it cannot show an effect either way; `make bench-smoke` confirms nothing moved. The check that matters is on a task whose test log or search is large, which needs a long-output task set and live spend (OpenRouter credit is exhausted as of this writing). Until then this is a cost-control change for people at a keyboard.

## Consequences

- `internal/engine/outputcap.go` holds the pure function; `dispatch.go` applies it after the stall note. `Config.OutputCap`, its hash rule and `--output-cap` follow `StallThreshold`.
- README gains one sentence next to the stall detector's.
