# ADR-0010: Declarative harness configurations, and an automated search loop over them

- **Status:** Accepted
- **Date:** 2026-08-23
- **Deciders:** Jian (leejianrong2@gmail.com)

Drafted by an agent during a 2026-08-23 portfolio-strategy session and accepted by
Jian the same day.

Amends [ADR-0007](0007-model-selection-and-harness-config-shape.md) decision 5's
sentence "Users do not author one" — and only that sentence. Everything else in
ADR-0007 (one binary, the registry, the hash preimage, what an arm is) stands
unchanged and this ADR does not re-argue it. Extends
[ADR-0005](0005-benchmark-and-ab-methodology.md)'s paired-comparison methodology by
giving it a second caller: a search loop, not only a human running `kopibench` by
hand.

## Context

**The stated ambition is broader than the built-in registry can ever cover.** kopicode
is meant to become the preferred harness for coding models generally, not only the
handful the maintainer hand-tunes and ships. ADR-0007's registry is a row per
model, added by a person, on purpose — "the row forces the question 'which harness
configuration does this model get?' to be answered by a person rather than defaulted
silently." That discipline is correct for the models kopicode officially supports and
should not change. It does not scale to a user's own fine-tune, a niche open model
nobody has hand-tuned, or a model that does not exist yet. Nobody is going to open a
pull request against this repository for a private fine-tune, and it would be the
wrong ask if they tried.

**ADR-0007 rejected user-authored configuration, for two reasons, and only one of
them is still live.** Decision 5's alternatives-rejected section gives two: the axes
are not known yet (ADR-0005 §7's deferral), and a config living outside the artifact
means "a published number could not be reproduced from the artifact alone."

The first reason does not actually block this ADR. ADR-0005 §7 defers **inventing new
fields** on `harness.Config` — a plugin/middleware catalogue built on axes nobody has
evidence for yet. This ADR proposes nothing of the kind. `harness.Config` already has
real, existing, non-speculative fields today: `AdvertiseNativeTools`, the fuzzy-match
floor, the turn cap, the parse-route order, the verification policy, the sampling
parameters. Those exist because slice 1 needed them, not because anyone guessed at an
add-on taxonomy. What this ADR proposes is a second **source** of a fully-specified
value for that **already-existing** struct shape — not a new field, not a new kind of
field, not a new extensibility mechanism. ADR-0007 decision 5 itself already drew
exactly this kind of distinction to justify settling one question while leaving
another deferred: "§7 defers *what the fields are*. It does not defer *what kind of
thing holds them*, and the two are separable." This ADR adds a third, equally
separable question ADR-0007 did not need to answer at the time: *who chooses values
for the fields that already exist, and where does the resulting value live.*

The second reason — reproducibility — is real, and this ADR does not dissolve it. It
narrows it to the case it actually protects: a **published, citable** benchmark
number. Nobody is citing a result from a private fine-tune as a comparable public
claim about the kopicode binary, so the concern does not transfer to that case
unmodified. Decision 3 below states precisely what stays true and what does not.

**Most of what a search loop needs already exists, built for a different reason.**
ADR-0005's paired McNemar comparison with sequential early stopping was built so a
*human* running an A/B could stop as soon as a result was decisive rather than
exhausting the corpus. That is exactly the primitive a search algorithm needs — "is
candidate B better than the current best, with the fewest evaluations" — and it does
not care whether a human or a loop asked the question. The mock/replay provider gives
a free correctness pass (ADR-0005 decision 3) before any real token is spent on a
candidate. `internal/corpus`'s loader already validates a task's shape (starting
tree, a prompt in the form a user would type, a two-directional argv oracle). None of
this needs to be rebuilt; it needs a second, automated caller.

## Decision

**1. Two classes of harness configuration: built-in and declared.**

- **Built-in** — unchanged from ADR-0007 decision 5. A named Go struct value compiled
  into the binary, in the registry, keyed by model id, hand-authored by a maintainer
  for officially-supported models.
- **Declared** — new. A value of the *same* `harness.Config` shape, expressed
  declaratively (a TOML document, resolved the same way `.kopicode/config.toml`
  already is) rather than compiled in. A declared config is authored by naming a
  built-in configuration as its base and overriding specific fields — the same shape
  `minimaxM2Config`'s own doc comment already uses in Go ("copying the base and
  changing exactly that field") — so authoring one does not require re-specifying
  every field kopicode has. The loader resolves base-plus-overrides into one fully
  concrete `Config` value before it is used for anything, so the "no map anywhere in
  `Config`'s type graph" invariant (ADR-0007 decision 6's hash-preimage discipline)
  is unaffected: a sparse TOML table is a parsing convenience, not a field on the
  resolved struct, and the value that gets hashed is exactly as fully determined as a
  built-in one.

