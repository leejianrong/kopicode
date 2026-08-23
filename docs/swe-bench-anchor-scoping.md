# SWE-bench-Verified anchor: scoping spike (KAN-998)

- **Status:** Scoping only. No code change, no corpus change, no runner.
- **Answers:** whether to pull in "a small SWE-bench-Verified subset run rarely as
  an anchor" now, deferred by [ADR-0005](adr/0005-benchmark-and-ab-methodology.md)
  §6, for external comparability — explicitly **not** a replacement for the
  in-house corpus.
- **Recommendation up front:** not worth doing now. Worth revisiting once a second
  harness config exists to compare against a first. Reasoning below.

This spike does not touch `bench/tasks/`, `corpus.json`, or `internal/corpus`, and
does not implement a SWE-bench runner. It reads ADR-0005, `bench/tasks/README.md`,
`internal/corpus`, and ADR-0008/0011, and reasons from general knowledge of
SWE-bench-Verified's public composition. **I had no network access while writing
this** — every claim about SWE-bench-Verified's actual instance data (repo names,
dependency footprints, environment setup mechanics) is stated from prior general
knowledge of the benchmark, not a live lookup against the dataset or its paper.
Anyone treating a number in §1 as load-bearing should verify it against the
current `princeton-nlp/SWE-bench` repository and the SWE-bench-Verified dataset
card before using it in a decision that costs money.

## 1. What a SWE-bench-Verified instance actually requires to run

SWE-bench-Verified is OpenAI/Princeton's human-filtered subset of the original
SWE-bench, roughly 500 instances (down from SWE-bench's ~2,294) that a human
annotator confirmed have a well-scoped issue, a correct reference patch, and a
FAIL_TO_PASS/PASS_TO_PASS test list that actually discriminates. That filtering
step is exactly why it's attractive as a comparability anchor — but it filters for
issue *quality*, not for infrastructure lightness. The infrastructure demands of
the full dataset carry over unchanged:

- **A small, fixed set of real GitHub Python repositories** — `django/django`,
  `sympy/sympy`, `matplotlib/matplotlib`, `scikit-learn/scikit-learn`,
  `sphinx-doc/sphinx`, `pytest-dev/pytest`, `pylint-dev/pylint`,
  `astropy/astropy`, `psf/requests`, `pydata/xarray`, `mwaskom/seaborn`,
  `pallets/flask`, and a few more — each contributing instances at specific
  historical commits. This is a closed, fixed repo list, but it is **not**
  kopicode's two guaranteed toolchains; it's one language (Python) at a
  research-software scale kopicode's own corpus deliberately avoids (`bench/tasks/`
  tasks are small, single-purpose repos with their own throwaway `go.mod` or a
  standalone script — nothing like Django's or SymPy's own dependency graph).
- **A per-repo, often per-version, environment.** Each instance pins a specific
  commit of its repo *and* the dependency versions that were current for that
  commit — different Django versions want different Python versions and
  dependency pins from different SymPy versions. The upstream SWE-bench harness
  handles this with a matrix of conda/pip environments (and, in newer harness
  versions, prebuilt Docker images per repo+version) precisely because "install
  whatever `requirements.txt` says" does not reliably reproduce a multi-year-old
  dependency set on a fresh machine.
