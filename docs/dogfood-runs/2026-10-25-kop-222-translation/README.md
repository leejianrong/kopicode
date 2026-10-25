# KOP-222: translating an anchor this reply's own edit made stale, measured on qwen3-coder-next s3 (2026-10-25)

The measurement [ADR-0038](../../adr/0038-in-reply-anchor-translation.md) asked for, run on the build in PR #268 against `origin/main` before it (e1e2150).

## Method

s3 (`rename-across-files`) on `qwen/qwen3-coder-next`, provider pinned as the journals record (`parasail/bf16`, temperature 0), a fresh scratch repo per run from `docs/agent-driver/scenarios/setup.sh`.
Two binaries built and stamped the same day, **base** v0.5.0-35-ge1e2150 and **new** v0.5.0-36-g389728b, run alternately, six each. Unlike the earlier KOP-222 notes this used
`kopicode run --print` directly, with no Claude Code driver, so these runs are not pooled with those; the two arms are compared with each other only. `check.sh` is the scenario's tree check.
Not a driver run: the task text went in unchanged and nothing else touched the tree.

## Result

| arm | runs | `anchor_drift` per run | `check.sh` OK | applied edits | turns |
| --- | --- | --- | --- | --- | --- |
| base | 6 | 0, 3, 3, 3, 3, 3 | 5 of 6 | 1, 11, 11, 10, 11, 11 | 5, 6, 6, 6, 7, 6 |
| new | 6 (+1 trial, not counted) | **0, 0, 0, 0, 0, 0** | 6 of 6 | 10, 11, 12, 11, 11, 10 | 4, 6, 8, 6, 6, 4 |

- **Rejections.** Base had three in five runs, at the same three edits as in the earlier notes. New had none in six. Each new run translated 4 or 6 anchors (16 translated edits in all, each with `translated_anchors` on `EditApplied`).
- **No wrong-line landing.** For every translated edit (16 in 6 runs: 3, 3, 2, 3, 2, 3) the line the diff removed was compared with the text the model's own `read_file` showed under the anchor it sent; all matched. The five other new runs' trees pass `check.sh`.
- **Turns** are not higher: new 4 to 8 against base 5 to 7. Token totals were not summed (the ProviderResponse usage field was not read); the total `cost_usd` in all 13 journals is about $0.05.
- **The one base failure and the trial run** (`base-1`, `trial-new-0`) are the same early stop in both arms: one `edit_file` on README.md, verification passes, "I've successfully renamed ..." with `fetch_user` in four files, `check.sh` FAIL. It is the model's habit, not the rule (it applied nothing to translate), and it matches the early stops in the [regions note](../2026-10-24-kop-222-regions/README.md), which blamed that arm's prompt. It happened on `main` here, so that attribution was wrong or incomplete. The trial run was made while checking the script and is kept for that reason, and is not in the table.

## What this does not show

Six runs a side, one model, one scenario, one provider, one day. Not run: deepseek s1 to s5 (ADR-0038 asked for one rerun to show no change where there was no drift), the Claude Code and Codex drivers, and any scenario with duplicated lines. Whether
the zero rejections hold with a differently phrased task is unknown. The ambiguous-original hazard ADR-0038 records as not closed was not exercised.
