# ADR-0038: Translate a stale anchor that this reply's own edit made stale

*Accepted* 2026-10-25 by the owner, to be built and measured as decided below. KOP-222. It changes a sentence of [ADR-0006](0006-hash-anchored-edits-and-failure-attribution.md) decision 1 and touches the "Edits fail closed" boundary in `AGENTS.md`, which is why it is a proposal first.

## Context

An anchor hashes a line with both neighbours (ADR-0006 decision 7), so an applied edit changes the anchor of the unedited line above and the one below it without touching their text. `qwen/qwen3-coder-next` writes all its edits for one file in a single reply, so the second `edit_file` call is written before the first call's result exists. If the second call names a neighbour of the first, its anchor is stale by the engine's own doing and the call is refused as `anchor_drift`.

On `s3` (`rename-across-files`) that is the usual outcome: three refusals at seq 48, 62 and 71 in six of twelve runs, one at seq 73 in three more ([schema note](../dogfood-runs/2026-10-24-kop-222-schema/README.md)). The model recovers, at the price of failed calls and a half-edited tree for a few turns. `deepseek/deepseek-v3.2` has none.

Four remedies have not moved it:

| Remedy | Result |
| --- | --- |
| A prompt line (calls in one reply do not see each other) | no change, one run ([KOP-215 note](../dogfood-runs/2026-10-23-s3-kop-215/README.md)) |
| A sentence in the `edit_file` description | 3,3,1,3,3,0 against 3,0,1,3,1,0 rejections over six runs a side: no difference, and the two zero-rejection runs that failed `check.sh` show the count alone is a poor score ([schema note](../dogfood-runs/2026-10-24-kop-222-schema/README.md)) |
| An `edit_regions` tool (several regions, one read, all or nothing) | 0 of 6 runs called it; 4 of 6 stopped early with `fetch_user` left in four files (not merged; [regions note](../dogfood-runs/2026-10-24-kop-222-regions/README.md)) |
| KOP-215's old to new map in an edit's result | fixes the cross-turn case only; a call already written cannot read a result |

What is left is the one thing a result cannot do: let the engine apply, to a call already written, the map it already computed. That accepts an anchor that matches no line, which is the line ADR-0006 drew, so it needs a decision.

## Decision

