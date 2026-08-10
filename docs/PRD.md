# kopicode PRD

What kopicode is for, who it serves, and how we will know it worked. Decisions live in
[`docs/adr/`](adr/) and implementation lives in [`SLICE-1.md`](SLICE-1.md); this document
cites both rather than restating them, so there is one place to change each fact.

Written 2026-08-11, before any product code existed.

## The problem

A coding agent's measured ability is not a property of the model alone. The harness
around it decides how tool calls are parsed, whether a failed edit costs a turn or gets
repaired, what the model is shown, and whether anything verifies the work. Published
claims put the swing at 10 to 25 percentage points on standard software-engineering
benchmarks, which is larger than the gap between adjacent model releases.

Open-weight models sit at the wrong end of that. They get the least harness attention,
so they carry the most unrealised capability, and the tooling built around them is
usually a thin port of something designed for a frontier model that fails in different
ways. A cheap model that emits a malformed tool call and then burns the turn looks
incapable. Often it is badly served.

Two things follow. Someone running open models wants a terminal agent that handles their
specific failure modes rather than shrugging at them. And nobody appears to be measuring
harness deltas per model in a way you could check, which means the claims above circulate
without evidence anyone can reproduce.

## Who it is for

**First, the author.** kopicode is a daily driver or it is nothing, and the honest bar for
that is whether it gets used for real work in preference to the alternatives. Dogfooding
is the primary signal, and it is available from the first slice.

**Second, developers running open-weight models**, whether through OpenRouter or locally,
who want the harness to compensate for a weaker model rather than assume a strong one.

**Third, the benchmark rig itself**, which is a real consumer with real requirements: it
cannot drive a REPL, it needs machine-readable output, and it needs every run to be
attributable after the fact. Treating it as a user from day one is what forces the
engine to have two front ends ([ADR-0003](adr/0003-single-repo-internal-engine.md)).

When the interactive user and the rig disagree, the interactive user wins. The rig exists
to make the product better, not the other way round.

## Requirements

Numbered so cards and tests can cite them.

### The agent has to work

- **R1.** Given a task in natural language, land a correct diff in a real repository
  without hand-holding, and know whether it worked by running that repository's own
  tests.
- **R2.** Survive weak-model tool calling. A malformed or unparseable call gets a specific
  error back and a bounded retry rather than costing the turn.
- **R3.** Never corrupt a file. An edit that cannot be placed with confidence is refused,
  not applied somewhere plausible ([ADR-0006](adr/0006-hash-anchored-edits-and-failure-attribution.md)).
- **R4.** Never silently claim success. A failing verification command blocks the success
  report.
- **R5.** Stay interruptible. Ctrl-C cancels the turn, kills any child process group, and
  leaves the session usable.
- **R6.** Leave the user's git state alone. Snapshots go to refs under `refs/kopicode/`,
  and the branch, HEAD, index and stashes are off limits.

### The record has to be trustworthy

- **R7.** One session record, typed, append-only, never truncated. Everything a user or a
  reviewer sees is derived from it, so it cannot drift from what happened
  ([ADR-0002](adr/0002-no-durable-runtime-own-journal.md)).
- **R8.** Reconstruct why the agent did something, from the record, after the fact:
  which parse route fired, which anchors an edit referenced, what the tool actually
  returned, what the human approved.
- **R9.** Credentials appear nowhere in the record, the blobs, the fixtures, or any log.

### The measurement has to be honest

- **R10.** Benchmark from the same engine the human drives, through a headless front end
  with machine-readable output and meaningful exit codes.
- **R11.** Attribute every failure mechanically, from journal events rather than judgement,
  into `harness`, `model`, or `unattributed`. A harness defect must never be counted as
  model incapacity, because that is the one error that corrupts the project's own thesis.
- **R12.** Make comparisons pairwise. Absolute pass rates on a small in-house corpus
  cannot resolve the effect being measured, so the unit of evidence is a flip on an
  identical task ([ADR-0005](adr/0005-benchmark-and-ab-methodology.md)).
- **R13.** Pin the provider and quantization on every recorded result, and discard a
  result whose pin does not match the declared one.
- **R14.** Test the harness at zero token cost. Plumbing regressions are the common case
  and must be catchable in CI without spending money.

### It has to be installable

- **R15.** One static binary, no runtime on the host, no dependency resolution at install
  time ([ADR-0001](adr/0001-go-implementation-language.md)).
- **R16.** Work as ordinary terminal software: scrollback intact, copy-paste working,
  output pipeable to a file with no escape sequences ([ADR-0004](adr/0004-line-oriented-repl.md)).
- **R17.** One binary serves every supported model, chosen at run time — a flag, then the
  repository's config file, then a built-in default, with no environment variable in the
  chain. The model id resolves to a per-model harness configuration, both are recorded on
  the session record, and together with the provider pin they identify the arm a result
  belongs to. An unrecognised model id is a startup usage error naming the supported set,
  not a provider error a request away
  ([ADR-0007](adr/0007-model-selection-and-harness-config-shape.md)).

R15 and R17 are separate promises and are easy to confuse. R15 is about *installation* —
the artifact needs nothing from the host. R17 is about *coverage* — one artifact serves
every model. Per-model binaries would satisfy R15 and violate R17.

## How we will know it worked

Success measures, in the order they can be checked.

