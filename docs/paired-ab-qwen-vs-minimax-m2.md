# The first real paired A/B: qwen/qwen3-coder-next vs minimax/minimax-m2

- **Observed:** 2026-08-19, live OpenRouter traffic against the frozen 10-task corpus
- **Card:** KAN-943 (the run), KAN-944 (this writeup)
- **Rule it satisfies:** [ADR-0005](adr/0005-benchmark-and-ab-methodology.md)'s paired
  McNemar method, exercised for the first time against real results rather than known
  contingency tables; [ADR-0007 decision 6](adr/0007-model-selection-and-harness-config-shape.md)'s
  hash, which is what makes the two runs below comparable at all

Every prior number in this repo about the McNemar scorer (`internal/bench/mcnemar.go`,
`pair_test.go`) was computed against synthetic tables built to exercise its edge cases.
KAN-800 (CLAUDE.md, "Both halves of §Demo landed") was the first real corpus run, but it was
one arm with nothing to pair it against. This is the first time two real arms have actually
been run and scored against each other — the point KAN-942 (a second, argued harness
configuration) and KAN-943 (the plumbing to persist and compare two `RunResult`s) both exist
to reach.

**Read the number below for what it is.** SLICE-1's own risk 8 says this corpus is 10
in-house tasks, frozen and versioned so an experiment series stays comparable across changes
— not a stand-in for a published benchmark, and not evidence about either model's coding
ability outside this repository's own harness. A result here is a fact about kopicode's
current harness against these two arms on these ten tasks, and nothing wider.

## The arms

| | Model | Harness config | Provider pin |
|---|---|---|---|
| A | `qwen/qwen3-coder-next` | `default` (hash `5d7c99ef4bd3…`) | `parasail/bf16`, no fallbacks |
| B | `minimax/minimax-m2` | `minimax-m2-v1` (hash `11b7fe204d49…`) | `minimax/fp8`, no fallbacks |

The two harness configs differ in exactly one field (KAN-942): B's `Sampling.SeedPolicy` is
`SeedUnseeded` rather than `SeedFixed`, because minimax's pinned endpoint does not support
`seed` at all (`docs/provider-pin.md` §minimax/minimax-m2) — sending one would assert a
control the endpoint cannot exercise. Nothing else about the two arms' harness — system
prompt, tool set, parse routes, turn/token budgets, verification policy — differs. Both pins
are argued in `docs/provider-pin.md`; neither allows a fallback provider.

Raw results: [`bench-results/2026-08-19-qwen-vs-minimax-m2/qwen-qwen3-coder-next.json`](bench-results/2026-08-19-qwen-vs-minimax-m2/qwen-qwen3-coder-next.json)
and [`.../minimax-minimax-m2.json`](bench-results/2026-08-19-qwen-vs-minimax-m2/minimax-minimax-m2.json)
— exactly what `kopibench run --save` wrote, produced by `kopibench compare` per KAN-943.

## The result

```
paired comparison: qwen/qwen3-coder-next/default vs minimax/minimax-m2/minimax-m2-v1

  A   result    10/10 passed, 0 errored, 915204 tokens
  B   result    10/10 passed, 0 errored, 729452 tokens

  contingency table (rows: A, columns: B)
                 B pass   B fail
    A pass           10        0
    A fail            0        0

  discordant pairs   0 (of 10 task(s))
  method             none
  p-value            1.0000
  direction          no difference

  verdict: the arms agreed on every task; there is no evidence of a difference.
```

Both arms solved every task in the corpus. Zero discordant pairs means McNemar has nothing
to test — the ten tasks the corpus currently contains do not discriminate between these two
arms, at least not on pass/fail. This is itself a finding: the corpus needs harder or more
varied tasks before a pass-rate comparison between two capable models says anything, which is
exactly the kind of gap a real run finds and a synthetic contingency table cannot.

## The one number the pass-rate comparison doesn't capture

Every task both arms solved is also in every task's own report, and it shows a real,
consistent difference McNemar isn't built to score: minimax-m2 used noticeably fewer tokens
for the same result on most tasks.

| Task | A (qwen) tokens | B (minimax) tokens | B vs A |
|---|---|---|---|
| go-unicode-reverse | 170,677 | 29,037 | −83% |
| go-json-response-shape | 42,350 | 19,757 | −53% |
| go-error-wrapping | 94,610 | 83,370 | −12% |
| go-interval-merge | 30,913 | 92,874 | +200% |
| go-lru-cache | 27,065 | 58,825 | +117% |
| go-wordwrap-width | 88,788 | 50,582 | −43% |
| py-csv-blank-rows | 65,243 | 48,734 | −25% |
| go-flag-equals-form | 140,703 | 69,875 | −50% |
| go-config-retries | 114,774 | 124,224 | +8% |
| py-calc-modulo | 140,081 | 152,174 | +9% |
| **Total** | **915,204** | **729,452** | **−20%** |

Reading this per task rather than by the total matters: `go-interval-merge` and
`go-lru-cache` show minimax-m2 costing *more*, not less, so "minimax is cheaper" is not
uniformly true — the aggregate −20% is real but hides real per-task variance in both
directions. `go-unicode-reverse` is the extreme case on either side: qwen ran to the 20-turn
cap (`stop: max_turns`) while still passing, whereas minimax-m2 finished in 7 turns — the
same task the KAN-800 account in CLAUDE.md flagged as previously flaky for qwen (it failed on
`max_turns` in that earlier single-arm run) passed for both arms here, but qwen's path to
passing was visibly less efficient on this specific task.

**Cost**, computed from each run's actual prompt/completion token split against the pinned
endpoint's documented per-million price (`docs/provider-pin.md`; OpenRouter does not report
a cost figure back through this project's current plumbing, so this is arithmetic over real
usage, not a billed number):

- A (qwen, parasail/bf16 at $0.12/M prompt, $0.80/M completion): 903,833 prompt +
  11,371 completion → **≈$0.118**
- B (minimax, minimax/fp8 at $0.255/M prompt, $1.02/M completion): 715,185 prompt +
  14,267 completion → **≈$0.197**

So the run that used fewer total tokens (B) cost *more* in dollars, because its pinned
endpoint's per-token price is roughly double qwen's. Token count and dollar cost point in
opposite directions here — a reason to report both rather than either alone.

## What this does and does not show

- It shows the harness's forced-verification, hash-anchored-edit, parse-and-repair loop
  producing a clean 10/10 on two structurally different models with zero harness-classified
  failures on either arm — the same honest bar KAN-800 held qwen to alone, now held to a
  second arm.
- It does **not** show that minimax-m2 is "as good as" qwen in general, or that either model
  is unconditionally cheaper — both claims need harder tasks and more of them before this
  corpus can support them, and the per-task cost table above already shows the direction can
  flip.
- It is the proof that `internal/bench`'s paired McNemar path — `run --save`, `compare`, the
  contingency table, the significance test — works end to end on real data, which is what
  every future A/B in this project now gets to build on rather than re-derive.