1. **The engine remembers, per reply and per file, what its own applied edits re-anchored.** After each applied `edit_file`, the entries the tool already computes (`tools.Reanchor`: the anchor the line had, `Old`, and the anchor it has now, `Now`) are added to a map for that file. The map belongs to one assistant reply (the engine's `turn`), is held in memory, and is dropped when the reply's tool calls are done. It is not carried to the next reply and not written to the journal.

2. **A later `edit_file` call in the same reply, on the same file, may have an anchor translated.** For `anchor_start` and `anchor_end` separately, if and only if all of these hold:
   - the anchor matches **no** line on disk (so today it would be `anchor_drift`);
   - it equals the `Old` of exactly one entry in this reply's map for this file;
   - that entry's line has not been replaced, deleted or moved by a later edit in this reply, and the entry's `Now` is current (see 3);
   - the translated `Now` anchor then resolves to exactly one line on disk by the ordinary rules.

   The anchor is replaced by `Now` and the call proceeds as if the model had sent it. The line is the same line: an anchor hashes that line's text and its neighbours, the text has not changed, and only the neighbouring text did.

3. **The map is kept true as edits land.** When a second edit lands next to a remembered line, that line's `Now` is recomputed (`Old` stays, so a chain A to B to C is `Old` A to `Now` C); when an edit replaces or deletes a remembered line, its entry is dropped. Entries are tracked by line position through each edit, not by searching for hashes. Any change to the file that is not an applied `edit_file` in this reply (a `write_file`, `delete_file`, `run_shell`, an `edit_file_fuzzy`, or bytes on disk that differ from what the last applied edit produced) clears the whole map for that file.

4. **Everything else is refused exactly as now, with the file untouched.**
   - an anchor from an older read that is not an `Old` in this reply's map;
   - an anchor on another file, or in a later reply (a new reply starts with no map);
   - an `Old` that two entries share (a duplicated line: see failure scenarios), or whose `Now` is now ambiguous;
   - an `Old` whose line was edited or deleted in this reply;
   - any anchor that matches no entry; and malformed, ambiguous and out-of-order anchors, which never reach translation;
   - an anchor that does match a line on disk. Translation never competes with a live match.

5. **Each translation is journaled and shown.** `EditApplied` gains an optional `translated_anchors` field (omitted when empty), a list of `{which: "start" | "end", from, to}`. The tool result tells the model it happened ("`anchor_start` 3f2a9c01 was this reply's earlier anchor for line 14, now 7be1d044; used that"), so the model's own next call in the reply can use current anchors. `AnchorStart` and `AnchorEnd` keep echoing what the model **sent**, as they do now ("the engine journals what was asked for"), so the record shows both.

6. **Mode stays `anchored`, and the session is not marked `unattributed`.** See the argument below.

7. **A feature name** is not needed (this is engine-internal and adds no wire method, parameter or consent mode); the `EditApplied` field is documented in the journal schema notes, and the unknown-field rule (events are a compatibility surface) already covers older readers.

8. **In the harness hash.** The rule changes what a given reply does, so a build with it is a different arm from one without. It is folded into the harness configuration hash (a version string, as the anchor version is), so a benchmark never pools the two.

## Should a translation mark the session `unattributed`?

Recommendation: **no.** ADR-0006 decision 3 exists because a fuzzy match can land above the floor and in the wrong place and say nothing. The taint is for matches that are approximate. This one is exact:

- the old anchor is a hash of the three-line window the file had; the engine itself produced the new one from the same line; the pairing is a computed fact, not a similarity score;
- it is refused rather than applied whenever more than one reading exists (decision 4);
- it is journaled per use (decision 5), so a reviewer can find every one, which is what the taint is for.

The case for yes is that the bucket is meant to be biased against us, and a translation is the first time the engine acts on an anchor it did not match. If the owner prefers that, the cost is that every qwen s3 run would land in `unattributed` on failure instead of `model`, which defeats counting it. A middle way is cheap and kept open: the classifier reads `translated_anchors`, and a **failing** session with a translation followed by a failed syntax gate on that file is `harness`-leaning evidence worth a report line, not a bucket change. That is not part of this proposal.

## Failure scenarios, and what the rule does

| Scenario | What happens | Why it is safe, or not |
| --- | --- | --- |
| **Wrong-line landing.** The model means line 14 and sends line 14's old anchor; the translation names line 14 | applied to line 14 | what the rule is for |
| **Wrong line, ambiguous original.** Two lines had the same anchor `X` before the edit (a repeated `}` with the same neighbours). Edit 1 re-anchors one of them. A later call sends `X`, which now matches only the *other* line on disk | applied to the other line, **today and under this rule** | pre-existing fail-open hole: translation never runs because `X` matches a line. It is not introduced here and not closed here. Mitigation inside this rule: an entry is recorded only if its `Old` was unique in the file before the edit, so the map never adds a second reading. Worth its own card |
| **Two edits whose maps conflict.** Edit 1 re-anchors lines 9 and 13; edit 2, adjacent to line 13, re-anchors it again | the entry for line 13 chains (`Old` to the newest `Now`); line 9's entry is untouched | decision 3; the property test below holds chain composition against applying each call on a fresh read |
| **An edit that deletes a neighbour.** Edit 1 replaces lines 10-12; line 13's anchor changes. A later call anchors on line 12's `Old` | refused (`anchor_drift`) | line 12 was in the replaced region, so it has no entry; there is no "same line" to name |
| **A later edit deletes a remembered line.** Edit 2 deletes line 13, which edit 1 re-anchored | entry dropped; a call naming its `Old` is refused | decision 3 |
| **A duplicated line.** Edit 1's new text contains a line identical, with identical neighbours, to a remembered one, so `Now` matches two lines | refused as `ambiguous`, naming both | the ordinary ambiguity rule runs after translation |
| **Two entries share an `Old`.** Two different lines had equal anchors and both neighbour edits | the `Old` is dropped from the map on the second insert | decision 4 |
| **Same anchor string, different file.** A call to `b.py` sends an `Old` recorded for `a.py` | refused | the map is per file |
| **Stale read that happens to equal an entry's `Old`.** An anchor from three replies ago equals this reply's `Old` | translated | the equality means the same three-line window text at the moment the entry was made, and the entry was unique then; the same line unless the window recurs, which uniqueness excludes. Stated as the rule's one honest edge: it trusts the hash |
| **The file changed under us** (`run_shell` between two edits) | map cleared, refused | decision 3 |

The wrong-line risk that KOP-215 measured (a *fresh* anchor is valid wherever it points, so two were spent on the wrong line and applied) does not apply: the model sends an anchor it was given by a read, and the engine only ever maps it to the line that anchor named.

## Guard tests

Written when and if this is accepted, each seen red before it counts:

1. **A stale anchor from an older read is refused and the file is byte-identical**, in a reply that also applied a translated edit. This is the guard that matters most; the break is making translation consult every `Old` the session has ever seen.
2. An anchor that is a neighbour of an edit in the **previous** reply is refused (the map is dropped at the end of a reply).
3. The same anchor sent to another file is refused.
4. A remembered line that a later edit deleted is refused; one a later edit re-anchored translates to the newest anchor.
5. Duplicated `Old` and duplicated `Now` are refused as above.
6. A `write_file`, `run_shell` or fuzzy edit between two edits clears the map.
7. A live match is never translated: an anchor that is both an `Old` and a current anchor takes the live line.
8. A seeded property test over many file shapes (LF, CRLF, no trailing newline, adjacent and overlapping regions, shuffled call order, deletions): for each reply of N edits written against one read, either the call is refused with the file unchanged, or the result equals applying that call on a fresh read of the current file. A translation that ever lands elsewhere fails it.
9. `EditApplied` carries `translated_anchors` and only on a translated call; `Mode` stays `anchored`; the bench classifier does not call such a session `unattributed`.
10. Replay: a recorded qwen s3 session replays byte-identically under the new arm hash.

## Measurement (before any claim)

The scenario is `s3` on `qwen/qwen3-coder-next`, the Claude Code driver, six runs on the change against six runs of `origin/main` on the **same day**, provider pinned (`docs/provider-pin.md`). The numbers that would justify it:

- `anchor_drift` rejections near 0 in the change arm (the baseline mode is 3);
- `check.sh` OK in every run, as many as the baseline's;
- **no wrong-line landing**: every `translated_anchors` entry in every journal is checked by hand against the diff of that call (the line the model's own prior read shows under that anchor), and any miss ends the proposal;
- turns and tokens no higher than baseline, since the point is that the failed calls and their repair turns disappear.

One run per cell is a lead, not a result; six a side is the floor, and the schema measurement shows that count alone can look better while `check.sh` fails, so the second and third bullets carry the verdict. `make bench-smoke` stays green with the new hash. Deepseek s1 to s5 are rerun once to show no change where there was no drift.

## What it changes

- **ADR-0006 decision 1**, the bullet "If any referenced anchor no longer matches the file, the edit is **rejected** with the current anchors for that region. A drifted file can never be silently corrupted." gains an exception, in the ADR's own words if accepted: *the one case that is not rejected is an anchor that an earlier edit in the same assistant reply made stale, which the engine translates to the line's current anchor and journals.* "Anchors are only obtainable from a read" stays true: the engine only ever maps an anchor from a read.
- **`AGENTS.md`, "Edits fail closed":** "An `edit_file` whose anchors no longer match the file is rejected, never applied somewhere plausible." The rule would carve out a single sentence-sized exception, and the exception is the whole risk. The text should say: unless the anchor is an entry in the current reply's own re-anchor map, in which case it is translated and journaled. The final sentence of that boundary ("Never add an edit path that can land in the wrong region without emitting a signal") holds: every translation emits one.
- ADR-0006 decision 3 is **not** changed (no new `unattributed` trigger), unless the owner chooses the other answer above.
- KOP-215's result text and prompt bullet stay; they serve the cross-turn case.

## Alternatives

- **Close KOP-222 as a documented qwen limitation.** Real, and cheap: nothing changes, the fail-closed boundary stays word for word, and the failure is recoverable and visible (a refused call, never a wrong tree). Deepseek has 0 rejections on s3, and the project's order puts cuttlefish-crew and the driver loop first. The cost is that the common qwen outcome stays three refusals and a few extra turns on every multi-edit rename, and a fleet on a cheap model pays that on every such task. If this ADR is rejected, that is what happens: the card is closed with that comment and the four measurements above linked.
- **Re-read and retry in the engine** (on a drift, re-read the file and re-run the call against current anchors). Rejected: that is "apply somewhere plausible" with no link to the line the model meant.
- **Apply a reply's edits as one batch** (the engine collects every `edit_file` for a file in a reply and applies them against one read). Sound in principle, the same guarantee as `edit_regions`, and needs no stale anchor accepted. Rejected for now: it changes when calls run and what each call's result says (a later result would arrive before an earlier write), and the journal's order of events would no longer be the order of effects. Worth revisiting if the translation rule is rejected.
- **Wider anchor window / drop the neighbour hash.** Rejected by ADR-0006 decision 7 on the numbers.
- **Another prompt or schema sentence.** Measured twice, no effect.

## Consequences

- One engine-side map per reply, one optional journal field, one line in the tool result, one hash input.
- A model-facing contract grows by one fact: an anchor the engine re-anchored in this reply still works for the rest of the reply.
- The fail-open hole in the ambiguous-original row is documented, not closed.
- Not checked and not claimed: other models, other scenarios than s3, parallel edits to one file from more than one tool call dispatcher (calls in a reply run one after another today; the rule depends on that and a change would have to revisit it).
