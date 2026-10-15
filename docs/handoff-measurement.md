# Handoff against continuing in place (ADR-0026 decision 8, KAN-1968)

- **Observed:** 2026-10-10, live OpenRouter traffic, `qwen/qwen3-coder-next` pinned to
  `parasail/bf16` (see [`provider-pin.md`](provider-pin.md)), default harness config,
  corpus 2.1.0 (13 tasks), `kopibench` at `54d6242` plus the changes in this PR.
- **Card:** KAN-1968.

## The claim is still unmeasured

The question was: when a session hits the turn cap, does handing off (a fresh session
started from a handoff document) do as well as simply continuing? **The arm that answers it,
a handoff with the model's own narrative, was not measured.** The OpenRouter account ran out
of credit partway through the series (balance −$0.05 of $10 when checked), so the reruns of
that arm either failed outright or were cut off by HTTP 402 and are discarded.

What exists is a clean result for a *weaker* arm, a handoff carrying only kopicode's facts
block, because a bug made every narrative section blank. It is reported below as what it is.
Do not read it as the measurement ADR-0026 asked for.

## Protocol, fixed before the handoff arms ran

One pinned arm, run three ways over the whole corpus. Every arm has the same ceiling of 24
turns per task, so the comparison is about *how* the turns are spent.

| arm | how | flags |
|---|---|---|
| **A** continue | one session, 24 turns | `--max-turns 24` |
| **B** hand off | segments of 8 turns; at the cap, draft a handoff, close, start a fresh session from it in the same worktree; at most 2 handoffs | `--max-turns 8 --handoff-at-cap 2` |
| **C** floor | the cap alone, no handoff | `--max-turns 8` |

The draft is asked for the task statement as its goal. The next segment is told only the
handoff and is asked to continue; the model's tools and the working tree carry over, the
conversation does not. Turns, tokens and cost are summed over segments, handoff calls
included. The oracle grades the tree after the last segment. Runs were interleaved to spread
provider drift.

Eight tasks needed more than 8 turns in the first continue run, so a cap of 8 forces real
handoffs.

## What ran

`X` is arm B as it first ran: the handoff's narrative sections were all blank, so each new
segment was told only the facts block (files written, last verification, spend). It is a valid
measurement of that, and of nothing richer.

| arm | runs | passes per run | mean pass | mean turns | mean tokens | mean cost | handoffs / run |
|---|---|---|---|---|---|---|---|
| A continue (cap 24) | 3 | 12 / 13 / 13 of 13 | 12.67 | 165 | 1.42M | $0.121 | 0 |
| C cap 8, no handoff | 2 | 4 / 5 of 13 | 4.50 | 96 | 0.61M | $0.052 | 0 |
| X hand off, facts block only | 3 | 8 / 6 / 10 of 13 | 8.00 | 204 | 1.48M | $0.119 | 15.0 |

Per task (passes of runs, then turns per run), for the five tasks that decide it:

| task | A | C | X |
|---|---|---|---|
| py-calc-modulo | 3/3, 23/19/20 | 0/2 | 0/3, 24/24/24 |
| py-multi-module-exploration | 3/3, 11/11/13 | 0/2 | 0/3, 24/24/18 |
| go-multi-file-rename-contract | 2/3, 24/22/21 | 0/2 | 0/3, 24/24/24 |
| go-event-dispatch-ordering | 3/3, 20/15/12 | 0/2 | 1/3, 24/24/14 |
| go-interface-implementation-gap | 3/3, 17/13/17 | 0/2 | 1/3, 10/24/22 |

The full table is in [`bench-results/2026-10-10-handoff/`](bench-results/2026-10-10-handoff/)
with the saved results and `analyze.py`.

## What the data supports, and what it does not

- **A facts-only handoff beats stopping at the cap.** X passed 8.0 of 13 against 4.5 for C,
  so carrying the working tree and the facts into a fresh session recovered about three and a
  half tasks the cap alone loses.
- **It did not match continuing in place.** X passed 8.0 against 12.7 for A, for about the
  same tokens and cost. The tasks it lost are the ones A solved in 19 to 24 turns, and X used
  all 24 on them and still failed. The likeliest reading is that each fresh segment spends
  turns re-reading files and has too few left to act, but this data does not show that; no
  segment-level analysis was done.
- **n is 3 per arm, one model, one corpus.** A is already at 12.7 of 13, so the corpus has
  almost no headroom above the continue arm and cannot show a handoff doing *better*. It can
  only show how much a handoff gives up.
- The runs report `tree state unknown`, so under ADR-0007 decision 7 they are not poolable
  with any other run.
- Cost is small: about $0.12 for a full run, so the whole series is a few dollars. The block
  was credit, not price.

## What measuring found

Two defects in the handoff itself, neither visible from tests against a scripted provider:

1. **A blank narrative, silently.** Asked for a handoff with no tools on the request and a
   history full of tool calls, the model returned an empty completion (`finish_reason: stop`,
   no content, no tool calls). The drafter rendered a document whose every section said "the
   previous session did not say" and carried on. That is arm X. Fixed: the draft now treats a
   reply with no sections as a failed draft, the raw reply is journaled on `HandoffWritten`,
   and the engine reads its facts through its own journal (a runner may keep journals outside
   the working directory).
