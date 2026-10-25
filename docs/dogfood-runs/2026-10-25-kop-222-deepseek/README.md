# KOP-222: deepseek s1 to s5 with and without in-reply anchor translation (2026-10-25)

The regression rerun [ADR-0038](../../adr/0038-in-reply-anchor-translation.md) asked for: one run per scenario on each build, to show no change where there was no drift. Same method as the
[qwen note](../2026-10-25-kop-222-translation/README.md): `kopicode run --print`, no driver, a fresh scratch repo per run, **base** v0.5.0-35-ge1e2150 (before) and **main** v0.5.0-36-g93c7f8a (after), model `deepseek/deepseek-v3.2`.

| scenario | build | applied | rejected (all `anchor_drift`) | translated | turns | `check.sh` |
| --- | --- | --- | --- | --- | --- | --- |
| s1 | base | 1 | 3 | 0 | 3 | OK |
| s1 | main | 0 | 1 | 0 | 4 | OK |
| s2 | base | 3 | 2 | 0 | 4 | OK |
| s2 | main | 3 | 2 | 0 | 3 | OK |
| s3 | base | 10 | 3 | 0 | 6 | OK |
| s3 | main | 10 | 0 | 6 | 5 | OK |
| s4 | base | 1 | 1 | 0 | 5 | OK |
| s4 | main | 2 | 0 | 0 | 10 | OK |
| s5 | base | 1 | 0 | 0 | 6 | FAIL |
| s5 | main | 1 | 1 | 0 | 4 | FAIL |

- **Tree outcomes are the same on every scenario.** s1 to s4 pass `check.sh` on both builds; s5 fails on both (its tests contradict, so `check.sh` fails unless the model stops honestly, and neither did; not scored against the rubric here).
- **Translation fired only on s3** (6 anchors), where it removed the three rejections. That is a correction to the earlier notes: deepseek is not at zero rejections on s3 in this setup (base had 3), so the rule helps both models there.
- **Rejections remain on s1, s2, s4 and s5 on main** (1, 2, 0, 1). None was translated, so they were not this rule's case; they were not examined. One run each, so the counts differ between builds by sampling as much as by the change (s1 3 to 1, s4 1 to 0, s5 0 to 1).
- s4 took 10 turns on main against 5; not examined, one run each.

## Not checked

One run per cell, one model, `run --print` only (no driver, no `ask` answers). The rubric was not applied. Why s1, s2, s4 and s5 still have `anchor_drift` was not looked into.
