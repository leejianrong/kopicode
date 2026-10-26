# ADR-0040: Tell the model, on the edit's own result, that its edit left code that can never run

*Proposed* 2026-10-27, for the owner to accept or reject. Nothing is built. KOP-232. Builds on [ADR-0006](0006-hash-anchored-edits-and-failure-attribution.md) (edits fail closed, and never land somewhere plausible without a signal), [ADR-0038](0038-in-reply-anchor-translation.md) (a note appended to an applied edit's result) and [ADR-0039](0039-leftover-name-note.md) (a note is information and never blocks). It changes what the model is told, so it is a decision.

## Context

An edit replaces the lines between two anchors. A model that wants to rewrite a function but names only its `def` line as the region writes a replacement that holds the whole new function, and the old body, which was outside the region, stays where it was. The file now has the new function followed by the old function's body. Tests pass if the new code is right, the syntax gate passes because the result parses, and nothing in kopicode looks at what an edit left behind.

Seen in the journals on disk today, 89 distinct sessions and 746 applied edits with an inline diff (qwen/qwen3-coder-next and deepseek, `run --print` and driven):

| Session | What the edit left | Tree check |
| --- | --- | --- |
| Codex-driven s1 (`2026-10-26-codex-driver-s1-s5`), seq 33 | the module docstring twice, and the old docstring and guard after the new `return` in `page_count` | `check.sh` OK |
| Codex-driven s2, seq 16 | the old body of `last()` after the new `chunk()`'s `return` | `check.sh` OK |
| `fp/qwen-s1`, seq 45 | the same shape as Codex s1 | OK |
| `runs/main-3` seq 63, `runs3/lo-5` seq 53, `runs3/lobase-20` seq 63 | the `def names(...)` line was the replaced region and the replacement began with `return`, so the function lost its header and kept the old `return` below the new one | not checked |
| `runs3/lo-12`, seq 100 | a comment and `return` repeated under a rewritten `def` | not checked |

Seven edits of 746 (about 1 percent), in seven sessions. The Codex-driven s1 and s2 trees pass `check.sh` and contain dead code. The Codex note scored s2's R9 `ok` and has been corrected for this.

The detector that found them is a line pair: in the file after the edit, a `return` or `raise` and the next non-blank line at the **same indentation** that is not `else`, `elif`, `except`, `finally`, `case` or a closing bracket, where either line was added by the edit. It matched 7 of 746 edits in Python files; the seven were read by hand and all are real. The other 739 were not read, so the false-positive rate on edits it did not flag is unknown, and a miss (a dead block that is not a `return` pair) is not counted. It is Python only; Go and others close blocks with braces and are a separate question.

Rejected before: failing the edit (it is not "wrong region": the anchors matched, and a rejection on a heuristic costs a recoverable edit when it is a false positive); a diff-tail "echo" check (the last added line equals the next context line), which found 1 of the 7.

## Decision

1. **After an applied `edit_file` to a `.py` file, the tool checks the post-edit text** for the line pair above, restricted to pairs where at least one line was added or moved by this edit, so an old file's own dead code is never reported.
2. **If found, the edit still applies**, and its result gets one appended note: *"kopicode: after this edit, line 12 (`return math.ceil(...)`) is followed at the same indentation by line 13 (`\"\"\"How many pages...\"\"\"`), which can never run. If the old text was meant to be replaced, your range ended short: re-read the file and replace the rest."* Up to three pairs per edit. It never blocks.
3. **It is journaled on `EditApplied`** as a new optional field (`unreachable: [{line, after}]`), because the tree is not the record. An older reader ignores the field.
4. **Detection is always on and the note is off by default.** The field is written whatever the setting, so a run shows how often it fires with no change to what the model sees (the measurement costs nothing). `Config.EditAftercare` (bool, in the harness hash only when set, `--edit-aftercare` on the REPL and `run --print`, a declared key for `serve` and `mcp`: the same shape as ADR-0039) turns the note on. It becomes default only on a measurement.
5. **`kopicode sessions -report` counts them** on its edits line, so a driven-run note can cite them without reading a diff.

## Consequences

- A second model-facing note on an edit result, after ADR-0038's translation note; both are one sentence and neither changes the edit.
- Python only at first. The same check for brace languages needs a block parser, not an indentation rule, and is not proposed here.
- A false positive costs one line of note on an edit that landed. A miss is today's behaviour.
- Not measured: what the note does to a model. The 746 edits show how often the shape occurs, not what a model does when told. That measurement is the first thing to do once the field is journaled.

## Alternative

Close KOP-232 as a documented limit: seven in 746 is rare, the tests usually catch the case where the old code matters, and each added note is a few tokens on every Python edit that fires (about 1 percent of them). The cost of closing is that two driven-run trees which pass the scenario check contain dead code, and that the only thing that found them was a person reading a diff.
