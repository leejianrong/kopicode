# Background

The reasoning and history behind kopicode that the [README](../README.md) leaves out so a
reader deciding whether to try it does not have to scroll past it. Nothing here is needed
to install or use kopicode. [`docs/adr/`](adr/README.md) holds the decisions themselves;
this is the story around them.

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
planning — slice 3, after the second model, since it expands the A/B matrix), and a
JSON-RPC surface for editors (slice 3, mostly plumbed by the headless runner already).
AST-structural editing stays an open question, blocked on tree-sitter's Go bindings
being CGo; the resolution sketch is stdlib `go/ast` for Go plus an optional external
`ast-grep` binary, per ADR-0006 §6.

## Reversed and amended decisions

Two ADRs reverse earlier plans in this repo, and several more amend earlier ones without
reversing them (one amends two prior ADRs at once). Worth being explicit about the
reversals and the main amendments.

**Satay is out.** The original bet was "a kopicode session is a journal" — replay
it, fork it at the turn where it went wrong. The hole in that: Satay forks by
replaying a prefix and *reusing recorded results*, so a fork at turn 7 replays the
`edit_file` calls as recorded return values rather than re-running them. **The
journal records what the agent decided and what it was told. It does not record the
repo.** A forked session gives you a rewound conversation over a working tree that
never moved, which is worse than not forking. For a coding agent the state that
matters is the filesystem, and the thing that versions filesystems is git. Details
and the replacement design in [ADR-0002](adr/0002-no-durable-runtime-own-journal.md).

**kopi-engine is folded in.** With Satay out and cuttlefish unstarted, a separate engine
repo was two CI setups and a version matrix for one unbuilt product. Go's
`internal/` gives the boundary for free. [ADR-0003](adr/0003-single-repo-internal-engine.md).

**Declared configs sit next to the built-in registry.** ADR-0007 said users don't
author a harness configuration. That held for the case it was written for: a
published benchmark result, which has to reproduce from the artifact alone. A
private one doesn't carry that requirement, so
[ADR-0010](adr/0010-declarative-harness-configs-and-self-tuning.md) adds a
second, declarative config class, TOML, base configuration plus field overrides, for
a model with no built-in entry, plus `kopitune`: a search loop over the fields the
harness already has. It's local-only by design. A declared config never anchors a
published number.

**The unattended case needed its own trust model, not a workaround.** ADR-0008
accepted that a consented shell command runs with the operator's full privilege, on
the premise that the operator and the person whose task is running are the same
trusted person. cuttlefish breaks that premise on purpose, so
[ADR-0011](adr/0011-unattended-invocation-policy-gate.md) adds a second mode: a
declared allowlist policy instead of a human answering, with real containment
supplied by whoever calls kopicode unattended, not by kopicode itself.

## Models, prices and the provider pin

**One binary serves every supported model.** There is no per-model build and no
build-time model constant; the model is selected at run time with `--model`, falling
back to `model = "…"` in `.kopicode/config.toml` and then to a built-in default, and an
unrecognised id is a startup usage error listing what is supported rather than a
provider error one request later. Each supported model resolves to a per-model harness
configuration held in the binary, and (model × harness configuration × provider pin) is
what defines a benchmark arm
([ADR-0007](adr/0007-model-selection-and-harness-config-shape.md)).

The **Role** column below is about what gets *measured*, and in what order — not about
what the binary can run.

Verified against OpenRouter on 2026-08-11. Prices are USD per million tokens,
input/output.

| Model | Price | Context | Role |
| --- | --- | --- | --- |
| `qwen/qwen3-coder-next` | 0.12 / 0.80 | 262K | **slice-1 target** |
| `minimax/minimax-m2` | 0.26 / 1.02 | 205K | A/B candidate |
| `z-ai/glm-5.2` | 0.56 / 1.76 | 1M | A/B candidate, long-context |
| a current frontier model | — | — | ceiling, run rarely |

The last row is a role rather than an entry: the frontier model is picked and added to
the registry when the ceiling run is actually scheduled, since naming one now would only
date the table.

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
([ADR-0005](adr/0005-benchmark-and-ab-methodology.md)).

Slice 1 pins `parasail/bf16` at `bf16` — the only one of the four endpoints serving
`qwen3-coder-next` that reports a full-precision quantization, and also the cheapest of
them, which is where the price in the table above comes from. Two of the other three
report their quantization as `unknown`, which is a legal filter value and a worthless
pin. [`docs/provider-pin.md`](provider-pin.md) has the observed endpoint list, the
date, and the re-check.

## What slice 1 meant

Done means both of these are true:

- **It builds things.** Point it at a real repo, give it a real small task, get a
  correct diff that passes the existing suite.
- **It benchmarks.** The headless runner executes a local task corpus against
  `qwen3-coder-next` with unit-test oracles, and against a mock provider at zero
  token cost for plumbing regressions.

One model measured, one harness configuration registered. No plugin system, no
second arm, no add-on catalogue.

That is a **scope limit on slice 1, not a property of the product**. The binary
selects its model at run time and resolves a harness configuration from it
([ADR-0007](adr/0007-model-selection-and-harness-config-shape.md)); slice 1 simply
registers one configuration and measures one model against it.

## Where this sits

```
kopicode (this repo)          cuttlefish (later, not started)
   terminal coding agent        always-on assistant
   interactive, supervised      unattended, triggered
                                  |
                                satay
                          durable execution, journal, replay, fork
```

cuttlefish is where durable execution earns its cost: unattended, trigger-driven,
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
from it.** [ADR-0002](adr/0002-no-durable-runtime-own-journal.md) holds that
line without Satay.

## Naming

`kopi` is coffee. `kopitiam` is **not** available, being the working name for
Satay's hosted plane in
[ADR-0026](https://github.com/leejianrong/satay-runtime/blob/main/docs/adr/0026-license-and-hosted-journal-plane.md).
