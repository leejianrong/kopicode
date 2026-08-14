# The provider pin

- **Observed:** 2026-08-14 (unauthenticated OpenRouter API)
- **Card:** KAN-775
- **Rule it satisfies:** [ADR-0005 §2](adr/0005-benchmark-and-ab-methodology.md),
  [SLICE-1 §7](SLICE-1.md), PRD R13

ADR-0005 §2 says *that* every benchmark request pins a provider. This file says *which*,
and shows the traffic the choice was made from. The two are separate on purpose: the rule
is a decision of record and does not change, while the value is operational and will
change the first time a provider retires an endpoint. Editing an ADR every time a
quantization moves is how an ADR stops meaning anything.

## The pin

```json
{
  "model": "qwen/qwen3-coder-next",
  "provider": {
    "order": ["parasail/bf16"],
    "allow_fallbacks": false,
    "quantizations": ["bf16"]
  }
}
```

Every benchmark request carries exactly this block, and the resolved pin is journaled on
`ProviderRequest`. A result served outside it is **discarded, not adjusted**.

**Where it lives in code.** On the model's row in `internal/harness`'s registry, which is
where ADR-0007 decision 5 puts a model's default pin — an arm being (model × harness
configuration × provider pin). The shipped fixtures carry their own copy, because a
fixture records the arm its traffic was taken under rather than pointing at whatever the
binary currently prefers, and `TestRegistryPinMatchesShippedFixtures` holds the two
together. That test is not cosmetic: the replay provider refuses traffic recorded under
another arm's pin, so a registry that drifted from the fixtures would fail every
mock-driven engine test, much later and looking like a mock bug. Change the value here
first, with the date and the argument; the row and the fixtures follow.

## Two names, and they are not interchangeable

The request and the response name the provider in different namespaces, and conflating
them is how a discard check quietly passes everything.

| | Value | Where it appears |
| --- | --- | --- |
| Request slug | `parasail/bf16` | `provider.order` — OpenRouter's provider-routing slug, endpoint variant included |
| Response name | `Parasail` | the completion's top-level `provider` field, which `journal.ProviderResponse.ServedBy` records |

OpenRouter's provider-routing documentation is explicit that a **base** slug matches every
endpoint a provider hosts, and that a suffixed slug (`deepinfra/turbo`,
`google-vertex/us-east5`) matches one. `parasail/bf16` is the suffixed form, taken
verbatim from the `tag` field of the endpoints response below, so the pin names one
endpoint rather than "whatever Parasail is serving this week".

The response's `provider` field is a display name and its casing is **not** documented —
OpenRouter's own examples show both `"OpenAI"` and `"openai"`. Compare it
case-insensitively, never require it to be present, and see the note on `completion.Provider`
in [`internal/provider/fixture/wire.go`](../internal/provider/fixture/wire.go).

## What was observed

Two unauthenticated endpoints answer this, and no API key was sent to either:

```bash
curl -sS https://openrouter.ai/api/v1/models | \
  python3 -c 'import json,sys; print("qwen/qwen3-coder-next" in [m["id"] for m in json.load(sys.stdin)["data"]])'

curl -sS https://openrouter.ai/api/v1/models/qwen/qwen3-coder-next/endpoints
```

`qwen/qwen3-coder-next` **is** in the catalogue under exactly that slug — it was checked
rather than assumed, because the near-miss neighbours (`qwen/qwen3-coder`,
`qwen/qwen3-coder-flash`, `qwen/qwen3-coder-plus`, `qwen/qwen3-coder-30b-a3b-instruct`)
are all real models and any of them would have been a plausible thing to pin by accident.

Four providers served it on 2026-08-14. Prices are USD per million tokens; `uptime` is
OpenRouter's own 24-hour figure at the time of the look, and moves.

| Provider | `tag` | `quantization` | Context | Max output | Prompt | Completion | `seed`? | Uptime 1d |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| **Parasail** | `parasail/bf16` | **`bf16`** | 262,144 | 262,144 | $0.12 | $0.80 | yes | 99.99% |
| StreamLake | `streamlake` | `unknown` | 256,000 | 64,000 | $0.18 | $0.90 | **no** | 99.58% |
| Novita | `novita/fp8` | `fp8` | 262,144 | 65,536 | $0.20 | $1.50 | yes | 99.80% |
| Alibaba | `alibaba` | `unknown` | 262,144 | 65,536 | $0.30 tiered | $1.50 tiered | yes | 99.63% |

All four report `tools` and `tool_choice` in `supported_parameters`, so tool calling was
not a discriminator.

## Why Parasail, `bf16`

