# KOP-226: ten qwen s3 runs on main, for the leftover-name rate (2026-10-25)

Ten runs of `qwen/qwen3-coder-next` on s3 with the build of PR #270 (`run --print`, a fresh scratch repo per run). Together with the thirteen runs in
[`2026-10-25-kop-222-translation`](../2026-10-25-kop-222-translation/README.md) they are the 23 counted in [ADR-0039](../../adr/0039-leftover-name-note.md).

| run | applied | rejected | turns | verifications | `check.sh` |
| --- | --- | --- | --- | --- | --- |
| main-1 | 11 | 0 | 6 | 2 | OK |
| main-2 | 12 | 0 | 8 | 3 | OK |
| main-3 | 12 | 0 | 9 | 4 | OK |
| main-4 | 11 | 0 | 7 | 3 | OK |
| main-5 | 11 | 0 | 8 | 3 | **FAIL**: `# fetch_user is the only lookup we have.` left in `accounts/greeting.py` |
| main-6 | 9 | 0 | 14 | 9 | OK |
| main-7 | 10 | 0 | 5 | 1 | **FAIL**: leftover in `tests/test_accounts.py` |
| main-8 | 11 | 0 | 8 | 3 | OK |
| main-9 | 10 | 0 | 4 | 1 | OK |
| main-10 | 12 | 0 | 9 | 4 | OK |

Two of ten leave the old name and say the rename is done; none stopped after a single edit (the early stop seen in `base-1` and `trial-new-0`). Across all 23 s3 runs of the day: 4 with a leftover.
Zero `anchor_drift` in all ten (ADR-0038 is in this build). One model, one scenario, one day; cost about $0.03.
