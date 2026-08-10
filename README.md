# kopicode

A coding agent for the terminal, and a harness you can tune per model.

Nothing is built yet. This README records the decisions already taken, so the
first commit of real code starts from them rather than re-deriving them. The
reasoning behind each one is in [`docs/adr/`](docs/adr/).

## The thesis

A model's harness — prompt structure, tool surface, parse-and-repair behaviour,
context handling, verification loop — moves its measured coding ability by a large
margin. Published claims put the swing at 10–25 percentage points on standard
software-engineering benchmarks.

Open-weight models are the ones with the most headroom, because they get the least
harness attention. So kopicode is two things at once, deliberately:

1. **A terminal coding agent good enough to build with daily.** That is the product,
   and the thing it is judged on.
2. **A harness that can be tuned per model, with a benchmark rig to prove the tuning
   worked.** Benchmarks are a regression gate and an experiment platform, not the
   deliverable.

Both matter from the first slice, because they constrain the architecture in the
same direction: one engine, two front ends — an interactive REPL and a headless
runner. A benchmark cannot drive a REPL.

## Where the harness gains actually are

Ordered by expected payoff, which is also the build order:

1. **Tool-call parse-and-repair.** Weak models emit malformed JSON, invented tool
   names, and wrong parameter shapes. Accepting several formats and feeding a
   specific error back for a retry — instead of burning the turn — is the single
   biggest lever.
2. **An edit tool where the model never reproduces file content.** `read_file`
   returns per-line content anchors; `edit_file` references them and **rejects on
   drift** rather than applying somewhere plausible. Exact-match replacement fails
   constantly on models that paraphrase, and fuzzy matching fails *open* — it can
   land above the similarity floor in the wrong place, silently. Anchors sidestep
   both instead of tolerating either
   ([ADR-0006](docs/adr/0006-hash-anchored-edits-and-failure-attribution.md), a
   design adopted from oh-my-pi).
3. **Forced verification.** The loop runs tests and lint after edits and will not
   report success without them.

Note what these have in common: none of them are model-*specific*. They are generic
reliability work that disproportionately helps weak models. The genuinely
model-specific layer — prompt phrasing, XML versus JSON preference, thinking-block
handling — sits on top and is thinner. Build the thick layer first.

There is no plugin catalogue and no add-on taxonomy yet. The axes get invented after
two models have been made to work, not before ([ADR-0005](docs/adr/0005-benchmark-and-ab-methodology.md)).

## Prior art, and what we are deliberately not building

Surveyed 2026-08-11. Two projects define the space kopicode sits in, and both are MIT:

