# The harness-axis A/B: qwen/qwen3-coder-next, tuned vs a naive baseline

- **Observed:** 2026-08-25, live OpenRouter traffic against the hardened 13-task corpus
- **Card:** KAN-1012 (design), KAN-1013 (registration), KAN-1014 (this run),
  KAN-1015 (this writeup), epic 128
- **Rule it satisfies:** the same [ADR-0005](adr/0005-benchmark-and-ab-methodology.md)
  paired McNemar method and [ADR-0007 decision 6](adr/0007-model-selection-and-harness-config-shape.md)
  hash discipline every prior paired A/B in this repo has used — but this is the
  first one where **model and provider pin are held fixed and harness config is
  the only independent variable**, which is the comparison ADR-0007's own
  "Consequences" section anticipated the registry existing for
  ("it is what makes slice 2's first paired A/B two invocations instead of two
  builds") and which no result in this project has actually run until now.

## Why this run exists

Every paired A/B so far — the original qwen-vs-minimax-m2 result
(`docs/paired-ab-qwen-vs-minimax-m2.md`) and its re-run against the hardened
corpus (`docs/paired-ab-qwen-vs-minimax-m2-v2.md`, KAN-999) — varied the
**model** while holding each model's own harness configuration fixed. Neither
ever tested kopicode's own central thesis: that harness quality (forced
verification, hash-anchored edits, parse-and-repair tolerance) is what moves a
model's measured coding ability, independent of which model sits behind it.
This run holds the model (`qwen/qwen3-coder-next`) and the provider pin
(`parasail/bf16`) fixed and varies harness config alone — a tuned `default` arm
against a deliberately naive baseline, `naive-v1`.

## The mechanism, settled during scoping (KAN-999's follow-on, epic 128)

No new ADR and no [ADR-0010](adr/0010-declarative-harness-configs-and-self-tuning.md)
declarative config were needed. `internal/harness/resolve.go`'s `Resolve()`
looks up `--harness`'s override via `ConfigByName(name)`, which is **not**
gated by the requested model's registry row — any registered `Config` is
selectable for any model. Registering a second built-in configuration and
selecting it with `--harness naive-v1` on a model that already defaults to
`default` is squarely inside [ADR-0007](adr/0007-model-selection-and-harness-config-shape.md)
decision 5's existing built-in registry, exactly as that ADR's own
"Consequences" section anticipated.

## What "naive" means here, and why (KAN-1012's design record)

`naive-v1` differs from `default` in exactly three independent policy fields,
plus a system prompt that is a required honest consequence of two of them —
**not** a fourth independent axis:

| Field | `default` | `naive-v1` | Why |
|---|---|---|---|
| `Verification.Forced` | `true` | **`false`** | The headline lever. `internal/engine/loop.go`'s `verify()` skips calling `verify.Run()` entirely when this is off — no forced check-and-correct loop at all, not merely "runs but doesn't block." |
| `RepairBudget` | `2` | **`0`** | No parse-and-repair round trips for a malformed tool call. `0` is the engine's documented legal floor (`internal/engine/engine.go` refuses only a negative value), not an edge case being abused. |
| `ParseRoutes` | `["native","fenced_json","xml_tag"]` | **`["native"]`** | Drops SLICE-1 §3's multi-route tolerance — a non-native-shaped call is no longer forgiven by falling back to fenced-JSON/XML extraction. `AdvertiseNativeTools` stays `true` on both arms so this doesn't make tool-calling non-functional, just less forgiving. |
| `SystemPrompt` | `prompt_default.md` | **`prompt_naive.md`** | A required consequence of the two rows above, not an independent choice: the default prompt's "## Verification" section describes forced-verification behaviour that would simply be false under `naive-v1`, and a prompt still claiming multi-route tolerance the harness no longer honours would be the same problem in the other direction. Same tool-section/argument/reject-reason/anchor-version contract, far less guidance prose. |

