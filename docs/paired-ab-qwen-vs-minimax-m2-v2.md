# The hardened corpus, re-run: qwen/qwen3-coder-next vs minimax/minimax-m2

- **Observed:** 2026-08-25, live OpenRouter traffic against the hardened 13-task corpus
- **Card:** KAN-999 (this run and writeup), closing the loop opened by epic 125
  (KAN-992..998, "Corpus hardening")
- **Rule it satisfies:** the same two rules
  [`docs/paired-ab-qwen-vs-minimax-m2.md`](paired-ab-qwen-vs-minimax-m2.md) satisfied —
  [ADR-0005](adr/0005-benchmark-and-ab-methodology.md)'s paired McNemar method and
  [ADR-0007 decision 6](adr/0007-model-selection-and-harness-config-shape.md)'s
  harness-config hash — plus ADR-0005 §2's requirement that a result record the
  corpus's own version and digest, since this run is only comparable to the last one
  if the corpus underneath it is a fact of record too.

## Why this run exists

The prior paired A/B (`docs/paired-ab-qwen-vs-minimax-m2.md`, KAN-943/944,
2026-08-19) ran these same two arms against the 10-task corpus and got **10/10 on
both arms, zero discordant pairs** — McNemar had nothing to test. Epic 125
(KAN-992..998) hardened the corpus in response: two saturated single-file,
no-distinguishing-trait tasks were retired (`go-json-response-shape`,
`py-csv-blank-rows`), and five new multi-file / exploration / shallow-fix-trap tasks
were added (`go-interface-implementation-gap`, `py-multi-module-exploration`,
`go-multi-file-rename-contract`, `go-event-dispatch-ordering`,
`go-partial-fix-trap-validation`), landing at `corpus_version` **2.0.0**, 13 tasks.
This card is the validation step: does the harder corpus actually discriminate
between these two arms now, or is it still saturated?

## Corpus under test

```
corpus_version   2.0.0
digest           sha256:6f8010f0c0605145af975a0860f9946ee917bbcab231f991cfb0d7977203553a
task count       13
```

Confirmed against `main` at commit `cf920ec3107c8ac9482983b612b0a7cafe05b40b` —
`go test -race -count=1 -run TestLoadRealCorpusWalksEveryTaskDirectory
./internal/corpus/` passes, which means `corpus.Load` recomputed this digest from
the tree on disk and it matched the value recorded in `corpus.json` (`internal/corpus/corpus.go`
refuses to load otherwise).

## The arms

Unchanged from the prior run — same pins, same harness configs, argued in
`docs/provider-pin.md`:

| | Model | Harness config | Provider pin |
|---|---|---|---|
| A | `qwen/qwen3-coder-next` | `default` (hash `e6d896ac8f3f…`) | `parasail/bf16`, no fallbacks |
| B | `minimax/minimax-m2` | `minimax-m2-v1` (hash `8161544674d4…`) | `minimax/fp8`, no fallbacks |

Full hashes: A `e6d896ac8f3f82f87f4c29e357422ed0c02027d219a0a41d432fc8617376790f`, B
`8161544674d41757915fdde138fe0432960ce7b49f2f590d1e870456049cc972`. Both were run at
`-jobs 4` (the default) with no retries or rate-limit trouble on either arm — the
`z-ai/glm-5.2` 10rpm-new-account issue documented in `docs/provider-pin.md` did not
recur here.

Raw results: [`bench-results/2026-08-25-qwen-vs-minimax-m2-v2/qwen-qwen3-coder-next.json`](../bench-results/2026-08-25-qwen-vs-minimax-m2-v2/qwen-qwen3-coder-next.json)
and [`.../minimax-minimax-m2.json`](../bench-results/2026-08-25-qwen-vs-minimax-m2-v2/minimax-minimax-m2.json)
— `kopibench run --save` output, `run-20260824t170505z` (A) and `run-20260824t170818z`
(B), paired with `kopibench compare`. A's run also serves as this card's solo-arm
result (step 3 of KAN-999) — there was no reason to spend a second run on the same
arm.

## The result

```
paired comparison: qwen/qwen3-coder-next/default vs minimax/minimax-m2/minimax-m2-v1

  A   result    12/13 passed, 0 errored, 1338514 tokens
  B   result    12/13 passed, 0 errored, 1079841 tokens

  contingency table (rows: A, columns: B)
                 B pass   B fail
    A pass           12        0
    A fail            0        1

  discordant pairs   0 (of 13 task(s))
  method             none
  p-value            1.0000
  direction          no difference

  verdict: the arms agreed on every task; there is no evidence of a difference.
```

