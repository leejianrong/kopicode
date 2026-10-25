# ADR-0039: Tell the model, after verification, that a name its edits removed is still in the tree

*Proposed* 2026-10-25, for the owner to accept or reject. Nothing is built. KOP-226. Builds on [ADR-0034](0034-reminders-todo-and-turn-warnings.md) (wire-only notes appended to what the model is shown next) and on forced verification (`Config.Verification`, `internal/engine/loop.go`). It touches no boundary in `AGENTS.md`; it does change what the model is told, so it is a decision.

## Context

On `qwen/qwen3-coder-next` scenario s3 (rename `fetch_user` to `get_user` across code, tests, docstrings and README), some sessions end with the old name still in the tree, the tests passing, and a final message saying the rename is done. Verification cannot catch it: the tests do not mention the leftover. All 23 s3 runs of 2026-10-25 on this model, counted from the journals and `check.sh`:

| Run | What happened | `check.sh` |
| --- | --- | --- |
| `base-1`, `trial-new-0` | one `edit_file` (README), verification passed, "I've successfully renamed ... throughout the repository"; `fetch_user` left in four files | FAIL |
| `main-5` | 11 edits applied; `# fetch_user is the only lookup we have.` left in `accounts/greeting.py`; final message lists that file as updated | FAIL |
| `main-7` | 10 edits applied; a leftover in `tests/test_accounts.py`; final message says every occurrence is renamed | FAIL |
| the other 19 | whole rename | OK |

Four of 23. The two earlier KOP-222 notes show the same shape (`baseline-7`, `schema-2`, and four of six `default-regions` runs), on builds that differ from each other. Raw runs: [`2026-10-25-kop-222-translation`](../dogfood-runs/2026-10-25-kop-222-translation/README.md) and the ten `main-*` runs of this card (to be added to the same folder if this is accepted). Nothing tells the model, because the engine does not look at the tree after the model stops reading it.

Rejected before: parsing the task text for identifiers (KOP-226's own "not in scope"), because a free-text task is a guess the harness should not make (the same reasoning as [ADR-0037](0037-protected-paths.md)).

## Decision

1. **The engine derives the names to look for from its own journal, not from the task.** Over the edits applied in the current prompt (`EditApplied` diffs, anchored and fuzzy), a *removed name* is an identifier (`[A-Za-z_][A-Za-z0-9_]{3,}`) that appears in the removed lines of at least two applied edits and in no added line of any applied edit in that prompt.
2. **After a forced verification that passed, a note is appended to the observation the model is shown** (the same place as the existing one-line pass summary): for each removed name still present as a whole word in the tree, up to three names and five locations each, as `file:line`. Wording, one example: *"kopicode: `fetch_user` was removed from 9 lines by your edits and still appears at accounts/greeting.py:6. If this change should be complete, update it; if it should stay, say why in your closing summary."*
3. **At most once per name per prompt**, so a model that decides to keep one occurrence is not nagged. A failed verification carries no note (it already sends the whole output).
4. **The search is the engine's, not the model's**: the same walk and ignore rules `grep` uses, run on the tree at that moment, with `.kopicode/` and the repository's ignored paths excluded. A search that cannot run is skipped and said so on the journal event, never read as "clean".
5. **It is journaled, because the tree is not.** A new event `LeftoverNoted {names, locations}` records what was shown, so a replay shows the same observation. It is a new event type; unknown types are preserved by older readers (events are a compatibility surface).
6. **Off by default.** `Config.LeftoverNote` (bool), in the harness hash only when set, a `--leftover-note` flag on the REPL and `run --print`, and a declared-config key for `serve` and `mcp`, the same shape as ADR-0028 and ADR-0034. It becomes default only on a measurement.
7. **It never blocks and never changes a stop.** The model may finish as it was going to; the note is information.

## Failure scenarios

| Scenario | Outcome |
| --- | --- |
| A deliberate removal: the model deletes `old_helper` calls but keeps its definition for another caller | the note fires once; the model says why in its summary. One extra turn |
| A name removed by two edits and added back elsewhere in the same prompt | no note: it is in an added line |
| A common word (`result`, `value`) removed twice and still everywhere | would fire. The length floor and the "never added" rule do not exclude it; the cap of three names and the once-per-name rule bound the cost. It is the main false-positive risk and is what the measurement counts on s1, s2 and s4 |
| An identifier inside a string or comment | counted, deliberately: s3's leftover in `main-5` is a comment |
| An edit with only one removal of a name | no note (the threshold is two removed-line edits); a single-edit rename is not detected, and `base-1` made exactly one edit before stopping, so it would not be caught by this rule. Stated, not hidden: see below |
| The tree is large | the walk is bounded by the existing `grep` limits; over them, skipped and journaled |

**What it does not catch.** Four of the four failures above would be seen only if their removed name met the two-edit threshold. `base-1` and `trial-new-0` made one edit; the threshold of two would miss them. Lowering it to one would also fire on every single-line fix that touches an identifier. The ADR keeps two and accepts missing the one-edit stop; the alternative below is the way to reach it.

## Measurement (before any claim)

Same scenario, model and provider pin as the table above, `run --print`, a same-day baseline build:

- **qwen s3, 20 runs a side.** The base rate is about 17% (4 of 23), so ten a side cannot tell 2 from 0; twenty can show a clear fall and still be a lead, not a ranking.
- Count: sessions ending with the old name present (`check.sh`), turns, cost, and whether the note fired and was acted on.
- **False positives:** s1, s2, s4 on qwen and deepseek, one run each at minimum: the note should not fire, and every firing is listed with the removed name and what the model did.
- `make bench-smoke` green with no hash change when off.

## Alternatives

- **Close as a documented qwen limit.** Cheap and honest: about 17% of runs on this one scenario end with a leftover that the tests do not see, no other scenario shows it, and deepseek's s3 runs were all clean on `check.sh`. The cost is that a fleet on a cheap model ships those silently.
- **A one-line prompt reminder** ("after a rename, search the tree for the old name"). The prompt is at its byte budget, and four earlier sentence-level changes on this model moved nothing (KOP-215, KOP-222).
- **Block the stop until the search is clean.** Rejected: it changes a stop into a loop and a deliberate keep would need an override.
- **Let the model grep.** It already can, and these runs show it does not before finishing.
- **Parse the task text for the identifier.** Rejected above.

## Consequences

- One engine function over journal events and a tree walk, one note in the verification observation, one new event type, one config value, one flag, one hash-when-set rule.
- Cost when it fires is one extra turn; when it does not, a bounded walk after each passing verification that followed an edit.
- A claim that it helps needs the measurement above; nothing here asserts it.