`ToolSet`, `ToolCatalogue`, `AdvertiseNativeTools`, `MaxTurns` (20),
`TokenBudget` (2,000,000) and every `Sampling` field (temperature 0, fixed
seed 1) are **identical** on both arms — held fixed deliberately, the same
discipline `MinimaxM2ConfigName`'s own doc comment used to justify changing
exactly one field and no more.

**Per [ADR-0007](adr/0007-model-selection-and-harness-config-shape.md) decision 6/7: this
result attributes to the whole three-field bundle above, never to any one
field alone.** Whatever the result shows below is evidence about "a harness
with no forced verification, no repair budget and no route tolerance" as a
package — not a controlled test of forced verification in isolation.

## Corpus and build under test

```
corpus_version   2.0.0
digest           sha256:6f8010f0c0605145af975a0860f9946ee917bbcab231f991cfb0d7977203553a
task count       13
build            71d739e1b641a76fe78cf51f8a82dc2054ec30eb (clean, ldflags)
```

Re-verified immediately before this run (`go test -race -count=1 -run
TestLoadRealCorpusWalksEveryTaskDirectory ./internal/corpus/`), unchanged
since KAN-999. **Both arms ran under the same build commit**, which matters
for [ADR-0007](adr/0007-model-selection-and-harness-config-shape.md) decision
7's pooling rule — KAN-999's earlier `default`-arm result could not be reused
here even though nothing about `defaultHarnessConfig` changed, because the
repository had moved (KAN-1013 merged) since that run and a differing build
commit is not poolable regardless of a matching hash.

## The arms

| | Model | Harness config | Harness hash | Provider pin |
|---|---|---|---|---|
| A | `qwen/qwen3-coder-next` | `default` | `e6d896ac8f3f…` | `parasail/bf16`, no fallbacks |
| B | `qwen/qwen3-coder-next` | `naive-v1` | `0cc1776a5c4a…` | `parasail/bf16`, no fallbacks (identical to A) |

Full hashes: A `e6d896ac8f3f82f87f4c29e357422ed0c02027d219a0a41d432fc8617376790f`,
B `0cc1776a5c4a2fa9ea9f07b1ad9857231453327cd3cd6749035c28cddc78dd29`. Both at
`-jobs 4` (the default), no retries or rate-limit trouble on either arm.

Raw results: [`bench-results/2026-08-25-qwen-default-vs-naive-v1/default.json`](../bench-results/2026-08-25-qwen-default-vs-naive-v1/default.json)
and [`.../naive-v1.json`](../bench-results/2026-08-25-qwen-default-vs-naive-v1/naive-v1.json)
— `run-20260825t131349z` (A) and `run-20260825t131603z` (B).

## The result

```
paired comparison: qwen/qwen3-coder-next/default vs qwen/qwen3-coder-next/naive-v1

  A   result    13/13 passed, 0 errored, 1355537 tokens
  B   result    12/13 passed, 0 errored, 1352553 tokens

  contingency table (rows: A, columns: B)
                 B pass   B fail
    A pass           12        1
    A fail            0        0

  discordant pairs   1 (of 13 task(s))
  method             exact-binomial
  p-value            1.0000
  direction          favours A

  verdict: not significant at alpha=0.05 (p=1.0000) — favours A, but the
  discordant pairs are too few or too close to call.
```

**This is the first discordant pair any paired A/B in this project has ever
produced.** Both prior model-axis comparisons (`docs/paired-ab-qwen-vs-minimax-m2.md`
at 10 tasks, `docs/paired-ab-qwen-vs-minimax-m2-v2.md` at 13) scored zero
discordant pairs. One discordant pair out of 13 is nowhere near statistically
decisive — the exact-binomial test correctly reports `p=1.0000` — but the
*direction* is exactly what the project's own thesis predicts: the tuned
configuration beat the naive one on the one task where they disagreed, never
the other way around.