**Both arms passed 12/13, and both failed the *same* task**:
`go-multi-file-rename-contract`, both `stop: max_turns` (harness-bucketed under
KAN-797's mechanical rule). Zero discordant pairs — identical to the 10-task result,
just one notch down from a perfect score on both sides instead of two.

## Per-task, so "12/13 twice" doesn't hide anything

| Task | A (qwen) | B (minimax) | Traits |
|---|---|---|---|
| go-unicode-reverse | pass | pass | (original) |
| go-interface-implementation-gap | pass | pass | new |
| go-error-wrapping | pass | pass | (original) |
| go-interval-merge | pass | pass | (original) |
| go-lru-cache | pass | pass | (original) |
| go-wordwrap-width | pass | pass | (original) |
| py-multi-module-exploration | pass | pass | new |
| go-flag-equals-form | pass | pass | (original) |
| go-config-retries | pass | pass | (original) |
| py-calc-modulo | pass | pass | (original) |
| go-multi-file-rename-contract | **fail** (max_turns) | **fail** (max_turns) | new |
| go-event-dispatch-ordering | pass | pass | new |
| go-partial-fix-trap-validation | pass | pass | new |

All five new tasks landed a pass on at least one arm; four of the five passed on
both. The one that didn't — `go-multi-file-rename-contract` — is exactly the kind of
task the hardening was meant to add (`traits: [multi_file, requires_read]`, a
tier-threading fix that has to reach three independent call sites, not one), and it
is genuinely hard rather than ambiguous: reading both journals
(`.kopicode/bench/runs/run-20260824t170505z/.../events.jsonl`,
`run-20260824t170818z/.../events.jsonl`), neither model finished. qwen's turn-20
`go test ./...` still failed to *build* — `computeLine` and `d.Apply` still missing
the widened `Tier` parameter the task's own notes call "the harder half." minimax's
turn-20 build failed on a plain syntax error in `invoice.go`. Both spent most of
their 20 turns re-running verification against a codebase that wouldn't compile
(9 `VerificationRun` events for A, all `exit_code: 1`), not iterating toward a fix
that was almost there.

## Cost, measured

| | Prompt | Completion | Total tokens | Cost |
|---|---|---|---|---|
| A (qwen, $0.12/M prompt, $0.80/M completion) | 1,322,633 | 15,881 | 1,338,514 | **≈$0.1714** |
| B (minimax, $0.255/M prompt, $1.02/M completion) | 1,058,078 | 21,763 | 1,079,841 | **≈$0.2920** |
| **Combined** | | | 2,418,355 | **≈$0.4634** |

Both runs together cost less than half of one dollar, well inside the ~$2
per-corpus-run ceiling README.md carries, and roughly in line with the 10-task
run's $0.0846 + $0.197 scaled up by 13 tasks and the harder new tasks (the failed
task alone burned 197,443 + 171,304 ≈ 369K tokens across both arms running to the
turn cap — the single most expensive task for both models, unsurprising for the one
neither finished).

## Does the hardening work?

**Not yet, on pass/fail — the corpus still returns zero discordant pairs between
these two specific arms.** The headline number is unchanged from the 10-task run in
the one respect that matters for McNemar: A and B still agree on every task's
outcome. What changed is that the ceiling moved down from 10/10-and-10/10 to
12/13-and-12/13 — the corpus is measurably harder than it was, but "harder for both"
and "discriminating between them" are different properties, and this run only
confirms the first.

That is not a wasted result. The corpus needed harder tasks regardless of whether
this exact pair discriminated on them — a corpus where every task is trivial for
both current arms would understate future arms' failure rates too. And the
shared failure is informative on its own: `go-multi-file-rename-contract` is hard
in a specific, designed way (thread a parameter through three call sites, only one
of which already has it in scope) and both models hit the same wall in the same
number of turns, which is a real signal about a shared limitation in
multi-file refactor tasks under this harness's turn budget — not evidence the
corpus is ambiguous or the harness is misbehaving. Neither model's session shows a
harness defect: syntax gates ran, edits applied cleanly, verification ran
correctly and reported real compile errors every time. The failure is exactly what
KAN-797's classifier calls it, `harness` (hit the mechanical `max_turns` bound), while
the actual root cause traced through the journal is a model capability limit on this
specific multi-file threading task — the same gap between the classifier's
deliberately conservative bucket and the journal's actual story that KAN-800 first
surfaced.

**What this implies for further hardening**, if the goal is specifically to produce
discordant pairs between *this* pair of models: the five new tasks landed, on
balance, too easy to discriminate (4 of 5 passed on both) and one landed hard enough
that both failed identically. A task that discriminates needs to sit in the band
where one model's specific weaknesses show and the other's don't — which requires
either a wider spread of task difficulty within the "new task" bucket, or tasks
targeted at a known asymmetry between these two specific models (e.g., minimax's
consistently lower average token usage per task in both runs suggests it may reach
conclusions faster but possibly with less exploration on tasks that reward
thoroughness — untested here, since this corpus doesn't yet have such a task).
That is a hypothesis for a future corpus-hardening pass, not a claim this run
proves.

## What this does and does not show

- It confirms the corpus-hardening epic's mechanical acceptance criteria: 13 tasks,
  `corpus_version` 2.0.0, digest verified, both directions of the new tasks' oracles
  presumably checked at merge time (epic 125's own PRs, not re-verified here).
- It shows the harness's forced-verification, hash-anchored-edit, parse-and-repair
  loop held up cleanly on both arms against five genuinely new task shapes — no
  syntax-gate failures, no fuzzy-edit fallbacks (`unattributed = 0` on both runs), no
  provider retries beyond the two `ProviderRetried` events already inside qwen's
  normal budget.
- It does **not** show that the hardened corpus discriminates between
  `qwen/qwen3-coder-next` and `minimax/minimax-m2` specifically. It may well
  discriminate other arms — a weaker or more idiosyncratic model is a much more
  plausible source of a real discordant pair than sharpening the gap between two
  already-capable ones further.
- It does not show anything about either model's general coding ability outside
  this repository's own harness and this specific 13-task corpus, per the same
  scope note the prior writeup carried.

## Next step, flagged and not started here

Per KAN-999's own scope: the result this project actually wants next is a
**tuned-vs-deliberately-naive `harness.Config` A/B on one model** — holding the model
and provider pin fixed and varying only the harness configuration, to isolate what
the harness itself is worth rather than what one model is worth relative to another.
That needs its own new `harness.Config` (a naive baseline: no forced verification, no
hash-anchored edits or a looser edit tool, a thinner system prompt — the specific
shape is a design question, not a foregone one) and its own `/pandan-pm` scoping pass
as a new epic. This card does not build it — it only clears the way by confirming
the current corpus's behavior against the current model pair is understood before a
harness-delta experiment reuses the same corpus.
