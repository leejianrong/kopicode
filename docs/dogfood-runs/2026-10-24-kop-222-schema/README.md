# KOP-222: does telling the model in the `edit_file` schema stop in-reply anchor drift? (2026-10-24)

An anchor hashes a line with both neighbours (ADR-0006 decision 7), so an applied edit re-anchors the line above and below it. `qwen/qwen3-coder-next` writes all its edits for a
file in one reply, so a later call can name a neighbour whose anchor the first edit just changed, and it is refused as `anchor_drift`. KOP-215 reports the new anchors in an edit's
result, which a call already written cannot use. This round tried the cheapest remedy the card listed that had not been tried: the `edit_file` description.

## The change (not shipped)

One sentence appended to the `edit_file` description in `internal/harness/harness.go`:

> Calls in one reply run in order, and an edit changes the anchors of the line above and below its region, so a later call anchored on those lines is rejected: change
> neighbouring lines in one call, spanning them from anchor_start to anchor_end.

Matching was not touched. The code change was reverted after the measurement (the two harness hash literals moved with it and moved back).

## Method

s3 (`rename-across-files`) on `qwen/qwen3-coder-next`, the Claude Code driver (Haiku, `kopicode-driver`) as before, `consent_mode: "auto"`, a fresh scratch repo per run from
`docs/agent-driver/scenarios/setup.sh`. Two builds, run side by side on the same day: **baseline** is `origin/main` at c9d594a, **schema** is that plus the sentence above. Rejections and
turns are `kopicode sessions -report` over the journal in each folder; `check.sh` is the scenario's tree check.

## Result

| run | rejected (`anchor_drift`) | applied | turns | tokens | `check.sh` |
| --- | --- | --- | --- | --- | --- |
| baseline-2 | 3 | 11 | 6 | 43,864 | OK |
| baseline-3 | 3 | 11 | 6 | 43,828 | OK |
| baseline-4 | 1 | 11 | 8 | 59,137 | OK |
| baseline-5 | 3 | 11 | 6 | 43,518 | OK |
| baseline-6 | 3 | 11 | 6 | 43,845 | OK |
| baseline-7 | 0 | 10 | 7 | 46,231 | **FAIL** |
| schema-1 | 3 | 11 | 6 | 43,834 | OK |
| schema-2 | 0 | 11 | 8 | 55,196 | **FAIL** |
| schema-3 | 1 | 13 | 9 | 70,434 | OK |
| schema-4 | 3 | 11 | 6 | 43,826 | OK |
| schema-5 | 1 | 12 | 10 | 80,888 | OK |
| schema-6 | 0 | 10 | 7 | 48,591 | OK |

Rejections per run: baseline 3, 3, 1, 3, 3, 0 (mean 2.2); schema 3, 0, 1, 3, 1, 0 (mean 1.3). Six runs a side is too few to call a difference: the usual outcome is still three rejections in both arms
(four of six baseline, two of six schema), and the schema arm's lower runs cost more turns (8 to 10 against 6). `baseline-1` is not in the table (see below).

- **Three rejections is a mode, not noise.** Six of the twelve runs are rejected at exactly seq 48, 62 and 71 (`accounts/greeting.py`, `accounts/store.py`, `tests/test_accounts.py`; baseline-2, -3, -5, -6 and schema-1, -4), and
  the three one-rejection runs (baseline-4, schema-3, schema-5) are each rejected at seq 73 on `tests/test_accounts.py`. The model's replies repeat closely at this temperature and seed.
- **Zero rejections is not the same as clean.** baseline-7 (0 rejected, 10 applied) and schema-2 (0 rejected) both failed `check.sh`: `fetch_user` is left in `tests/test_accounts.py`
  (schema-2: `test_lookup_goes_through_fetch_user`, a test name, with the tests passing). Skipping an occurrence avoids the rejection, so the rejection count alone is a poor score for this scenario.
- **The new text did reach the model**: the tool catalogue is part of the harness config hash, which moved. Whether the model read it is not something the journal shows.

## Verdict

No demonstrated effect, so nothing is shipped. KOP-222's first remedy is tried and does not move the common case. The second candidate on the card, one `edit_file` call carrying several regions of one file
applied together against one read, is untried; it does not accept a stale anchor (every region is checked against the same file state), but it only helps if the model uses it instead of sending separate calls,
and it adds a tool shape, so it is left for a decision rather than built here.

## Also in this folder: a stream that went silent on qwen (KOP-221)

`baseline-1/events.jsonl` ends at seq 10, a `ProviderRequest` for turn 2 with no response and no `SessionEnded`, and stayed that way until the driver's 600 s timeout. The model is `qwen/qwen3-coder-next`
(KOP-221 was filed from two deepseek hangs), so the silent stream is not specific to one model. That run is not counted above.

## Not checked

One model, one scenario, one temperature/seed (0 and 1) and one driver. The other scenarios were not rerun with the schema text. Cost of the twelve counted runs plus the hung one: about $0.065 of kopicode, and Haiku for the drivers.