2. **A tool call instead of text.** With the loop's tools on the request and "do not call any
   tool" in the prompt, the model returned text on the probe but a tool call on some tasks.
   The two failure modes differ, so a draft now tries with the tools, then without, then with
   again, and reports an error only if all three come back empty. I changed the tool list and
   the prompt together, so I cannot say which of the two cured the first failure.

## To finish the measurement

Add credit, then run arm B three times with this branch's `kopibench` and compare against the
saved A and C runs:

```bash
kopibench run --corpus bench/tasks --max-turns 8 --handoff-at-cap 2 --save B1.json
python3 docs/bench-results/2026-10-10-handoff/analyze.py <dir of A*.json B*.json C*.json>
```

Also worth running: a corpus with tasks that need more than 24 turns, where the continue arm
fails too, since that is where a handoff could win; and a cap other than 8.

---

# Second measurement: the narrative arm, on `deepseek/deepseek-v4.1-flash` (KOP-171)

- **Observed:** 2026-10-16, live OpenRouter traffic, `deepseek/deepseek-v4.1-flash` pinned to
  `deepinfra/fp8` ([`provider-pin.md`](provider-pin.md)), default harness config, corpus 2.1.0
  (13 tasks), build tree state `unknown`.
- **Why a different model:** `qwen/qwen3-coder-next` was blocked by an account guardrail
  when this was run, and a pinned arm cannot change model mid-comparison, so all three arms
  were rerun on v4.1-flash. Nothing below compares with the Qwen numbers above.
- **Results:** [`bench-results/2026-10-16-handoff-v41-flash/`](bench-results/2026-10-16-handoff-v41-flash/)
  (`cap8/`, `cap3/`, and `analyze.py <dir> <continue cap> <segment cap> <max handoffs>`).

Unlike the first measurement, the handoff here is the real one: the drafting fixes above are
in, so the next segment is told the model's own narrative as well as the facts block.

## Protocol 1, cap 8: no handoff ever fired

Same protocol as above (A 24 turns, B segments of 8 with up to 2 handoffs, C cap 8).

| arm | passes per run | mean pass | mean turns | mean cost | handoffs / run |
|---|---|---|---|---|---|
| A continue (cap 24) | 13 / 13 / 13 | 13.00 | 69 | $0.013 | 0 |
| B hand off at cap 8 | 13 / 13 / 13 | 13.00 | 66 | $0.013 | 0 |
| C cap 8, no handoff | 13 / 13 / 12 | 12.67 | 72 | $0.013 | 0 |

v4.1-flash solves every task in 4 to 9 turns, so almost nothing reaches a cap of 8 and B
never handed off. **This measures nothing about handoff**; it only shows the corpus is too
easy for this model at that cap, as the card predicted.

## Protocol 2, cap 3: the same 12-turn ceiling, handoffs forced

| arm | how | passes per run | mean pass | mean turns | mean tokens | mean cost | handoffs / run |
|---|---|---|---|---|---|---|---|
| A continue | one session, `--max-turns 12` | 13 / 13 / 13 | 13.00 | 68 | 0.42M | $0.012 | 0 |
| B hand off | segments of 3, `--handoff-at-cap 3` | 11 / 12 / 13 | 12.00 | 142 | 0.99M | $0.026 | 34.3 |
| C floor | `--max-turns 3` | 6 / 6 / 7 | 6.33 | 39 | 0.19M | $0.005 | 0 |

## What the data supports, and what it does not

- **A handoff recovers most of what a small cap loses.** B passed 12.0 of 13 against 6.3 for
  C: about 5.7 of the 6.7 tasks the cap alone drops.
- **It still gives something up against continuing in place.** A passed 13.0 in every run,
  B 12.0, and B cost about 2.2 times as much (0.99M tokens against 0.42M), because a task
  A finishes in 4 to 7 turns used all 12 in B on most tasks. The two tasks B lost most are
  `go-event-dispatch-ordering` (1 of 3) and `go-interface-implementation-gap` (2 of 3).
- **Why B spends all 12 turns on easy tasks is not explained.** Likely candidates are
  re-reading and re-verifying in each fresh segment, but no segment-level analysis was done,
  so this is a guess.
- **n is 3 per arm, one model, one corpus, one cap.** The cap-3 protocol is artificial: it
  forces handoffs on tasks the model solves in a handful of turns, so it shows the cost of
  a handoff when one is forced, not how often a real long task would need one.
- **The case a handoff could win is untested.** Both protocols give A enough turns to finish
  every task. A corpus with tasks that defeat the continue arm, and a model weak enough to
  need it, is still missing.
- The runs report `tree state unknown`, so under ADR-0007 decision 7 they are not poolable.
- Total spend for both protocols, 18 full runs plus a probe, was a few tens of cents.

The ADR's claim moves from "unmeasured" to "measured once, with these limits".