One honest note on determinism: `default` scored 13/13 here, against 12/13 in
KAN-999's run three days earlier under the same configuration and a fixed
seed. `docs/provider-pin.md` already states plainly that a fixed seed is "not
a guarantee of determinism — no hosted endpoint offers one," and this run is
a second, direct illustration of that on the pinned endpoint. It does not
change this run's own paired result, since both arms here ran back-to-back
under the identical build and pin.

## Per task

| Task | A (default) turns/tokens | B (naive-v1) turns/tokens | Note |
|---|---|---|---|
| go-unicode-reverse | pass, 5t / 24,896 | pass, 11t / 57,670 | |
| go-interface-implementation-gap | pass, 17t / 125,294 | pass, 19t / 115,257 | |
| go-error-wrapping | pass, 9t / 58,530 | pass, 6t / 26,726 | |
| go-interval-merge | pass, 11t / 92,183 | pass, 8t / 42,376 | |
| go-lru-cache | pass, 6t / 35,299 | pass, 8t / 39,086 | |
| go-wordwrap-width | pass, 10t / 84,879 | pass, 11t / 79,028 | |
| py-multi-module-exploration | pass, 15t / 112,524 | pass, **20t (max_turns)** / 142,732 | |
| go-flag-equals-form | pass, 13t / 104,545 | pass, **20t (max_turns)** / 171,506 | |
| go-config-retries | pass, 15t / 111,881 | pass, 14t / 77,467 | |
| py-calc-modulo | pass, 20t (max_turns) / 181,525 | **fail, 20t (max_turns)** / 144,438 | the one discordant task |
| go-multi-file-rename-contract | pass, 20t (max_turns) / 203,117 | pass, **20t (max_turns)** / 208,588 | hard task, both hit the cap |
| go-event-dispatch-ordering | pass, 16t / 128,068 | pass, **20t (max_turns)** / 158,268 | |
| go-partial-fix-trap-validation | pass, 12t / 92,796 | pass, 14t / 89,411 | |

**The clearest pattern isn't the one discordant task — it's the turn-cap
column.** `naive-v1` hit `max_turns` on **5 of 13 tasks**; `default` hit it on
only 1 (the same structurally-hard `go-multi-file-rename-contract` KAN-999's
writeup already traced as genuinely difficult for both models it tested).
Four of `naive-v1`'s five turn-cap tasks still passed — the model got there
anyway — but it burned its entire 20-turn budget doing it, every time,
regardless of whether the fix was actually done well before turn 20.

## Why `py-calc-modulo` failed under `naive-v1` (the one discordant task)

Traced through the journal
(`.kopicode/bench/runs/run-20260825t131603z/py-calc-modulo/.../events.jsonl`):
the session shows **zero `VerificationRun` events**, confirming
`Verification.Forced: false` behaves exactly as designed — no forced check
ran at any point. The model's own tool-call sequence tells a specific story,
not a generic "ran out of turns" one:

- Turns 1–5: read every relevant file (`tokens.py`, `lexer.py`,
  `evaluator.py`, `test_calc.py`).
- Turns 6–7: ran `python3 -m unittest discover -q` **itself**, via
  `run_shell` — self-compensating for the missing forced check, unprompted.
- Turns 8–20: thirteen further tool calls, ten of them `edit_file`,
  implementing modulo support across all three source files in a coherent,
  correctly-ordered sequence (tokens → lexer → evaluator) — this was not a
  model flailing or looping.
- The session ends at turn 20 **mid-edit on `evaluator.py`**, having never
  run the test suite again after the single check at turns 6–7, before any of
  the actual fix existed.

Under `default`, forced verification runs after every edit that changes the
tree and would have caught exactly this: an incomplete `evaluator.py` change
partway through a multi-file fix, with nineteen more turns of budget left to
react to it. `naive-v1`'s failure here was not the model reasoning worse — the
edit sequence itself looks sound — it was the harness giving the model no
natural checkpoint for *when* to re-check its own work, so it spent its whole
turn budget editing without ever confirming it was done, and ran out before
seeing whether the final piece even compiled.

## Cost, measured