**It is the only endpoint that reports a full-precision quantization.** That is the
argument that decides it. ADR-0005 exists because "two runs of the same model can be
different weights"; `bf16` is the weights the model was released at, so a harness delta
measured on it is not confounded by a quantization the project did not choose. `fp8`
(Novita) is a real, declared, reproducible choice too — it is simply a *different* model
for the purposes of this experiment, and the cheaper one is also the more faithful one, so
there is nothing to trade off.

**Two of the four report `unknown`.** StreamLake and Alibaba do not tell OpenRouter what
they serve. Pinning either would mean declaring `quantizations: ["unknown"]`, which is a
legal filter value and a worthless pin: it fixes the *label* while leaving the weights
free to change underneath it. An arm whose declared quantization cannot be checked is an
arm whose results cannot be defended.

**It is also the cheapest, on both halves of the price.** $0.12/$0.80 against $0.18/$0.90,
$0.20/$1.50 and $0.30/$1.50 — and Alibaba's tiers step up to $0.80/$4.00 above 128K
prompt tokens, which is precisely the region a long agent loop lives in.

**It supports `seed`.** StreamLake does not. A paired McNemar design (ADR-0005 §1) wants
every source of run-to-run variance it can remove, and sampling temperature is one the
harness controls only if the endpoint honours a seed. It is not a guarantee of
determinism — no hosted endpoint offers one — but the endpoint that does not accept the
parameter cannot even try.

**Its `max_completion_tokens` is the full 262,144.** The other three cap output at 64K or
65,536. Nothing in slice 1 needs a 262K reply, so this is a tiebreaker rather than a
reason, but it is one less ceiling to discover mid-run.

Uptime was not used as a criterion. All four sit above 99.5% and the figure moves between
one fetch and the next; treating a 0.4-point difference as evidence would be reading noise.

## The quantization is reported, and this is worth being explicit about

The risk this decision most invites is a **fabricated** quantization: a value that reads
plausibly, matches nothing the provider serves, and quietly makes every result
discardable under ADR-0005 §2 without anyone noticing, because the check compares the
declared pin against itself.

That did not happen here. `bf16` is the literal value in Parasail's endpoint record, and
it is one of the twelve values OpenRouter documents for the `quantizations` filter:
`int4`, `int8`, `fp4`, `mxfp4`, `nvfp4`, `fp6`, `fp8`, `mxfp8`, `fp16`, `bf16`, `fp32`,
`unknown`. `internal/provider/fixture` now holds that vocabulary and **refuses** a pin
naming anything outside it, so an invented quantization fails a test rather than a
benchmark.

Had the chosen endpoint reported no quantization, the honest pin would have been
`quantizations: ["unknown"]` with that fact stated here in as many words — not a guess at
what the provider probably runs. It did not come to that, which is itself part of why
Parasail was chosen.

## Cost, as of 2026-08-14

The prices above are **unchanged** from the ones README.md recorded on 2026-08-11
($0.12/$0.80), so the "roughly $2 per full corpus run" figure in CLAUDE.md still rests on
the same numbers it was written against. It is not a measurement — the bench runner does
not exist yet — and it will not become one until a real run reports usage.

The arithmetic it implies, for whoever checks it later: at $0.12/M prompt and $0.80/M
completion, $2 buys roughly 16M prompt tokens, or about 1.6M per task across the 10-task
corpus. That is a long loop with a large re-sent context on every turn, which is the shape
an agent run has, but it is an assumption about turn count and context growth rather than
an observation. Treat $2 as an order of magnitude with a plausible upper bias, and replace
it with the first real `bench` run's reported usage.

Prefix caching is available on this endpoint at $0.07/M read but is **not** implicit
(`supports_implicit_caching: false`), so nothing is cached unless the client asks. ADR-0005
§4 already warns against designing the experiment around caching; note only that the lever
exists.

## Re-checking this, and what would invalidate it

Re-run the two `curl` commands above. The pin still holds if, and only if, all of these
are true:

1. `qwen/qwen3-coder-next` is still in the catalogue under that exact slug.
2. An endpoint with `tag: "parasail/bf16"` still serves it.
3. That endpoint still reports `quantization: "bf16"`.

If (2) or (3) has changed, the pin is **not** silently updated to whatever replaced it.
Results gathered under the old pin and the new one are two experiments, and ADR-0005 §2's
discard rule is what keeps them apart. Change the value here, date the change, and start a
new experiment series.

Nothing in this file has been exercised against a live completion request. `provider.order`
accepting the suffixed slug form is from OpenRouter's documentation, and the response's
`provider` field is unverified in shape (see `wire.go`). KAN-776 makes the first real
request, and a first live run that returns something other than `Parasail` in `provider`
is a finding to record here, not a check to relax.
