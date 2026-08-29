# Standardized benchmark scoping: options, cost, and whether a subset suffices

- **Status:** Scoping only. No code change, no corpus change, no runner.
- **Answers:** Jian's 2026-08-29 planning-round ask — "let's make plans for a
  standardized benchmark. start with evaluating the benchmark options, cost
  analysis, and whether a subset of a benchmark would suffice." Widens
  [`swe-bench-anchor-scoping.md`](swe-bench-anchor-scoping.md) (KAN-998) from one
  named benchmark to the actual landscape, and corrects it where new evidence
  changes the answer.
- **Recommendation up front:** two separate answers, because they answer two
  different questions. **Do Aider's polyglot benchmark first** — a stratified
  Go+Python subset, buildable inside kopicode's current trust model with no new
  isolation commitment and no new ADR. **Leave SWE-bench-Verified (or its
  established 50-instance "Mini" subset) exactly where ADR-0005 §6 and KAN-998
  already left it** — deferred, not on cost grounds anymore (bounded and
  affordable below), but on the unresolved containment question KAN-998 named
  and this scoping pass does not resolve.

This spike does not touch `bench/tasks/`, `corpus.json`, `internal/corpus`, or
`internal/bench`, and does not implement a runner for anything named below. Unlike
KAN-998, this pass had live network access — every claim below is sourced (see §6),
and where KAN-998 flagged something as "unverified without network access," this
document either confirms it or corrects it explicitly.

## 1. The landscape, and what's ruled out immediately

| Benchmark | Measures | Tasks | Languages | Scoring |
| --- | --- | --- | --- | --- |
| SWE-bench Verified | Resolve a real GitHub issue in an existing large repo | 500 | Python only, 12 repos | Real unit tests (FAIL_TO_PASS/PASS_TO_PASS) |
| SWE-bench Verified (Mini) | Same | 50, an established random subset | Python only | Same |
| SWE-bench Lite / Full | Same, filtered differently | 300 / 2,294 | Python only | Same |
| Multi-SWE-bench | Same shape, multilingual | 1,632 full, 400 "lite" | Java, TS, JS, **Go**, Rust, C, C++ | Same shape, per-repo Docker |
| **Aider polyglot benchmark** | Solve + iteratively fix a self-contained Exercism exercise from error feedback | 225 (hardest of 697) | C++, **Go**, Java, JS, **Python**, Rust | Hidden Exercism unit tests |
| Terminal-Bench 2.0 | General agentic terminal tasks (compile/configure/debug) | 89 | N/A | Oracle + verification suite, containerized |
| BigCodeBench | Single-turn function-call generation against library APIs | ~1,140 | Python | Unit tests, sandboxed |
| LiveCodeBench | Competitive-programming problem solving | 511–880+ | Python-centric | Pass@1, hidden tests |
| Commit0 | Build a whole library from a spec (greenfield) | 54–57 | Python | Unit tests |
| RepoBench | Repo-level code *completion* | varies | Python, Java | Similarity match, not pass/fail |
| HumanEval / MBPP | Single function, no repo context | 164 / ~1,000 | Python | Unit tests |

**Ruled out immediately, with reasons, rather than silently dropped:**

