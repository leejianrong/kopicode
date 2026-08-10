# ADR-0007: One binary for every model, and what a harness configuration is

- **Status:** Accepted
- **Date:** 2026-08-11
- **Deciders:** Jian (leejianrong2@gmail.com)

Settles the shipped *shape* that [ADR-0005](0005-benchmark-and-ab-methodology.md) §7
left implicit while deferring the plugin *axes*. §7 is not reopened: what a harness
configuration contains is still invented after a second model has been made to work.
This ADR says what kind of thing it is, how it is selected, and what an arm is — none
of which §7 decides, and all of which the code already assumes.

Also extends ADR-0005 by defining the unit its paired method compares, and
[ADR-0006](0006-hash-anchored-edits-and-failure-attribution.md) §7 by saying what the
harness config hash it writes into actually hashes.

## Context

**One binary with the model chosen at run time is inferable from five places and
asserted in none.**

- [PRD](../PRD.md) **R15** says "one static binary" — but that requirement is about
  *installation*: no runtime on the host, no dependency resolution. Shipping four
  binaries, one per model, satisfies R15 exactly as well as shipping one. R15 does not
  reach model coverage and was never meant to.
- The [README](../../README.md) model table lists four models with roles: a slice-1
  target, two A/B candidates, and a frontier ceiling.
- ADR-0005's paired-McNemar method compares *arms*. If a model is a build-time choice,
  running one A/B means building and shipping two artifacts and trusting that they
  differ only where intended.
- `journal.SessionStarted` records a `model_id`. You only record what can vary.
- The project's strategy notes led with swappable model drivers and were reordered to
  lead with harness mechanisms. Swappable was deprioritised, not dropped.

Meanwhile [SLICE-1](../SLICE-1.md)'s "One model, one hardcoded harness" is the loudest
sentence in the docs and reads cold as a *product* decision. It is a slice scope limit.
Nothing says so.

**There is no documented way for a user to choose a model at all.** Not a flag, not a
config key, not an environment variable. The slice-1 build plan gets to step 18 without
one, which works only because the model is a constant.

**`SessionStarted.HarnessConfigHash` shipped this morning with a docstring — "identifies
the harness configuration, so two runs can be compared only when they were run the same
way" — and no definition of its domain.** Somebody has already decided that harness
configuration is a varying, hashable thing. ADR-0006 §7 then went further and obliged an
`anchor.Version` bump to be "included in `SessionStarted`'s harness config hash". A hash
whose preimage is undefined is not a comparison key; it is a field that will be filled
in by whoever implements the loop, from whatever was to hand.

**And ADR-0005 uses "arm" throughout without defining one.** §2 says a result whose pin
does not match the declared pin is discarded, which implies the pin is part of an arm's
identity. §7 implies the harness is. `SessionStarted` implies the model is. Nowhere are
the three put together, and "poolable" and "pairable" — the two questions the bench
runner has to answer per result — are not distinguished at all.

## Decision

**1. One binary serves every supported model, and the model is a run-time choice.**

There is no per-model build tag, no per-model binary, no build-time model constant.
`cmd/kopicode` and `cmd/kopibench` are each one artifact per platform, as `make xbuild`
already produces them.

The relationship to R15, stated so the two stop being confused: **R15 is about
installation, this is about coverage.** R15 says the artifact needs nothing from the
host. This says one artifact covers every model. They are orthogonal — per-model
binaries would satisfy R15 — and each has to be asserted on its own.

**2. Selection precedence: flag, then repo config, then the built-in default.** Highest
wins.

| | Source | Scope |
|---|---|---|
| 1 | `--model <id>` | this invocation |
| 2 | `model = "<id>"` in `.kopicode/config.toml` | this repository |
| 3 | the built-in default (`qwen/qwen3-coder-next`) | everything else |

`--harness <name>` and a `harness = "<name>"` key resolve the same way, and default to
whatever decision 4's registry maps the selected model to. Both flags exist on both
front ends, because setting the arm per invocation is the whole point of decision 1.