- **[Pi](https://pi.dev/)** (Earendil Inc. / Mario Zechner, TypeScript) — a
  deliberately minimal harness with lazy-loaded skills and extensions, four surfaces
  (TUI, print/JSON, RPC, SDK), and **session trees you can navigate back into and
  continue from**.
- **[oh-my-pi](https://github.com/can1357/oh-my-pi)** (Can Bölük, Rust core + Node
  CLI) — the batteries-included fork: ~160k LoC, 31 tools, LSP wired into every write,
  a DAP debugger, browser and desktop control, subagents in parallel worktrees, 60+
  providers, and the **hashline** edit format kopicode's ADR-0006 adopts.

Two consequences worth stating plainly.

**Session trees are not a differentiator.** Pi ships navigate-to-any-previous-point
already. kopicode's git-backed version restores the *tree* as well as the
conversation, which is the half Pi doesn't do, but that is a refinement of a shipped
feature and not a wedge. ADR-0002 had already stopped leaning on it.

**Competing on tool surface is unwinnable and off-thesis.** Explicit non-goals, not
"later": LSP integration, DAP debugging, browser or desktop control, voice and TTS,
image generation, a memory system, in-process coreutils, dozens of providers, and
IDE integration. Every one is orthogonal to whether a cheap open model can be made to
code reliably — which is the only question this project is trying to answer.

What *is* on the roadmap, borrowed deliberately: the `ask` tool (agent asks on genuine
ambiguity, slice 2), semantic model roles (cheap model for grunt work, strong for
planning — slice 3, after the second driver, since it expands the A/B matrix), and a
JSON-RPC surface for editors (slice 3, mostly plumbed by the headless runner already).
AST-structural editing stays an open question, blocked on tree-sitter's Go bindings
being CGo; the resolution sketch is stdlib `go/ast` for Go plus an optional external
`ast-grep` binary, per ADR-0006 §6.

## Decisions of record

| Decision | ADR |
| --- | --- |
| Go, not Python or TypeScript | [0001](docs/adr/0001-go-implementation-language.md) |
| No durable-execution runtime; own event log + git shadow refs | [0002](docs/adr/0002-no-durable-runtime-own-journal.md) |
| One repo; engine under `internal/`, not a separate library | [0003](docs/adr/0003-single-repo-internal-engine.md) |
| Line-oriented REPL, no full-screen TUI | [0004](docs/adr/0004-line-oriented-repl.md) |
| Paired A/B methodology, pinned providers, mock provider | [0005](docs/adr/0005-benchmark-and-ab-methodology.md) |
| Hash-anchored edits; three-bucket failure attribution | [0006](docs/adr/0006-hash-anchored-edits-and-failure-attribution.md) |

Two of these reverse earlier plans in this repo, and it is worth being explicit
about why:

**Satay is out.** The original bet was "a kopicode session is a journal" — replay
it, fork it at the turn where it went wrong. The hole in that: Satay forks by
replaying a prefix and *reusing recorded results*, so a fork at turn 7 replays the
`edit_file` calls as recorded return values rather than re-running them. **The
journal records what the agent decided and what it was told. It does not record the
repo.** A forked session gives you a rewound conversation over a working tree that
never moved, which is worse than not forking. For a coding agent the state that
matters is the filesystem, and the thing that versions filesystems is git. Details
and the replacement design in [ADR-0002](docs/adr/0002-no-durable-runtime-own-journal.md).

**kopi-engine is folded in.** With Satay out and sotong unstarted, a separate engine
repo was two CI setups and a version matrix for one unbuilt product. Go's
`internal/` gives the boundary for free. [ADR-0003](docs/adr/0003-single-repo-internal-engine.md).

## Layout

```
cmd/kopicode/        REPL surface
cmd/kopibench/       headless bench runner
internal/
  engine/            agent loop, turn state, context assembly
  provider/          OpenRouter client, mock/replay provider
  parse/             tool-call extraction + repair
  tools/             read, write, edit, list, grep, shell
  journal/           append-only tagged-union event log
  repo/              git shadow refs, worktrees, diff rendering
  permission/        policy decisions (what needs asking, not how to ask)
  bench/             runner, oracle execution, McNemar scoring
bench/tasks/         the frozen task corpus (data, not code)
docs/adr/            decisions of record
docs/SLICE-1.md      the current slice
```

`internal/` is the enforced boundary: nothing outside this module can import the
engine, so the split survives without a lint rule. The surface layer talks to the
engine through its interface only, which an import-hygiene test checks.

## Models

Verified against OpenRouter on 2026-08-11. Prices are USD per million tokens,
input/output.

| Model | Price | Context | Role |
| --- | --- | --- | --- |
| `qwen/qwen3-coder-next` | 0.12 / 0.80 | 262K | **slice-1 target** |
| `minimax/minimax-m2` | 0.26 / 1.02 | 205K | A/B candidate |
| `z-ai/glm-5.2` | 0.56 / 1.76 | 1M | A/B candidate, long-context |
| a current frontier model | — | — | ceiling, run rarely |

`qwen3-coder-next` is the slice-1 target: cheapest of the credible coding models,
coding-specialised, 80B total with 3B activated, and it runs in non-thinking mode
with no `<think>` blocks — one less parsing problem while the loop is being built.

Leaderboard aggregators disagree with each other on the current top of the open
field (Kimi K3, DeepSeek V4, Qwen 3.6, GLM 5.x all get named), so treat any
SWE-bench number quoted second-hand as unverified. The rig exists to measure this
ourselves.

**Pin the provider.** OpenRouter load-balances across providers by default and they
differ in quantization, which silently invalidates any A/B result. Every benchmark
request sets `provider.order`, `allow_fallbacks: false`, and `quantizations`
([ADR-0005](docs/adr/0005-benchmark-and-ab-methodology.md)).

## Slice 1

Done means both of these are true:

- **It builds things.** Point it at a real repo, give it a real small task, get a
  correct diff that passes the existing suite.
- **It benchmarks.** The headless runner executes a local task corpus against
  `qwen3-coder-next` with unit-test oracles, and against a mock provider at zero
  token cost for plumbing regressions.

One model. One hardcoded harness. No plugin system, no second driver, no add-on
catalogue.

## Where this sits

```
kopicode (this repo)          sotong (later, not started)
   terminal coding agent        always-on assistant
   interactive, supervised      unattended, triggered
                                  |
                                satay
                          durable execution, journal, replay, fork
```

sotong is where durable execution earns its cost: unattended, trigger-driven,
long-running, holding credentials, with no human watching to hit Ctrl-C. Replay
from the top is designed for exactly that workload. kopicode is the opposite case
and pays the determinism tax for benefits it does not collect.

They stay two products for the reason that was always the right one: something
acting while nobody watches needs a stricter safety model than something you are
supervising. A coding agent's worst case is a bad diff, caught in review, reversible
with `git revert`. An always-on assistant with long-lived credentials has no
equivalent rollback. That is an architecture, not a config flag.

## The lesson worth not relearning

sibei-flow hand-rolled a transcript alongside its agent loop: a `list[str]` built by
appending as the loop ran, with tool output clipped at 1200 characters. Lossy by
construction — the diagnostic output justifying a fix could be truncated exactly
where a reviewer looks — and free to drift, since adding a tool call and forgetting
the append produced a record of a run that never happened. Fixing it took a
dedicated PR and had to land before a launch to avoid becoming a migration. See
sibei-flow [ADR-0013](https://github.com/leejianrong/sibei-flow/blob/main/docs/design/adr/0013-transcript-tagged-union.md).

The lesson is not "use a durable runtime." It is: **one session record, typed as a
tagged union, never truncated, and everything a user or reviewer sees is derived
from it.** [ADR-0002](docs/adr/0002-no-durable-runtime-own-journal.md) holds that
line without Satay.

## Naming

`kopi` is coffee. `kopitiam` is **not** available, being the working name for
Satay's hosted plane in
[ADR-0026](https://github.com/leejianrong/satay-runtime/blob/main/docs/adr/0026-license-and-hosted-journal-plane.md).

## Licence

Apache-2.0, matching Satay and sibei-flow.

## Status

Planned. See the [Abang page](https://leejianrong.github.io/abang-landing-page/) for
where this sits among the rest.