- **HumanEval/MBPP** — function-in-a-vacuum, no repo context. Tests nothing about
  anchor-based editing, navigating a codebase, or verification-after-edit — R1's
  actual bar ("land a correct diff in a real repository... know whether it worked
  by running that repository's own tests").
- **LiveCodeBench, BigCodeBench** — single-turn generation or function-call
  synthesis against a fresh sandbox, not "read this repo, find the bug, edit it,
  verify." Same category error as HumanEval one level up.
- **RepoBench** — measures completion accuracy (exact/edit-similarity match), not
  task success. No pass/fail oracle at all, which is this project's actual bar
  (ADR-0006 §9's three-bucket classifier assumes a real oracle exists).
- **Terminal-Bench** — agentic and containerized, but general terminal
  competence, not issue-resolution-in-an-existing-codebase. A different research
  question than kopicode's.
- **Commit0** — greenfield generation from a spec, the opposite of kopicode's
  "edit an existing tree" design center (R1, R3).

That leaves three real candidates: **SWE-bench-Verified(-Mini)**,
**Multi-SWE-bench**, and **Aider's polyglot benchmark**.

## 2. Environment fit against `internal/bench`

kopicode's bench runner today: one `git worktree` per task off a frozen corpus
commit, no container, no network (`bench/tasks/README.md`'s guarantee —
`GOPROXY=off`, stdlib-only Python), ≤20 turns, ~$2 total budget, and
ADR-0008/0011's explicit "one trusted developer, their own machine" trust model
with containment declared out of kopicode's own scope.

| Benchmark | Per-task environment | Network needed | Fits `internal/bench` as-is? |
| --- | --- | --- | --- |
| SWE-bench-Verified(-Mini) | Repo-and-commit-specific Python env; official harness ships prebuilt Docker images (≈30 GiB for all 500) | Yes, to build/pull images | **No.** The exact gap KAN-998 already named: real third-party dependency graphs (NumPy/SciPy/Cython/Sphinx), version-pinned per instance, need a container runtime kopicode has twice declined to build in (ADR-0008, ADR-0011). |
| Multi-SWE-bench | Same per-repo/per-language Docker shape, more languages | Yes | **No**, same reason, worse: 8 language toolchains instead of 1. |
| Aider polyglot | Each exercise is a small, self-contained, single-directory problem — a stub file plus a hidden test file, **stdlib-only by construction** (that's the point of an Exercism exercise) | **No** | **Yes, closely.** Structurally almost identical to what `bench/tasks/` already is. |

**This is the headline correction to KAN-998's framing.** KAN-998 treated
"external benchmark" as synonymous with "SWE-bench-shaped, therefore needs new
container infra" — reasonable given it only evaluated SWE-bench-Verified. Aider's
polyglot benchmark is external, widely reported-against (it has its own published
leaderboard), and does **not** carry that infra tax — and it already covers Go and
Python, kopicode's two guaranteed toolchains today.

**One real caveat, named rather than glossed over:** Aider's own `benchmark.py`
harness is written to drive Aider specifically (its CLI, its edit formats). Using
the 225 exercises for kopicode means treating them as **raw task data** — a stub
file plus a hidden test suite per exercise, structurally interchangeable with a
`bench/tasks/` entry — not running Aider's own harness code. That's
`internal/corpus`-shaped integration work (a manifest per exercise, possibly a
sibling loader alongside the in-house corpus's), not `internal/bench`-isolation
work. Materially smaller than what SWE-bench-Verified would need, but still real
work, not a config flag.

## 3. Cost analysis

**SWE-bench-Verified, real published figures (frontier models, SWE-agent-class
scaffolds):** Claude 3.5 Sonnet ≈ $67.09 for the full 500 (≈ $0.134/instance),
GPT-4o ≈ $79.84 (≈ $0.16/instance), o1-mini ≈ $366.81 (≈ $0.73/instance). These are
frontier-model prices; kopicode's target model class (qwen3-coder-next and
similar, on OpenRouter) runs at a fraction of frontier per-token pricing, offset
somewhat by SWE-bench's much larger unfamiliar-codebase context and higher
exploration-turn count relative to kopicode's own corpus. **Defensible
order-of-magnitude estimate for kopicode's own harness: $0.02–$0.15/instance**,
tighter than KAN-998's un-anchored $0.05–$0.30 guess now that there's a real
reference point to scale from. A 50-task Verified-Mini run: **roughly $1–$8
total** — affordable, and no longer the blocker KAN-998's cost section worried it
might be.

**Aider polyglot, order-of-magnitude:** exercises are Exercism-sized — much
closer to kopicode's own corpus tasks (single-purpose, small stub file, no
unfamiliar-codebase exploration) than to a SWE-bench instance. Reasonable to
scale directly from kopicode's own measured **$0.0065–$0.01/task** (KAN-800,
13-task corpus, qwen3-coder-next), with a 1.5–3x multiplier since Aider's
polyglot set is deliberately the hardest 225 of 697 Exercism exercises.
**Estimate: $0.01–$0.03/task; a 40–50 task subset comes in well under $2** —
cheaper than kopicode's own existing 13-task corpus run, because there is no
container/environment tax at all.

**Multi-SWE-bench:** same cost profile as SWE-bench-Verified (identical
per-repo Docker/dependency weight, just more languages) — no reason to expect a
materially different number, so reuse the SWE-bench estimate above.

**Bottom line: cost is not the blocker for any of the three candidates.** The
blocker for the two SWE-bench-family options is entirely the containment
question in §2, unchanged from KAN-998.

## 4. Does a subset suffice, and what would it look like

**Yes, with real precedent on both the SWE-bench side and structurally for
Aider's set:**

- **SWE-bench Verified (Mini)** is an established, citable, 50-instance random
  subset of the 500, reported as tracking "approximately the same distribution
  of performance, test pass rate, and difficulty as the original dataset" —
  used across multiple published evaluations specifically to cut cost. This is
  exactly the shape ADR-0005 §6 already anticipated: "a small SWE-bench-Verified
  subset run rarely as an anchor... not a replacement for the in-house corpus."
  If SWE-bench is ever picked up, Verified-Mini is the subset to adopt rather
  than build a new one.
- A separate, independent precedent (SWE-bench-Lite-based) used stratified
  random sampling (seed=42, 50 instances, stratified across repository
  categories) specifically for token-cost reasons — a second data point for
  stratifying by repo/category rather than drawing purely at random.
- **Aider's polyglot set is already itself a curated subset** (225 of 697
  Exercism exercises, the hardest ones). There is no equivalent published
  "sub-subset" the way SWE-bench has Verified-Mini, but a stratified-by-language
  draw — say, 15–20 exercises each from the Go and Python columns specifically,
  matching kopicode's two guaranteed toolchains — would be an easy, defensible
  selection given the full set's small size.

