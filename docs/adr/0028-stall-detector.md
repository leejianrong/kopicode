# ADR-0028: A stall detector

*Accepted.* Builds on [ADR-0012](0012-context-compaction-strategy.md) decision 1, which collapsed a run of identical denials. Amends [ADR-0007](0007-model-selection-and-harness-config-shape.md) decision 6: a new value enters the hash preimage, but only when it is set.

## Context

A weak model that is stuck does not stop, it repeats. ADR-0012's dogfood data showed 13 of 22 round trips spent retrying an already-denied call after the task was done. A published harness study (LangChain, vendor data, one benchmark) lists loop detection among the changes that moved a score, though it did not isolate it. SWE-agent's ablations support the neighbouring idea that a harness which stops the agent compounding its own errors helps. There is no measurement of this detector on kopicode's corpus, and the corpus does not yet separate two models, so none is claimed.

## Decision

1. **Two shapes, counted in a row.** The same tool, arguments **and result**, again, with no write that landed in between; or edits to one file (`edit_file`, `edit_file_fuzzy`) that keep failing, again with no landed write in between. Different arguments, a different result, or any successful `write_file`, `edit_file`, `edit_file_fuzzy` or `delete_file` starts the count over.
2. **`run_shell` neither resets the count nor counts as an edit.** It may change the tree and usually does not, and re-running an unchanged failing test is exactly the circle this is for.
3. **At the threshold, a note follows the result**: what has happened, and what to do instead (read again for fresh anchors, do something different, say what is blocking you). It fires once per run of N and then starts over. A denied call is never called a stall; ADR-0012's collapse already handles those.
4. **Only the wire changes.** The call runs, and the journal holds its full, unmodified result. The note is a pure function of the call sequence, so a replay produces it again. This is ADR-0012 decision 1's precedent. A resumed session rebuilds its history from the journal and so does not carry past notes; the detector starts again.
5. **`Config.StallThreshold`, 0 = off, in the hash only when non-zero.** Every registered arm is 0, so no benchmark arm and no recorded hash changes. The REPL defaults it to 3, the smallest count that is not an honest retry. `--stall-threshold N` overrides it on the REPL and `run --print`. `serve` and `mcp` stay at 0 (as ADR-0022 left their limits) and can opt in through a declared harness config.

## Rejected

- **Stopping the session at the threshold.** A nudge costs a few lines; ending the session loses the work done.
- **Counting `run_shell` calls as writes.** It would make the most common stall, a test re-run with nothing changed, invisible.
- **A new journal event.** The wire note is derived; a second record of it is the parallel transcript the journal rule forbids.
- **Turning it on for the benchmark arms.** It would move every recorded number's hash for an effect nobody has measured.

## Consequences

- `internal/engine/stall.go` holds the state, transient and in memory like `lastDispatch`. `runTool` now reports whether the call failed.
- A paired run of an arm with and without the threshold is how to learn whether it helps; until then this is a usability change for people at a keyboard, not a claimed gain.