**2. A declared config is opt-in and explicit, never a silent fallback for an unknown
model id.** ADR-0007 decision 4's refusal — an unrecognised model id is a startup
usage error, exit 2, before anything else happens — is unchanged for the ordinary
case. A declared config is reached only when the caller explicitly names one, by a
new `--harness-config <path>` flag or a `harness_config = "<path>"` key in
`.kopicode/config.toml`, at the same precedence tier as `--harness`/`harness =` in
ADR-0007 decision 2's table. Naming a declared config for a model id that already has
a built-in registry entry is allowed — it overrides the registry's choice for that
model, same as `--harness` does today for a registered configuration name — and
naming one for an unregistered model id is what actually unlocks using kopicode with
a model the built-in registry has never heard of. A typo in `--harness-config`'s path
is a usage error, same posture as everywhere else in this chain.

**3. What reproducibility means for a declared config, stated precisely.** Every
session, built-in or declared, still writes its harness config's full hash and value
into `SessionStarted` — no exception to the journaling discipline. A user comparing
their own declared-config results across their own runs, with their own kopicode
binary, has everything ADR-0007's provenance argument asks for: the file, the hash,
the journal. What a declared config **cannot** do is anchor a number this project
publishes as a comparable, citable result (ADR-0005 decision 6's SWE-bench-Verified
anchor and the project's own corpus results) — those stay built-in-only, because a
declared config is not part of the artifact a third party downloads and cannot be
reproduced by them without also obtaining the config file. Anywhere a tuning result
is reported, this distinction must be stated the same way ADR-0005 decision 8 already
requires stating what was pruned or dropped.

**4. `kopitune`: a third front end, not a mode of the other two.** A new
`cmd/kopitune`, following the same pattern ADR-0003 already established for adding a
front end (a `cmd/` binary driving the engine and the existing bench machinery through
their public interfaces, nothing new exposed from `internal/engine` or
`internal/bench` for it to reach into). Reusing `kopibench`'s own binary for this
would conflate two different jobs — "run one arm and report a score" versus "search a
space and emit a config" — the same category distinction ADR-0009 drew between
`internal/permission` and `ask`: different questions get different mechanisms, not one
mechanism stretched to answer both.

**5. The search space is a curated, versioned list — not "every field of
`Config`."** Near-term dimensions, chosen because they are already real fields with
no speculative axis behind them:

- `AdvertiseNativeTools` (bool) — native tool-calling versus text-route extraction.
- The fuzzy-match floor (a bounded float; ADR-0006 fixed it at 0.90 for the one
  built-in configuration that exists, but nothing says every model wants that exact
  value).
- Turn cap and repair-attempt budget (bounded ints).
- Parse-route preference ordering (a permutation over the existing fixed route set —
  native, fenced JSON, XML — not a new route).
- System-prompt **variant selection**: a categorical choice among a small,
  human-authored set of prompt variants (for instance verbose versus terse, explicit
  versus implicit anchor explanation), never free-text generation. Open-ended
  natural-language search is not a tractable dimension today — see Alternatives.

Adding a new dimension to this list later is a small, separate decision, exactly the
way `AdvertiseNativeTools` and the parse-route order already vary per built-in
configuration today without needing a fresh ADR each time (ADR-0009's closing note on
this makes the same point about tool membership). It is not, and must not become,
ADR-0005 §7's plugin catalogue reopened by another name — this list stays bounded to
fields `harness.Config` already has.

**6. A staged, cost-bounded search, reusing ADR-0005's machinery directly rather than
inventing a new evaluator.**

- **Stage 0 — free.** Every candidate runs against the mock/replay provider's
  plumbing suite. A structurally broken candidate (a schema the tool catalogue
  renderer rejects, a config that fails `internal/harness`'s own validation) is
  eliminated at zero token cost, before anything is billed.
- **Stage 1 — cheap.** Survivors run once each against a small smoke subset (2–3
  tasks) of the corpus, against the real model, to prune candidates that are
  obviously worse before spending a full paired comparison on them.
- **Stage 2 — real evaluation.** Paired McNemar with sequential early stopping
  (ADR-0005 decisions 1 and 4) between the current best candidate and each remaining
  challenger, over the full corpus or a user-supplied one (decision 7). This is
  successive-halving in shape: the full corpus is spent only on the final few
  contenders, not on every candidate that entered Stage 1.

**7. A caller may supply their own evaluation corpus.** The frozen, digest-pinned
corpus ADR-0005 decision 6 describes exists to keep the *official* corpus comparable
across published results — that pin was never meant to certify a private corpus, and
requiring one for a user's own domain-specific tasks (say, a finance-specific
fine-tune the frozen ten-task generic-coding corpus does not represent at all) would
make self-tuning useless for exactly the audience it is for. `internal/corpus`'s
loader gains an explicit unofficial mode that skips the digest check but keeps every
other validation — the starting-tree shape, the prompt shape, the both-directions
oracle check that already caught three malformed tasks in the frozen corpus. A result
produced against an unofficial corpus is marked as such wherever it is reported, per
decision 3.

**8. A hard cost governor, required, not optional.** `kopitune` refuses to start
Stage 2 without an explicit budget — a dollar ceiling or a token ceiling — set by the
caller. Spend is reported continuously as the search runs, and the loop refuses to
open a new paired comparison that would exceed what remains. This is the same
honesty discipline ADR-0005 decision 8 already requires of an ordinary bench run,
applied to a process that can otherwise spend money in an unattended loop with nobody
watching each request.

## Alternatives rejected

- **Free-text mutation of the system prompt as a search dimension.** Rejected for
  now. Prompt content is unstructured, and searching it open-endedly means either a
  second LLM acting as a mutator (its own reliability and cost problem, and one this
  ADR has no evidence is even net-beneficial) or a hand-rolled template-mutation
  scheme with no principled stopping point. A small, human-authored set of variants
  as a categorical dimension (decision 5) captures most of the value without either
  problem, and can be revisited once there is real usage data showing it is the
  binding constraint.
- **Exhaustive grid search over every dimension.** Rejected on the same grounds
  ADR-0005 already warned about for the add-on catalogue: "the combinatorial matrix
  multiplies cost by exactly the number of guesses." The staged, successive-halving
  approach in decision 6 is the direct fix, and it is the same fix ADR-0005 decision 4
  already chose for the human-driven case.
- **Letting a declared config anchor a published result.** Rejected in decision 3 —
  this is precisely the reproducibility failure mode ADR-0007 decision 5 named, and
  narrowing the rule rather than discarding it is what keeps that argument intact for
  the case it actually protects.
- **A general plugin/middleware system (Parsers, Context Engines, Middleware), as
  originally sketched before ADR-0005.** Still rejected, unchanged from ADR-0005 §7.
  This ADR is a value-search mechanism over an existing, fixed struct shape, not a new
  extensibility architecture, and must not be read as reopening that deferral by
  another name.
- **Shipping a declared config as Go source the user compiles into their own kopicode
  fork.** Rejected: reintroduces the per-model-build problem ADR-0007 decision 1
  already rejected for the built-in case, defeats "one binary," and is unusable by
  anyone without a Go toolchain — directly contrary to the audience this feature is
  for.
- **Reusing `kopibench` itself for the search loop**, adding a `--tune` mode.
  Rejected in decision 4: conflates "score one arm" with "search a space and emit a
  config," which are different enough questions that forcing them through one binary
  would blur the same line ADR-0009 drew between authorization and information-seeking
  for a different pair of concerns.

## Consequences

- `.kopicode/config.toml` gains a documented, additive shape (`harness_config = ...`,
  or `--harness-config` on the command line) for naming a declared configuration.
  Existing repositories using only `model =` / `harness =` are unaffected.
- Every session opened against a declared config still writes its full hash and value
  into `SessionStarted`; the journaling discipline has no carve-out for this class of
  configuration.
- `kopitune` is a real spender of tokens and money against real models the moment
  Stage 2 begins, and must never run without an explicit budget — this is a hard
  requirement, not a default that can be overridden by omission.
- Reports coming out of `kopitune`, or out of any comparison run against a declared
  configuration or an unofficial corpus, must say so plainly — the same "report what
  was dropped" discipline ADR-0005 already requires, extended to cover reproducibility
  class as well as coverage.
- This is a real amendment to one sentence of ADR-0007 decision 5. The rest of
  ADR-0007 — one binary, the registry, the hash preimage, what an arm is — is
  untouched, and this ADR should be read alongside it rather than as a replacement for
  any part of it.
- Adding a new tunable dimension to decision 5's list later does not require a new
  ADR, the same way `AdvertiseNativeTools` and the parse-route order already vary per
  built-in configuration without one. Adding a genuinely new *field* to
  `harness.Config` still does, per ADR-0005 §7, unchanged.