**Conclusion: yes, a subset suffices for either path.** For SWE-bench, there's a
ready-made, already-legitimized 50-task subset to adopt rather than construct.
For Aider, a stratified Go+Python slice of 15–20 exercises each is a reasonable,
easily-justified first cut.

## 5. Recommendation

**Two honest answers, because they answer different questions — restated from the
top with the reasoning behind each:**

**If the goal is "does kopicode's number correspond to something the field
already recognizes,"** SWE-bench-Verified (Mini) remains the right long-term
anchor — it's the one number every outside reader already has a reference point
for. But **the blocker KAN-998 named is unchanged by this pass**: it needs a
container/dependency-install/network story kopicode has twice now (ADR-0008,
ADR-0011) deliberately declined to build into its own binary. Cost, now
better-bounded at $1–$8 for the 50-task Mini rather than KAN-998's wider,
un-anchored guess, was never actually the hard part — the containment decision
is, and this scoping pass does not make that decision for kopicode (it would need
its own ADR: does kopicode build in containment despite ADR-0008, contra its own
reasoning, or does this become an operator-provisioned-runtime story the way
ADR-0011 pushed cuttlefish's own containment onto cuttlefish?).

**If the goal is "get a real external anchor now, cheaply, with no new isolation
commitment,"** Aider's polyglot benchmark — a stratified Go+Python subset — is
the better first move, and it is the genuinely new option this pass adds that
KAN-998 didn't have available (it was written without network access, so it
never evaluated benchmarks beyond the one it was asked about). It's directly
on-thesis for languages (Go and Python-stdlib, kopicode's two guaranteed
toolchains), fits inside the current trust model with zero new isolation work,
and costs less than the in-house corpus already does.

**Concrete recommendation: pursue Aider-polyglot as the next actionable card, and
leave SWE-bench-Verified-Mini exactly where ADR-0005 §6 and KAN-998 already left
it — deferred, pending a separate containment decision this scoping pass does not
make.** The two are not competing for the same investment: Aider-polyglot needs
`internal/corpus`-level integration work under kopicode's existing trust model;
SWE-bench needs a trust-model decision first, independent of anything decided
here.

## 6. Sources

[SWE-bench Verified | Epoch AI](https://epoch.ai/benchmarks/swe-bench-verified) ·
[How to run SWE-bench Verified in one hour | Epoch AI](https://epoch.ai/latest/swebench-docker) ·
[OpenAI: Introducing SWE-bench Verified](https://openai.com/index/introducing-swe-bench-verified/) ·
[SWE-bench FAQ](https://www.swebench.com/SWE-bench/faq/) ·
[Multi-SWE-bench (arXiv)](https://arxiv.org/pdf/2504.02605) ·
[Multi-SWE-bench GitHub](https://github.com/multi-swe-bench/multi-swe-bench) ·
[Aider-AI/polyglot-benchmark](https://github.com/Aider-AI/polyglot-benchmark) ·
[Aider polyglot leaderboard post](https://aider.chat/2024/12/21/polyglot.html) ·
[Aider benchmark README](https://github.com/Aider-AI/aider/blob/main/benchmark/README.md) ·
[Terminal-Bench 2.0 | Snorkel AI](https://snorkel.ai/leaderboard/terminal-bench-2-0/) ·
[BigCodeBench paper](https://arxiv.org/html/2406.15877v2) ·
[LiveCodeBench GitHub](https://github.com/livecodebench/livecodebench) ·
[Commit0 (arXiv)](https://arxiv.org/pdf/2412.01769) ·
[SWE-bench_Verified on Hugging Face](https://huggingface.co/datasets/SWE-bench/SWE-bench_Verified) ·
[Holistic Agent Leaderboard, SWE-bench cost figures (arXiv)](https://arxiv.org/pdf/2510.11977) ·
[RepoBench (arXiv)](https://arxiv.org/abs/2306.03091) ·
[SWE-bench-Pro leaderboard/pricing](https://www.morphllm.com/swe-bench-pro).

All figures above are order-of-magnitude estimates scaled from published numbers
plus this project's own one real measurement (KAN-800), not a live kopicode run
against any of these benchmarks. Treat them as bounds to reason with, not a
number to budget against without a real measurement.
