# ADR-0044: Refuse an edit whose replacement already contains the lines after its region

*Proposed* 2026-11-07, for the owner's decision. KOP-236. Nothing here is built. Builds on [ADR-0006](0006-hash-anchored-edits.md) ("edits fail closed"), on [ADR-0040](0040-edit-left-code-behind-note.md) (which noted this damage after the fact and was ignored in 3 of 3 cases) and on [ADR-0039](0039-leftover-name-note.md) for how an opt-in harness change is shaped. It changes when an edit is applied, so it is a decision.

## Context

The damage, seen in qwen3-coder-next's s1 sessions since 2026-10-23: the model replaces a region that ends at a function's `def` line with the whole new function, signature and body. The old body is outside the region and stays, so the file now holds the new function followed by the old body, which no longer belongs to any definition and cannot run. Every such tree passed the scenario's `check.sh`. ADR-0040 detects the Python form and tells the model; it did not change what the model did (3 of 3).

### What a rule keyed on the lines after the region would do

Over every `EditApplied` in the committed journals under `docs/` (1,174 edits, from the `diff` each one journals), for each changed block I compared what the replacement added with the up to three unchanged lines that follow it in the diff:

| rule | edits flagged | of the 15 edits ADR-0040 recorded as leaving unreachable code | other flagged edits |
|---|---|---|---|
| the replacement **ends with** the lines that follow (the example on the KOP-236 card) | 6 | **0** | 6, all damage |
| the replacement **contains**, anywhere, a run equal to the first 2 or more non-blank, non-closer lines that follow | 23 | 9 | 14, all the same duplicate-body damage |
| same, with 1 line instead of 2 | 31 | 9 | 22: the 14, 6 that end with the following line, and 2 that add `import io` above an existing `import io` (harmless) |

Two things follow. The rule on the card catches none of the motivating cases: the damage is a restated block, not a replacement that ends on the next line. And a threshold of two substantive lines flagged no edit I would call fine, over this corpus.

How I judged "damage": the 9 are the edits ADR-0040's detector already marks. The other 14 I read in the diff: each restates the function's signature and body above a copy of the old docstring, `if` and `raise` lines (13 on `shop/pagination.py`, 1 on `textutil/lists.py`). 13 of the 14 come from runs that predate the `unreachable` field, which is why they are not in the 15; I did not check why the 14th, from 2026-11-07, has no record.

What it misses: 6 of the 15 recorded dead-code edits are not this shape (for example a re-indented `return`, `readonly-deepseek/on-5`), so this is part of the problem and not all of it.

Not measured: the rate on a corpus that is not these scenarios. Of the 1,174 edits, nearly all are in the same handful of repositories (`shop/`, `textutil/`, `accounts/`, `export/`) built by the scenario setup, so "no false positives" means none here. A real Go or Rust codebase, where adjacent lines such as repeated `}` blocks or `return nil` repeat legitimately, has not been tried. The closer filter drops lines that are only brackets, `end`, `else`, `fi` or `done`; it does not drop `return nil`, `continue` or `break`, which a Go edit may repeat on purpose.

## Decision (proposed)

1. **`edit_file` refuses an edit whose replacement contains a contiguous run equal to the first N lines after the region**, where the run holds at least two lines that are non-blank and not a lone closer. N is up to three, the lines the unified diff already shows. It is a line comparison in `internal/tools/edit.go`, so it needs no parser and runs for every language.
2. **The refusal is the existing one.** `ErrEditRejected`, the file byte-identical, as for a stale anchor. The text names the repeated lines and says: *the replacement already contains the lines that follow your end anchor; end the region after them, or remove them from the replacement.* The journal records the refusal as it records any other, with the text above, so a reader can count them.
3. **An identical second attempt is applied.** If the model sends the same request again unchanged, the edit goes through and the diff is journaled as now. This keeps a deliberate repeat possible (unrolling a block, copying two lines) without a per-language list of exceptions. The cost is that a model that retries blindly gets what it asked for.
4. **Off by default.** `Config.RepeatGuard` (bool, in the harness hash only when set), `--edit-repeat-guard`, a declared key `edit_repeat_guard` (0|1). It becomes default only on the measurement below.
5. **The guard never reads the model's task, the file's language or any list of names.**

### The measurement that would decide the default

Interleaved off/on pairs, 10 per arm, on s1 with qwen3-coder-next. Read the final trees (not only `check.sh`), the number of refusals, and what the model did after each: sent a corrected edit, sent the same one, or gave up. Then a Go task with 10 runs per arm (KOP-239 supplies the task), to see the guard on code where repeated lines are normal. Zero-token: `make bench-smoke` and the recorded fixtures must be unchanged with the flag off.

If the model sends the identical request after a refusal in most refusals, decision 3 makes the guard a speed bump, and the ADR is marked Rejected in favour of the ADR-0040 note.

## Alternatives

- **Keep the ADR-0040 note.** Measured: shown, not acted on, 3 of 3.
- **The rule on the card** (the replacement ends with the following lines). Catches 0 of the 15 recorded cases.
- **Apply the edit and extend its region to swallow the repeated lines.** Rejected: the edit would land somewhere the model did not name, which ADR-0006 forbids ("never apply somewhere plausible").
- **Refuse any edit that adds a second `def` or `func` of the same name.** Rejected: language-specific, and it does not catch the docstring-and-body form.
- **Run the project's linter after every edit.** Not this ADR; repositories differ, and it is a larger change.

## Consequences

- If accepted, a function in `internal/tools/edit.go`, a flag, a harness key, tests that break the guard and watch them fail, and the two measurements above.
- The refusal text is model-facing, so the system prompt is untouched (it is at its byte budget); a new tool result string moves no hash literal because it is not in the prompt or tool list.