A flat key rather than a `[model]` table: SLICE-1 §5 already puts the verification
command in this file, and a flat key is the smaller addition.

**3. No environment variable in the chain.** Not `KOPICODE_MODEL`, not at any position.

The reasoning is provenance, not taste. `SessionStarted` records the model id so a
result is attributable after the fact, and the whole rig exists to make numbers
checkable. A flag is in the shell history and the CI job log; a config key is in the
repository and shows up in a diff. An environment variable is ambient, invisible in both
places, and inherits from whatever set it — which is the standard mechanism by which
"I ran the same command and got a different number" happens.

The asymmetry with `OPENROUTER_API_KEY` is deliberate and points the other way: a
credential *must* live in the environment precisely because it must not be written to a
file anyone can commit. The model id is the opposite kind of fact. It must be written
down.

Reopening this needs an amendment, not a patch — if a CI matrix turns out to need it,
the fix is `--model` in the matrix, which is visible in the job log.

**4. An unrecognised model id is a startup usage error — exit 2 — and never a provider
error.**

kopicode resolves the model against the built-in registry before anything else happens:
before the system prompt is assembled, before the journal is opened, before
`SessionStarted` is written. The error names the requested id, lists the supported set,
and offers near matches.

Three reasons, in order of weight:

- **There is no harness configuration for an unknown model.** Decision 5 resolves the
  harness from the model id. An unrecognised id means the harness itself is undefined,
  which is a configuration failure wearing a network failure's clothes. Deferring it to
  the provider misreports the category.
- **The provider path costs a turn and some money**, and reports late. The request is
  assembled, billed if it is billable, and answered with a 404 the loop has to
  interpret. In a bench run this arrives *after* N tasks have already run, and lands as
  a provider error inside a corpus rather than a refusal before it.
- **Model ids are typo-shaped.** `qwen/qwen3-coder-nxt` should produce a list of what is
  available, not a stack of retries.

Exit 2 is the usage-error code SLICE-1 step 14 already documents, and failing before
`SessionStarted` means no half-session record exists for a session that never started.

**5. Per-model harness configuration is a built-in registry keyed by model id,
overridable by name. Users do not author one.**

- A **harness configuration** is a named, versioned value compiled into the binary — a
  Go struct with a name, not a file the user writes.
- A **registry** maps each supported model id to the harness configuration it gets by
  default, and to that model's default provider pin.
- Resolution is: model id → registry entry → harness configuration name → the value.
  `--harness` overrides the last arrow and can select any *registered* configuration.
- **Slice 1 registers exactly one configuration, `default`.** So `--harness` has one
  legal value and is, in practice, a no-op. That is the scope limit. The shape is not.

Normatively:

```
registry : model-id → (harness-config-name, default-provider-pin)
resolve(model, harness_override) :=
    entry   := registry[model]           // absent → usage error, decision 4
    name    := harness_override ?? entry.harness
    config  := configs[name]             // absent → usage error
```

Why this shape rather than the one slice 1 builds: **a paired A/B over the harness axis
must be two invocations of one binary, not two builds.** If tuning is done by editing
code, ADR-0005's arms are two artifacts, and the only thing distinguishing them is a
claim about what was edited. The harness config hash on `SessionStarted` then hashes a
constant and the field is dead weight. The registry is the smallest structure that makes
the hash mean something and the flag possible.

Why not the third candidate — user-authored configuration — is ADR-0005 §7's answer and
stands unchanged: the axes get invented from evidence about what actually differed
between two models, and there is no second model yet. Note that §7 defers *what the
fields are*. It does not defer *what kind of thing holds them*, and the two are
separable — which is why this ADR can settle one without touching the other.

Adding a model is a registry row and a pull request. That is deliberate: the row forces
the question "which harness configuration does this model get?" to be answered by a
person rather than defaulted silently.

**6. The harness config hash covers everything that changes the model-facing contract or
the loop's behaviour without changing the model, the pin, or the code.**

In the preimage:

