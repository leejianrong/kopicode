# KOP-226: the leftover-name note on qwen s3, 20 runs a side (2026-10-25)

The measurement [ADR-0039](../../adr/0039-leftover-name-note.md) asked for. Two binaries built the same day from adjacent commits, **lobase** (v0.5.0-42-ge31acb1, before the change) and **lo**
(v0.5.0-43-gb55760f, the change, run with `--leftover-note`), `qwen/qwen3-coder-next`, `run --print`, a fresh s3 scratch repo per run, interleaved lobase, lo, lobase, lo, ... Provider pinned as the
journals record. `check.sh` is the scenario's tree check. Not a driver run, so not pooled with the other KOP-222 notes.

## Result

| | runs | `check.sh` FAIL | note fired | mean turns | `anchor_drift` |
| --- | --- | --- | --- | --- | --- |
| lobase | 20 | 2 | n/a | 6.0 | 0 |
| lo (`--leftover-note`) | 20 | 1 | 5 | 7.3 | 0 |

- **Every firing named a real leftover, and the model acted on it every time.** In five runs the note named `fetch_user` still at one place (`tests/test_accounts.py:15` three times, `accounts/greeting.py:6` twice). In each the model's next message
  was to fix that place. Four ended `check.sh` OK. `lo-6` fixed the named comment in `greeting.py` and still left `tests/test_accounts.py`: the note fires once per name, so nothing told it about the second place.
- **The count is a lead, not a result.** 2 failing of 20 against 1 of 20, on one scenario and one model. The two baseline failures (`lobase-15`, `lobase-16`) are the shape this was built for: ten edits, four turns, `fetch_user` left in the tests. Whether the four runs the
  note repaired would have failed without it is not known; the base rate on this build is about 10%, so about two in twenty would have.
- **Cost:** 1.3 turns more on average (7.3 against 6.0), which is the repair turns plus `lo-6`'s ten.
- **No false positive on s1, s2 or s4**, one run each on qwen and deepseek with the flag on (`fp-*` below): the note never fired and every `check.sh` passed.
- The one-edit stops (`base-1`, `trial-new-0` in [the earlier note](../2026-10-25-kop-222-translation/README.md)) did not occur in these 40 runs, so the case the ADR says it cannot catch was not tested.

## Verdict

Off by default stays. The ADR's bar was a measured gain; 1 against 2 of 20 does not show one. What it shows is that the mechanism does what it says (it names a real leftover and the model fixes it) with no false firing on three other scenarios. A fall in failures would need many more runs
(about 10% to about 5% is a difference of one run in twenty), and a second note for a further location is the change `lo-6` argues for. Neither is done.

## Runs

| run | applied | turns | note | `check.sh` |
| --- | --- | --- | --- | --- |
| lobase-1 | 10 | 4 | - | OK |
| lobase-2 | 11 | 6 | - | OK |
| lobase-3 | 11 | 6 | - | OK |
| lobase-4 | 11 | 6 | - | OK |
| lobase-5 | 11 | 6 | - | OK |
| lobase-6 | 11 | 6 | - | OK |
| lobase-7 | 11 | 6 | - | OK |
| lobase-8 | 10 | 4 | - | OK |
| lobase-9 | 10 | 4 | - | OK |
| lobase-10 | 11 | 6 | - | OK |
| lobase-11 | 11 | 6 | - | OK |
| lobase-12 | 11 | 6 | - | OK |
| lobase-13 | 11 | 6 | - | OK |
| lobase-14 | 11 | 6 | - | OK |
| lobase-15 | 10 | 4 | - | FAIL |
| lobase-16 | 10 | 4 | - | FAIL |
| lobase-17 | 11 | 6 | - | OK |
| lobase-18 | 11 | 6 | - | OK |
| lobase-19 | 12 | 9 | - | OK |
| lobase-20 | 16 | 13 | - | OK |
| lo-1 | 11 | 7 | - | OK |
| lo-2 | 12 | 8 | - | OK |
| lo-3 | 12 | 9 | - | OK |
| lo-4 | 11 | 6 | - | OK |
| lo-5 | 10 | 6 | - | OK |
| lo-6 | 12 | 10 | `fetch_user` at accounts/greeting.py:6 | FAIL |
| lo-7 | 12 | 9 | - | OK |
| lo-8 | 11 | 6 | - | OK |
| lo-9 | 10 | 4 | - | OK |
| lo-10 | 11 | 6 | - | OK |
| lo-11 | 11 | 7 | - | OK |
| lo-12 | 12 | 9 | - | OK |
| lo-13 | 11 | 6 | - | OK |
| lo-14 | 11 | 6 | - | OK |
| lo-15 | 11 | 6 | `fetch_user` at tests/test_accounts.py:15 | OK |
| lo-16 | 12 | 9 | - | OK |
| lo-17 | 12 | 10 | `fetch_user` at tests/test_accounts.py:15 | OK |
| lo-18 | 11 | 6 | `fetch_user` at tests/test_accounts.py:15 | OK |
| lo-19 | 11 | 6 | - | OK |
| lo-20 | 13 | 11 | `fetch_user` at accounts/greeting.py:6 | OK |

False-positive runs, `--leftover-note` on:

| run | applied | turns | note | `check.sh` |
| --- | --- | --- | --- | --- |
| deepseek-s1 | 1 | 5 | - | OK |
| deepseek-s2 | 3 | 13 | - | OK |
| deepseek-s4 | 2 | 4 | - | OK |
| qwen-s1 | 1 | 8 | - | OK |
| qwen-s2 | 5 | 9 | - | OK |
| qwen-s4 | 1 | 13 | - | OK |

## Not checked

One model and scenario for the effect, one run per cell for the false-positive check, no driver, no rubric. Cost of the 46 runs: about $0.2. A second note per name, a lower threshold, and a default-on arm were not tried.