1. **Zero harness-attributed failures** across a full corpus run. This is the slice-1
   bar, and note what it does not claim: the model's capability is measured, not
   asserted. If the model turns out to be the blocker, that is a finding.
2. **A full corpus run costs under two dollars** and the mock-provider suite costs
   nothing. Cheap measurement is what makes repeated measurement possible.
3. **The author reaches for it** on real work in the abang-ai repos, unprompted, at least
   some of the time. Soft, and still the measure that matters most.
4. **A paired delta with a number on it.** Once a second harness configuration exists,
   one A/B producing a McNemar result on an identical task set, with the pin, the task
   count and the stopping rule stated. This is the evidence the whole project is for, and
   it arrives in slice 2 rather than slice 1.
5. **Someone else installs it** with one command and gets a working agent against their
   own key.

Measure 4 is the one worth protecting. Requirements R11 through R14 exist entirely to
make it credible, and any shortcut there costs the project its point.

## Scope for v1

In: the agent loop, the six tools, hash-anchored editing with a fuzzy fallback, forced
verification, the typed journal, git turn snapshots, engine-side permissions, the REPL,
the headless runner, the mock and OpenRouter providers, a ten-task corpus, and mechanical
attribution. One model measured and one harness configuration registered — a scope limit
on what slice 1 exercises, not on what the binary can select, which is
[ADR-0007](adr/0007-model-selection-and-harness-config-shape.md)'s subject. The full
breakdown is [`SLICE-1.md`](SLICE-1.md).

Deferred, with a home: fork and resume over the snapshots already being written, a second
model measured and the plugin axes it earns, the `ask` tool, semantic model roles, a
JSON-RPC surface, container isolation for the bench runner, and context compaction chosen
from real session data rather than guessed.

**Not planned, and this list is as load-bearing as the one above.** LSP integration, DAP
debugging, browser or desktop control, voice, image tools, a memory system, in-process
coreutils, dozens of providers, IDE integration, AST-structural editing, and any hosted or
shared journal.

The reason is one sentence: every item on that list is orthogonal to whether a cheap open
model can be made to code reliably, and competing on tool surface against a
batteries-included agent is a fight this project would lose while abandoning its thesis.
See the prior-art section of the [README](../README.md).

## Constraints

- **Apache-2.0**, matching satay-runtime and sibei-flow.
- **Zero third-party dependencies** beyond `golang.org/x/term` and a test helper. Language
  tooling is shelled out to, never linked, which keeps the binary static and
  cross-compilable.
- **Linux and macOS first class, Windows best effort.** Process groups, advisory locking
  and shadow refs all differ there, matching satay-runtime's posture.
- **No telemetry.** A local CLI phoning home is a privacy decision, not a quality one.
- **The bench runner is not a security boundary in v1.** Model-authored shell runs in a
  worktree rather than a container, and that is stated in the docs rather than implied.

## Risks

Product-level. Implementation risks are in [`SLICE-1.md`](SLICE-1.md).

**The model may not be good enough, and the harness cannot fix that.** Mitigated by
separating the two in the acceptance criteria rather than hoping. A capability ceiling is
a publishable finding, not a failed project.

**The daily-driver bar is genuinely hard.** Pi and oh-my-pi are mature, MIT-licensed, and
one of them ships session trees we thought were our differentiator. Competing on breadth
is not the plan; being better at the specific thing (serving a weak model well, with
evidence) is.

**The in-house corpus will flatter us.** Ten tasks chosen by the person building the
harness is close to the definition of overfitting. Mitigated by freezing and versioning
the corpus, reporting what was excluded, and anchoring against a public benchmark subset
before quoting any number outside the project.

**One card carries the critical path.** Hash-anchored editing sits upstream of the loop,
both surfaces, and the entire rig. If the anchor mechanism turns out not to work with a
cheap model, the slice reshapes around it rather than continuing past it.

## Traceability

Which epic on board 21 serves which requirement. A requirement with no epic, or an epic
serving none, is a planning bug.

| Epic | Requirements |
| --- | --- |
| Journal and session record | R7, R8, R9 |
| Provider seam | R9, R13, R14 |
| Tool surface and call reliability | R2, R8 |
| Edits, verification, and repo state | R3, R4, R6 |
| Engine loop and permissions | R1, R5 |
| Surfaces | R10, R16, R17 |
| Benchmark rig | R11, R12, R13, R17 |
| Slice-1 proof | R1, R15 |

R15 is delivered by the toolchain rather than an epic: `make xbuild` proves it on every
CI run, and the slice-1 proof is where a human installs the result.

R17 sits in two epics because it is two halves of one promise: the flag, the config key
and the precedence between them are a surface concern, and the arm definition the
resolved values feed is a bench concern. Splitting it into two requirements would let one
half ship without the other, which is exactly the state ADR-0007 was written to end.

## Open questions

- Isolation model for the bench runner. Container per task is the obvious answer and its
  cost is unmeasured.
- Context management. Compaction, retrieval, or a file map, chosen once real sessions
  exist to measure rather than now.
- Whether cheap A/B by replaying an unchanged prefix is worth building, and whether it is
  enough to reopen the durable-runtime question ADR-0002 closed.
- Whether sotong ever happens, which is the only thing that would justify promoting the
  engine out of `internal/`.