| | Prompt | Completion | Total tokens | Cost |
|---|---|---|---|---|
| A (default) | 1,339,932 | 15,605 | 1,355,537 | **≈$0.1733** |
| B (naive-v1) | 1,334,217 | 18,336 | 1,352,553 | **≈$0.1748** |
| **Combined** | | | 2,708,090 | **≈$0.3481** |

Both arms cost almost exactly the same in aggregate token count, which is
worth stating plainly because it could easily be read the wrong way: this is
**not** evidence that `naive-v1` was as efficient as `default`. It is the net
of two effects pulling in opposite directions — `naive-v1` needed fewer turns
on several tasks it got right quickly with no verification overhead to pay
for, and dramatically more on the five tasks that ran to the turn cap. The
per-task table above is the real picture; the aggregate total flatters
`naive-v1` by cancelling out its worst cases against its best ones.

## Does the harness actually matter, on this evidence?

**Directionally, yes — and this is the first result in this project's history
to show any daylight between two arms at all.** The tuned configuration went
13/13; the naive one went 12/13 and needed the full turn budget on 5 of its
13 tasks against `default`'s 1. Both signals point the same way: forced
verification, a nonzero repair budget, and multi-route parse tolerance
together buy something measurable, at least on this corpus and this model.

**What "no discordant pairs" meant for the model-axis comparisons doesn't
transfer cleanly here, and that distinction is worth stating precisely.** For
qwen-vs-minimax, zero discordant pairs meant "the corpus can't tell these two
models apart" — a statement about the corpus's difficulty relative to two
already-capable models. Here, one discordant pair plus a large gap in
turn-cap rate is closer to "the corpus *can* tell a naive harness from a
tuned one, but the specific policy differences tested don't push the
pass/fail needle hard on 13 tasks" — a statement about how much headroom this
particular naive baseline leaves before failures start compounding into
wrong answers rather than merely slower, more turn-hungry correct ones.

**This is not statistically significant, and one discordant pair from one
run is not strong evidence on its own.** ADR-0005's sequential-early-stopping
design exists precisely so a single run isn't over-read; a repeat run, or a
harder naive baseline (dropping the tool-set restriction KAN-1012's design
explicitly deferred), is the natural next data point rather than a
conclusion drawn from this one.

## What this does and does not show

- It confirms the harness mechanism works exactly as ADR-0007 anticipated:
  one binary, `--harness naive-v1`, two invocations, a real paired result —
  no rebuild, no new ADR, no declarative config needed.
- It shows `Verification.Forced: false` behaves precisely as
  `internal/engine/loop.go` documents: zero `VerificationRun` events, no
  blocking, the model left entirely to its own judgment about when it is
  done.
- It shows the first non-zero discordant-pair count and the first visibly
  different turn-cap rate this project's paired A/B methodology has ever
  produced — direct, if not yet statistically decisive, evidence for the
  project's own central thesis.
- It does **not** isolate which of the three changed fields (forced
  verification, repair budget, parse-route tolerance) is doing the work —
  per ADR-0007 decision 6/7, this result is about the bundle. A future card
  could vary each field independently against this same baseline to find out.
- It does not show anything about `qwen/qwen3-coder-next`'s general coding
  ability, or generalize past this corpus and this pin, per the same scope
  note every prior result in this series carries.

## Next steps, not started here

- **Repeat or extend this run** before treating the one discordant pair as
  more than a first data point — sequential early stopping (ADR-0005) is
  built for exactly this.
- **Isolate the three fields.** A `naive-v2` that changes only
  `Verification.Forced`, holding `RepairBudget` and `ParseRoutes` at their
  tuned values, would attribute a result to one axis instead of a bundle.
- **The tool-set axis KAN-1012 deliberately deferred** — dropping `edit_file`
  in favour of `edit_file_fuzzy` alone — is a real, separate naive lever this
  epic chose not to bundle in. Worth its own experiment if this baseline's
  signal holds up under repetition.