- the harness configuration's name and every field value it carries
- `anchor.Version` — required by ADR-0006 §7, and see the consequence below
- the system prompt, by digest
- the tool set as presented to the model: names, schemas, descriptions
- the parse route order and the repair budget (SLICE-1 §3)
- the max-turns cap and the token budget
- the verification policy — whether it is forced, and whether the command was configured
  or discovered
- the **sampling parameters**: temperature, top-p, max tokens, and the seed *policy*

Not in the preimage:

- the **model id**. It is a separate axis. Hashing it in would make the hash useless for
  the one question it exists to answer — "same harness, different model?"
- the **provider pin**. Same reason.
- a **per-run seed value**. The policy (fixed, or unseeded) is a harness property; the
  value drawn for one run is not, and hashing it would give every run its own arm.
- the **corpus version**. That belongs to the experiment, not the arm — decision 7.

Sampling belongs inside the harness configuration rather than beside it because choosing
temperature 0.2 for one model and 0.7 for another *is* per-model tuning, and because a
fourth axis that never varies independently of the third buys nothing. The `Sampling`
struct already recorded on every `ProviderRequest` is then the per-request *receipt* —
it lets a reader check that the requests matched the declared configuration, exactly the
relationship `ProviderResponse.ServedBy` has to the declared `ProviderPin`.

**7. A bench arm is (model id × harness configuration × provider pin), and poolable is
not the same question as pairable.**

- **Poolable** — two results may be aggregated into one arm's score — iff they carry the
  same `model_id`, the same `harness_config_hash`, and a `ServedBy` inside the declared
  pin. A result whose `ServedBy` falls outside it is **discarded, not adjusted**
  (ADR-0005 §2), and the discard is reported.
- **Pairable** — two results may form a McNemar discordant pair — iff they are the same
  task at the same corpus version, from two arms, inside one experiment series.
- **An experiment series** is bounded by a corpus version bump and by an
  `anchor.Version` bump. Results either side of a boundary may be neither pooled nor
  paired.

The pin is in the tuple not because anyone A/Bs it deliberately, but because it changes
*involuntarily* — a pinned provider disappearing mid-series is already named as a
consequence of ADR-0005 — and a change in it must invalidate pooling rather than pass
unnoticed.

Two arms differing in **more than one** axis may still be compared; the result attributes
to the combination and to neither axis alone, and the report must state which axes
differ. "Each model on its own tuned harness" is a legitimate and interesting comparison
that is not a clean attribution, and pretending otherwise is how a harness gain gets
claimed from a model swap.

One nice property falls out of decision 6 rather than needing to be remembered: because
`anchor.Version` is inside the harness config hash, an anchor-format bump changes every
arm's hash automatically. ADR-0006 §7's series boundary is therefore **mechanically
enforced** — a stale result cannot be pooled with a fresh one, because their hashes
differ. That is the reason §7's instruction was worth following, and it is stated here so
the coupling is not deleted by someone tidying the preimage.

### Alternatives rejected

**On the binary.**

- **One binary per model, or a build tag per model.** Makes an A/B two artifacts, so the
  claim that the arms differed only in the intended way rests on the build rather than on
  a recorded hash. Also makes `SessionStarted.model_id` a constant, which is a field
  recording something that cannot vary. Rejected on both.
- **Model chosen at build time, selection deferred to "when there is a second model."**
  This is the status quo and it is the trap: the second model is exactly when the
  selection mechanism is under time pressure and gets bolted on. The cost of deciding now
  is one ADR; the cost of deciding then is a mechanism designed while blocked.

**On selection.**

- **`KOPICODE_MODEL` in the chain.** Rejected in decision 3 on provenance. Named
  explicitly because it is the obvious thing to add and adding it later is a one-line
  change nobody would think to argue about.
- **Config file beating the flag.** A per-invocation override a checked-in file can veto
  is not an override, and the bench runner cannot then set the arm per run without
  rewriting the user's repository.