- **Network access during environment construction**, not during the coding turn
  itself. Building the instance environment (conda solve, `pip install` against
  PyPI, sometimes a specific compiler toolchain for a repo with C extensions
  like `scikit-learn` or `matplotlib`) needs the network the first time an
  instance's environment is materialized. The published harness mitigates the
  *repeated*-run cost with prebuilt images, but producing those images at all is
  a network-and-build step, and a from-scratch reproduction (which is what "we
  ran the anchor" should mean, not "we downloaded someone else's image") pays
  that cost.
- **Languages/toolchains beyond kopicode's current two.** SWE-bench-Verified is
  Python-only, which sounds like an overlap with kopicode's existing `python3`
  stdlib toolchain — it is not the same thing. `bench/tasks/README.md` guarantees
  "`python3` with the standard library" specifically *because* that needs nothing
  installed. SWE-bench-Verified tasks routinely need NumPy, SciPy, Cython-compiled
  extensions, Sphinx's own doc-build toolchain, or Django's test settings module —
  real third-party dependency graphs with C-extension builds, not stdlib-only
  scripts. "Python" as a label hides a completely different weight class of
  dependency footprint from what this project currently promises.
- **Per-instance setup and run time measured in minutes, not seconds.** Building
  an environment (even from a cached image) and running a repo's real test suite
  for FAIL_TO_PASS/PASS_TO_PASS verification is far slower than this corpus's
  fast, hand-sized suites — Django's or scikit-learn's test suites are not
  "runs in under a second" the way `bench/tasks/README.md`'s tasks are designed
  to be.

None of this is a criticism of SWE-bench-Verified — it's testing exactly what it
says it tests: can a model or harness patch a real, large, actively-maintained
open-source Python codebase. It is simply a different infrastructure problem than
"run a self-contained ≤20-turn task with no network and two guaranteed
toolchains," which is the problem `bench/tasks/README.md` was written to solve.

## 2. Where this collides with kopicode's existing determinism guarantees

`bench/tasks/README.md` states five guarantees that every in-house task and the
loader that reads it are built around, and SWE-bench-Verified instances violate
essentially all of them by construction, not by omission:

| Guarantee (`bench/tasks/README.md`) | SWE-bench-Verified instance |
| --- | --- |
| Two toolchains only: Go, `python3` stdlib — `internal/corpus` rejects anything else (`knownLanguages` in `internal/corpus/validate.go` is literally `{"go": true, "python": true}`, and that `python` means stdlib-only, not "Python with arbitrary pip dependencies") | Real third-party dependency graphs per repo (NumPy/SciPy/Cython/Sphinx/Django's own settings machinery), version-pinned per instance |
| No network, structurally (`GOPROXY=off`, stdlib-only Python, a test that asserts these are set) | Network needed to materialize each instance's environment the first time; the upstream harness's answer is a prebuilt image, which pushes the network dependency to "someone built this image," not "there is none" |
| Byte-stable checkout, frozen corpus digest over every task file | A SWE-bench instance's "starting tree" is a real repo checked out at a real historical commit — reproducible in principle (git is deterministic), but it is gigabytes of unrelated repo history and a live upstream project, not a small hand-authored tree that fits cleanly into a SHA-256 digest the way `internal/corpus.Digest` computes one today |
| ≤20 turns per task (`internal/corpus/validate.go`'s `maxTurnsCap`, ADR-0005 §6) | SWE-bench tasks are drawn from real GitHub issues against large, unfamiliar-to-the-model codebases — agentic scaffolds evaluated against SWE-bench routinely run tens of tool calls per instance to explore before editing; a ≤20-turn cap tuned for a small hand-sized task is not calibrated for "find the right file in Django's ORM layer first" |
| ~$2 budget for the whole corpus (`bench/tasks/README.md`'s "keep it small... has to stay near $2") | A handful of large-codebase instances, each needing exploration turns over a much bigger context, is a different cost regime entirely — see §3 |

The mechanical consequence: **`internal/corpus.Load`'s digest check and
`knownLanguages` allowlist are not a bolt-on point for SWE-bench-style tasks —
they are specifically built to reject them.** `internal/corpus/validate.go` fails
a task whose `Language` isn't `"go"` or `"python"` (stdlib), and even a task that
passed that check would still need to satisfy `MinTasks`, the digest-over-every-
task-file scheme, and the turn cap — none of which are the right shape for "one
`django/django` checkout at a pinned commit, its own environment, and its own
much larger turn budget."

This isn't a gap to patch in `internal/corpus`; it's a sign an anchor run needs a
**deliberately separate code path** — its own small package (something like
`internal/sweanchor` or a standalone tool under `cmd/`), its own manifest format,
its own environment-provisioning step, and its own harness config wiring — rather
than a new task type accepted by the existing loader. Trying to make one loader,
one digest scheme, and one turn cap serve both the in-house corpus's guarantees
and SWE-bench's infrastructure needs would either weaken the in-house guarantees
(the failure mode ADR-0006/ADR-0005 exist to prevent — an unpinned, undated
capability number is not evidence) or bury SWE-bench-shaped exceptions inside a
loader whose entire value is that every task it accepts satisfies the same
determinism contract.

## 3. Cost estimate for a small subset (5-10 instances)

Two different costs are worth separating: the one-time infra lift, and the
per-run marginal cost once that infra exists.

**Per-instance setup overhead (one-time-ish, but repeats per environment
refresh):**

- Building or pulling a repo-and-version-specific environment: minutes per
  instance the first time, even with the upstream harness's prebuilt Docker
  images (pulling a multi-hundred-MB-to-multi-GB image per repo/version is not
  free, and kopicode has no existing container pull/build path at all — see §4).
- Verifying FAIL_TO_PASS/PASS_TO_PASS locally before trusting an instance,
  because (mirroring `bench/tasks/README.md`'s own "verify in both directions"
  discipline for the in-house corpus) an un-verified SWE-bench oracle is exactly
  as untrustworthy as an un-verified in-house one — this project would not accept
  a task oracle it hadn't run both ways for its own corpus, and shouldn't for a
  borrowed one either.
- No existing kopicode machinery for any of this: no container runtime call, no
  network allowlist, no per-repo environment cache. All of it is new
  infrastructure, not configuration of something already built.

**Turn count, and why it changes the cost model, not just the total:**

`bench/tasks/README.md`'s ≤20-turn cap fits tasks sized so a model can read the
relevant file(s) and make a small, mostly-local fix. SWE-bench instances are
drawn from real maintainer-filed issues against codebases the model has never
seen the layout of — published agentic evaluations against SWE-bench-family
benchmarks commonly need substantially more exploration (locating the right
module, reading surrounding tests, iterating on a failing patch) than a
20-turn-capped small-repo task ever does. Even discounting the exact figures
(unverifiable without network access here), the qualitative point is not in
doubt: a SWE-bench-Verified instance is a **much larger context, much more
exploratory task** than anything in `bench/tasks/`, and would need a distinct,
larger turn/token budget rather than borrowing the in-house cap.

**Rough dollar figure, scaled from the one real measurement this project has:**

KAN-800's real run is the only actual cost data point: the full 10-task in-house
corpus (now growing toward 13 tasks via a parallel effort in this epic) cost
**$0.0846** total against `qwen/qwen3-coder-next` — 621,368 prompt tokens and
12,587 completion tokens across 10 tasks, i.e. roughly $0.008-$0.01 per task at
this corpus's size and turn discipline (`docs/provider-pin.md`). Scaling that
per-task figure directly to SWE-bench instances would be misleading, because the
two things that dominate cost — prompt tokens (much larger real-codebase context:
reading unfamiliar files, larger diffs, longer test output) and turns (more
exploration before the model can even attempt a fix) — both scale up
independently for a SWE-bench instance relative to an in-house task, not just the
per-turn cost. A defensible order-of-magnitude estimate: if a SWE-bench-Verified
instance runs even 2-3x the turns of an in-house task, each carrying a
multiple-times-larger prompt (real project source and test output rather than a
small self-contained repo), a single instance could plausibly cost **on the order
of $0.05-$0.30**, not the ~$0.008-$0.01 an in-house task costs today — call it a
rough 5-30x per-instance multiplier, wide because there is no real measurement to
narrow it, only the qualitative direction (more tokens, more turns) argued above.
A 5-10 instance subset therefore lands somewhere in the **$0.5-$3 range** in raw
provider cost per anchor run — plausibly comparable to or exceeding the entire
in-house corpus's own budget for a single anchor pass, before counting the
one-time environment-provisioning lift in §4, which has no dollar equivalent
today because it doesn't exist as infrastructure at all. Treat this as a rough
bound to reason with, not a number to budget against without a real measurement.

## 4. Recommendation: not worth it yet, revisit after a second harness config

**Verdict: not worth doing now.** Worth doing once a second harness config exists
to actually compare against a first. Reasoning:

**An anchor with nothing to anchor a comparison to isn't doing the job ADR-0005
§6 assigns it.** ADR-0005 §6 frames the SWE-bench-Verified subset explicitly as
"publicly anchored" and "for external comparability" — a number that lets someone
outside this project sanity-check that the harness's in-house deltas correspond
to something recognizable. ADR-0005 §7 ("No add-on catalogue in slice 1. One
hardcoded harness, one model... plugin axes are invented after a second model has
been made to work") and CLAUDE.md's own build-status section confirm: **there is
currently one registered model and one harness config.** A SWE-bench-Verified run
today would produce exactly one number, with no second arm to compare it against
and no in-house delta it could corroborate. That's not "the anchor is useless" —
a single absolute number does have some value as a sanity check against inflated
in-house numbers — but it isn't the comparability *ADR-0005 §6 is actually asking
for*, which is explicitly about deltas and external comparability once there's
something to compare.

**The infra lift is bigger than a corpus content decision, and this project has
already scoped that exact category of decision once, carefully, and said not
yet.** ADR-0008 (shell isolation, accepted risk) and its amendment ADR-0011
(the unattended-invocation policy gate) both establish, at length, that kopicode
today runs on a **single-trusted-developer trust model with no sandbox**, and
that adding real containment (Docker/OCI, Linux namespaces, seccomp/Landlock) was
surveyed and explicitly rejected for slice 1-and-beyond scope: a container
runtime is a second runtime dependency the ADR-0001 single-static-binary
distribution promise doesn't carry, and platform-specific sandboxing is
Linux-only in its lighter forms. Pulling in a SWE-bench-Verified anchor would
reintroduce almost exactly that same infra question from a different angle:
- **Repo-specific dependency installs at real weight** (NumPy/SciPy/Cython
  builds, Sphinx's toolchain, Django's settings machinery) are exactly the kind
  of "heavier, less-trusted install" that ADR-0008's trust model doesn't cover
  cleanly even for a benchmark task — `internal/bench`'s own doc comment already
  says the bench runner "is not a security boundary... do not point this at
  untrusted tasks," and a SWE-bench Python dependency chain is a much larger
  attack/breakage surface than this corpus's own tiny stdlib-only or
  no-dependency Go modules.
- **A real network allowlist** doesn't exist anywhere in kopicode today —
  `bench/tasks/README.md`'s whole determinism story rests on the *absence* of
  network need, not on a permission gate over it. Building one is new
  infrastructure, and building it narrowly enough to be useful (allow PyPI/conda
  for environment setup, nothing else) is nontrivial policy work of exactly the
  kind ADR-0011 built for a different, already-hard problem (cuttlefish's
  unattended invocation).
- **A separate cost/turn budget** is not just a config value — it's a second
  harness dimension (a much larger `MaxTurns`, a much larger context budget) that
  doesn't fit `internal/corpus`'s per-task `max_turns` cap or ADR-0005 §6's ≤20-
  turn design point, reinforcing §2's point that this needs its own path, not a
  parameter on the existing one.

None of this is a blocker forever — it's a statement that the SWE-bench anchor is
a **new environment-isolation and cost-governance commitment**, not a corpus
content decision like adding an eleventh in-house task. Conflating the two
would understate what's actually being asked for.

**What would change the answer:** once a second harness config exists (ADR-0007's
precondition for "the plugin axes are invented" and ADR-0005 §7's stated
trigger), there is something for an external anchor to actually validate — does
the in-house delta between config A and config B show up, in the same direction,
against a benchmark the rest of the field also reports on. That is the point at
which paying the infra cost buys something ADR-0005 §6 is actually asking for.
Until then, a from-scratch SWE-bench-Verified run would cost real engineering
time building isolation infrastructure this project has twice now (ADR-0008,
ADR-0011) deliberately chosen not to build yet, in exchange for one number with
nothing to compare it to.

**Concrete recommendation:** leave ADR-0005 §6's deferral exactly as written.
Revisit this spike's conclusion as a fresh scoping pass — not a silent
retrofit — once (a) a second harness config lands and needs an external sanity
check, and (b) a decision is made about who pays for isolation (in-house sandbox
work inside kopicode/kopibench, in the spirit ADR-0008 rejected doing yet, versus
an operator-provisioned container the way ADR-0011 pushed containment onto
cuttlefish as the invoking orchestrator rather than kopicode's own binary — the
same split would apply here: `cmd/kopibench` could plausibly depend on an
opt-in, operator-provisioned container runtime for a SWE-bench worker mode
without that becoming a dependency of `cmd/kopicode` itself, mirroring exactly
what ADR-0008's trigger-condition section already proposes for the bench runner
generally).
