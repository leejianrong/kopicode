# The harness-axis A/B, repeated: pooling three runs for significance

- **Observed:** 2026-08-26, live OpenRouter traffic against the hardened 13-task
  corpus, same arms as [`docs/paired-ab-qwen-default-vs-naive-v1.md`](paired-ab-qwen-default-vs-naive-v1.md)
  (KAN-1014's original run, referred to below as "run 1")
- **Card:** KAN-1018, epic 128
- **Rule it satisfies:** the "next steps" that original writeup left open — "repeat or
  extend this run before treating the one discordant pair as more than a first data
  point" — and [ADR-0005](adr/0005-benchmark-and-ab-methodology.md) decision 4's
  sequential-early-stopping principle, applied as a pre-registered plan since no
  automated stopping mechanism exists in this codebase (see below).

## What this is, and what it explicitly is not

This is **three runs, pooled, still not significant** — not five runs, and not a
concluded experiment. The plan agreed before running anything (recorded on KAN-1018)
was to repeat the identical `default`-vs-`naive-v1` pair up to four more times,
checking pooled significance after each one, stopping either the moment pooled
p < 0.05 or after five total runs. Three runs in, the pooled p-value had moved from
1.0 toward significance but not reached it, and the decision — made explicitly, not
silently — was to stop spending here rather than run the remaining two repeats.
ADR-0005 decision 8 requires reporting what was dropped: **two of the five planned
runs were not executed**, and this result is read against that, not against a
completed n=5 sequence.

## The mechanism: no automated stopping tool exists

`internal/bench/runner.go`'s own doc comment ties "ADR-0005 §4's sequential early
stopping" to preserving a fixed task order within one run — there is no code
anywhere that computes a running p-value mid-run and halts dispatch automatically.
"Repeating" here means whole 13-task paired runs (`kopibench run -save`, both arms,
identical to KAN-1014's own invocation), scored and pooled by hand between runs.

**`internal/bench.Compare` cannot pool repeat runs directly**, and this is correct
scoping rather than a gap: `pair.go`'s `index()` refuses a duplicate task id within
one `Arm` (`ErrDuplicateTask`), so feeding two repeat-runs' 13 outcomes each into one
`Arm` (26 entries, ids repeating) fails outright by design. Pooling instead uses
`bench.McNemar(Table)` directly — already exported, already tested — on the four
cell counts summed across runs' own `kopibench compare` output, rather than
reimplementing the statistic or adding new scoring tooling for a single card. A
`kopibench compare -pool` subcommand is worth building only if this turns out to be
a recurring need across future cards, not built here.

## The three runs

All three: `qwen/qwen3-coder-next`, `default` (hash `e6d896ac8f3f`) vs `naive-v1`
(hash `0cc1776a5c4a`), `parasail/bf16` pin, hardened corpus v2.0.0
(`sha256:6f8010f0c0605145af975a0860f9946ee917bbcab231f991cfb0d7977203553a`).

| Run | Build | default | naive-v1 | Table (BothPassed/AOnly/BOnly/BothFailed) | Discordant | p (this run alone) |
|---|---|---|---|---|---|---|
| 1 (KAN-1014) | `71d739e` | 13/13 | 12/13 | 12/1/0/0 | 1 | 1.0000 |
| 2 (repeat 1) | `ce94190` | 12/13 | 12/13 | 11/1/1/0 | 2 | 1.0000 |
| 3 (repeat 2) | `ce94190` | 12/13 | 11/13 | 11/1/0/1 | 1 | 1.0000 |

Runs 2 and 3 ran under a later commit than run 1 (KAN-1013/KAN-1019/KAN-1020's PRs
landed in between) — per [ADR-0007](adr/0007-model-selection-and-harness-config-shape.md)
decision 7, pooling requires matching *hashes*, not matching commits, and both arms'
hashes are unchanged across all three runs (`defaultHarnessConfig` and
`naiveV1Config` were not touched by the intervening PRs, which only added two new,
separately-named configurations). Verified by the hashes in the table above being
identical across all three runs' saved results, not assumed.

Raw results: [`bench-results/2026-08-25-qwen-default-vs-naive-v1/`](../bench-results/2026-08-25-qwen-default-vs-naive-v1/)
(run 1), [`bench-results/2026-08-26-qwen-default-vs-naive-v1-repeat1/`](../bench-results/2026-08-26-qwen-default-vs-naive-v1-repeat1/),
[`bench-results/2026-08-26-qwen-default-vs-naive-v1-repeat2/`](../bench-results/2026-08-26-qwen-default-vs-naive-v1-repeat2/).

## What actually flipped, run by run

The discordant task was **not the same task twice in a row**, which matters more
than the aggregate p-value for reading this honestly:

- **`go-multi-file-rename-contract`** — the task KAN-999's writeup already
  identified as structurally hard for every arm it has ever tested — hit `max_turns`
  in all three runs, but only *failed* outright in runs 2 and 3; it passed run 1
  despite hitting the same cap. `default` failed it in both repeats; `naive-v1`
  failed it once (run 2) and passed it once (run 3, where it was `default` that
  failed instead — the one `BOnly` cell in run 2's table). This is the clearest
  single illustration of the pinned endpoint's acknowledged non-determinism
  ([`docs/provider-pin.md`](provider-pin.md): "not a guarantee of determinism — no
  hosted endpoint offers one") landing on the exact task this project's corpus
  hardening already flagged as its hardest.
- **`py-calc-modulo`** — `naive-v1`'s one discordant failure in run 1, traced in
  that writeup to the harness giving the model no checkpoint to re-verify a
  multi-file fix before running out of turns — failed again under `naive-v1` in run
  2 (`AOnly` there), then **passed** under `naive-v1` in run 3. Two failures out of
  three is suggestive but is exactly the kind of single-task pattern a pooled
  aggregate can't distinguish from noise at this sample size, and the third run's
  pass means it is not a consistent failure either.
- **`go-config-retries`** — flipped for the first time in run 3 (`default` passed,
  `naive-v1` hit `max_turns`), a task neither prior run had ever seen disagree.

No task has failed under `naive-v1` in every run it appeared in discordant, and no
task has been the sole driver of the pooled count — the three discordant pairs
favouring `default` come from three different tasks, not one repeatedly.

## Pooled result

```
pooled table (summed across runs 1-3, via bench.McNemar directly — see above)

  BothPassed  34
  AOnly        3   (default passed, naive-v1 failed)
  BOnly        1   (naive-v1 passed, default failed)
  BothFailed   1

  discordant pairs   4
  method             exact-binomial
  p-value            0.625
  direction          favours A (default)

  verdict: not significant at alpha=0.05
```

p moved from 1.0 (run 1 alone) to 0.625 (pooled across three runs) — real movement
toward significance as evidence accumulated, and the 3-to-1 split still favours
`default` rather than converging toward even — but 0.625 is nowhere near the 0.05
threshold, and stopping at n=3 rather than the planned n=5 means this movement is
read as a trend worth continuing to watch, not a result.

## Cost

| Run | default | naive-v1 | Combined |
|---|---|---|---|
| 1 (KAN-1014) | $0.1733 | $0.1748 | $0.3481 |
| 2 (repeat 1) | $0.1570 | $0.1718 | $0.3288 |
| 3 (repeat 2) | $0.1690 | $0.1661 | $0.3351 |
| **Total** | | | **≈$1.0120** |

## What this does and does not show

- **It does not reach significance**, and stopping at n=3 means it does not rule
  significance out either — the pre-registered plan had two more repeats budgeted
  specifically because n=3 was expected to be an ambiguous stopping point either
  way.
- **The direction is consistent across all three runs**: `default` has never lost
  the pooled comparison, only tied it (run 1) or narrowly outscored `naive-v1` on
  discordant pairs (runs 2 and 3 combined). Three different tasks drove the three
  `default`-favouring flips, which is weak evidence against any single task being
  an outlier that inflates the aggregate.
- **`go-multi-file-rename-contract`'s instability across all three runs** is a
  separate, real finding from the harness-axis question this card set out to
  answer: it is evidence about *this task's* difficulty and the pinned endpoint's
  non-determinism, not about `default` vs `naive-v1`. Worth naming so a future
  reader does not read three runs' worth of one task's flakiness as a harness-axis
  signal.
- **The pre-registered plan is not complete.** Two more repeats (runs 4 and 5) were
  budgeted and not run — a deliberate stop on cost grounds, made explicitly rather
  than silently substituted for "done." Per ADR-0005 decision 8, this is stated
  plainly: this result answers "what does n=3 show," not "what does the full
  pre-registered n=5 plan show."

## Next steps, not started here

- **Resume the plan from n=3**, not restart it — repeats 4 and 5, same arms, same
  pooling method, whenever the cost is worth spending against this question again.
- **`go-multi-file-rename-contract`'s cross-run instability** may be worth its own
  look independent of this card — three runs, three different outcomes, on a task
  KAN-999 already flagged as structurally hard. Whether that is corpus-authoring
  work (a task with less run-to-run variance) or a genuine finding about the
  pinned endpoint is not decided here.
- **`naive-v2` (`naive-verify-only`) and `naive-toolset` (`naive-fuzzy-edit-only`)**,
  registered since run 1 (KAN-1019/1022, KAN-1020/1023), have not been run against
  `default` at all yet — each is its own, separately-budgeted paired A/B, not a
  continuation of this pooled result.