- **Short aliases** — `--model qwen`. An alias is a second name for something whose
  canonical name is already the thing recorded in the journal and quoted in a report. A
  report saying `qwen` is not reproducible in eighteen months. Provider ids only.
- **Deferring the unknown-id check to the provider.** Rejected in decision 4: wrong
  category, costs a turn, reports late, and in a bench run reports late *inside a
  corpus*.
- **An open registry — an unknown id silently gets the `default` harness.** The failure is
  that a typo becomes a plausible run rather than a refusal, and that a model runs on a
  configuration nobody chose for it while the report attributes the number to the model.
  It also removes the only forcing function for the "which harness does this get?"
  question. If an escape hatch is ever wanted, it is `--model X --harness Y` with both
  named explicitly, which is a slice-2 extension and costs nothing to defer: slice 1 has
  one harness configuration, so an unregistered model would necessarily receive it
  anyway, unremarked — which is precisely what the registry exists to prevent.
- **Resolving the harness from a model *family* by string prefix** (`qwen/*`). A silent
  default path with a matching rule, which is the same defect with more machinery.

**On the harness configuration.**

- **One configuration for everything, tuning by editing code.** This is what slice 1
  *builds* and it is rejected as the shipped *shape*, per decision 5: it makes the
  harness axis a build axis and the hash a constant.
- **Per-model configuration in the user's `.kopicode/config.toml`.** This is the
  user-authored shape under another name — ADR-0005 §7 — and it has an extra defect the
  in-binary registry does not: the configuration that defines an arm would live in the
  user's repository rather than in the artifact, so a published number could not be
  reproduced from the artifact alone.

**On the arm.**

- **(model × harness) with the pin as metadata.** Contradicts ADR-0005 §2 directly: if
  the pin is metadata, a mismatched result is annotated rather than discarded.
- **Sampling as a fourth axis.** Rejected in decision 6 — it does not vary independently
  of the harness, and a four-tuple where one member is a function of another is a
  three-tuple with extra steps.
- **Corpus version inside the arm.** Would make every corpus bump look like a new *arm*
  rather than a new *series*, so the same harness would appear to have been measured
  twice with different identities. The corpus is a property of the experiment and both
  arms of a pair share it by construction.

## Consequences

- **`--harness` ships with one legal value.** A flag with a single option looks like
  scaffolding, and reviewers will be tempted to delete it. It is load-bearing: it is what
  makes slice 2's first paired A/B two invocations instead of two builds.
- **The registry is a maintenance surface.** Every new model is a row, and a row that
  claims a harness configuration the model has not actually been run against is a claim
  the registry cannot check. Rows should carry the date the mapping was last validated.
- **The harness config hash cannot detect a code change** that alters loop behaviour
  without touching a configuration field. Two arms with identical hashes from different
  builds are not necessarily comparable, and nothing currently catches that:
  `SessionStarted` carries no build version, and the envelope's `schema_version` is about
  the journal format, not the binary. A build identifier on the session record is the
  obvious fix and is left as a separate card rather than smuggled in here.
- **The system prompt is now inside a hash**, which means editing it is an
  experiment-series-adjacent act. That is correct — a prompt change is a harness change,
  and it is the single most likely thing to be edited casually mid-series.
- **Where the resolution came from is `slog`, not the journal.** The journal records the
  *resolved* model, pin and hash, because the resolved values are what identify the arm.
  Which of flag, config or default supplied them is engine-internal diagnostics —
  "config resolution" is already inside `slog`'s scope in `CLAUDE.md` — and putting it in
  the journal would be session content that isn't.
- **Adding a model no longer implies adding a driver.** "A second model driver" in the
  older docs described a build-time artifact that this ADR removes; the docs that say so
  have been corrected, since a document contradicting an ADR loses.
- **This does not make kopicode multi-provider.** OpenRouter plus the mock/replay
  provider remains the whole set, and "dozens of providers" is still an explicit non-goal
  in the README. One binary for every supported *model* is a much smaller promise than
  one binary for every provider, and the registry does not become a provider abstraction.
