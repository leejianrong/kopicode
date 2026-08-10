# ADR-0005: Benchmark and A/B methodology

- **Status:** Accepted
- **Date:** 2026-08-11
- **Deciders:** Jian (leejianrong2@gmail.com)

Replaces the "Evaluation and API Strategy" section of `agentic-harness-ideas.md`,
and the "Add-on Catalog" section it depended on.

Extended by [ADR-0006](0006-hash-anchored-edits-and-failure-attribution.md), which adds
the third `unattributed` bucket to failure classification. Read it alongside this one:
a two-bucket classifier lets an undetectable harness defect be counted as model
incapacity, which silently corrupts every delta this ADR is built to measure.

Extended by [ADR-0007](0007-model-selection-and-harness-config-shape.md), which defines
the **arm** this ADR compares — (model × harness configuration × provider pin) — and the
shape a harness configuration ships in. Decision 7 below defers *what the plugin axes
are*, and that deferral stands; ADR-0007 settles only what kind of thing holds them, and
supersedes §7's word "hardcoded" without touching its substance.

## Context

The project thesis is that harness changes move measured coding ability by 10–25
percentage points, and that this can be exploited for open-weight models. Proving a
claim of that shape needs an experiment design, and the original plan's design does
not work.

**The sample size cannot resolve the effect.** The plan proposed 20–100 tasks with
pass/fail unit tests, run as: baseline → toggle one add-on → score delta. At n=20 and
a ~40% pass rate, the 95% confidence interval on a single score is roughly ±21
percentage points. An unpaired difference of two such scores is noise at the exact
magnitude the project exists to detect.

**Provider routing is an uncontrolled variable.** OpenRouter load-balances across
providers that differ in quantization and serving configuration. Two runs of "the
same model" can be different weights. Any A/B result gathered without pinning is
unfalsifiable.

**The add-on catalogue presupposes its own axes.** Three plugin families (Parsers,
Context Engines, Middleware) with toggleable combinatorics assumes the axes are
already known. They are not, and the combinatorial matrix multiplies cost by exactly
the number of guesses.

**Most regressions are not capability regressions.** A broken edit tool, a
non-terminating loop, a parser that rejects a valid call — these dominate day-to-day
breakage and cost nothing to detect against real capability.

## Decision

**1. Paired comparison, not difference of means.** Every A/B runs both arms over the
**identical task set** and scores the **flips**: tasks A passes and B fails, and
vice versa. Report McNemar's test on the discordant pairs. Tasks both arms handle the
same way carry no information about the delta, and pairing removes task difficulty as
a variance source. This is what makes a modest task count usable.

**2. Pin the provider on every benchmark request.** `provider.order` set to a single
slug, `allow_fallbacks: false`, and `quantizations` fixed. The pinned provider,
quantization, model id and all sampling parameters are recorded in the journal's
`ProviderRequest` event, and a result whose pin does not match the experiment's
declared pin is discarded, not adjusted.

**3. A mock/replay provider is part of slice 1.** It serves recorded
`ProviderResponse` events from the journal. Harness plumbing — parse-and-repair,
edit application, loop termination, permission gating, context assembly — is tested
against it at **zero token cost**, in CI, on every change. This is the largest single
cost saving available and it is also the fastest test suite.

**4. Cost controls on the capability matrix**, in order of savings:

- **Sequential testing with early stopping.** Run tasks in a fixed order and stop an
  arm when the paired difference is conclusive, or conclusively null. Typically 3–5×.
- **Prune non-discriminating tasks.** After a baseline pass, drop tasks that every
  arm passes and tasks that no arm passes; keep the discriminating middle. This is
  precisely what an in-house corpus can do and a fixed public benchmark cannot.
- **Small smoke set on every change, full matrix rarely.** A ~10-task set gated in
  CI; the full corpus weekly or before a claim.
- **Prefix caching where the arms allow it.** Helps variants that differ late in the
  prompt; does nothing for variants that change the system prompt. A smaller lever
  than it appears — do not design the experiment around it.

**5. Unit-test oracles only. No LLM judge.** Grading cost stays near zero and the
oracle cannot drift between arms.

**6. In-house corpus, publicly anchored.** Tasks are small-repo, fast-suite,
deterministic, and complete in ≤20 turns, drawn from the kind of work kopicode is
actually for. The failure mode is overfitting and non-comparability with published
numbers, so a small SWE-bench-Verified subset is run rarely as an anchor. Any
externally quoted SWE-bench figure is treated as unverified until reproduced here.

**7. No add-on catalogue in slice 1.** One hardcoded harness, one model. The plugin
axes are invented after a second model has been made to work, from evidence about
what actually differed. Until then there is nothing to toggle and no matrix to run.

**8. Report what was dropped.** Early stopping, task pruning and smoke-set gating all
bound coverage. Every result states its task count, its stopping rule, and what was
excluded. A pruned corpus reported as a full one is a false claim.

## Consequences

- Absolute pass rates from this rig are not comparable to published leaderboards, by
  construction. Deltas within the rig are what it is built to measure; the
  SWE-bench-Verified anchor is the only comparable number.
- Provider pinning means a pinned provider disappearing invalidates an experiment
  series mid-flight. Record the pin per result so the break is visible rather than
  silent.
- Task pruning risks selecting a corpus that flatters whichever arm defined the
  baseline. Mitigation: prune once against the baseline arm, then freeze the corpus
  for the experiment series and version it.
- The mock provider must be kept honest — if it drifts from real provider behaviour,
  green plumbing tests will mask real breakage. It replays recorded traffic rather
  than synthesising it, for that reason.
