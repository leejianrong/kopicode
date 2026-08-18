# The provider pin

- **Observed:** 2026-08-14 (qwen/qwen3-coder-next), 2026-08-18 (minimax/minimax-m2,
  z-ai/glm-5.2) — all unauthenticated OpenRouter API
- **Card:** KAN-775 (qwen/qwen3-coder-next), KAN-850 (minimax/minimax-m2, z-ai/glm-5.2)
- **Rule it satisfies:** [ADR-0005 §2](adr/0005-benchmark-and-ab-methodology.md),
  [SLICE-1 §7](SLICE-1.md), PRD R13, [ADR-0007 decision 5](adr/0007-model-selection-and-harness-config-shape.md)

ADR-0005 §2 says *that* every benchmark request pins a provider. This file says *which*,
for every model `internal/harness`'s registry knows, and shows the traffic each choice was
made from. The two are separate on purpose: the rule is a decision of record and does not
change, while a value is operational and will change the first time a provider retires an
endpoint. Editing an ADR every time a quantization moves is how an ADR stops meaning
anything.

This file has one section per registered model. The first section, below, is
`qwen/qwen3-coder-next` — the original slice-1 target and the one every generic mechanism
here (the two-namespace note, the quantization-honesty rule) was first argued against.
Later sections (`minimax/minimax-m2`, `z-ai/glm-5.2`) apply the same rule to different
observed traffic and do not re-argue it; read this one first if the shape is unfamiliar.
The README's fourth row — "a current frontier model" — is a *role*, not a model, and has no
section here: the README's own text says naming one now would only date the table, and a
section would need a real pin, which does not exist until that model is chosen.

## qwen/qwen3-coder-next

### The pin

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

### Two names, and they are not interchangeable

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

### What was observed

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

### Why Parasail, `bf16`

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

### The quantization is reported, and this is worth being explicit about

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

### Cost, as of 2026-08-14 (pricing) and 2026-08-18 (a real measurement)

The prices above are **unchanged** from the ones README.md recorded on 2026-08-11
($0.12/$0.80), so the "roughly $2 per full corpus run" figure CLAUDE.md used to carry
rested on those numbers rather than an observation.

**KAN-800 replaced the arithmetic with a measurement on 2026-08-18.** One full run of the
10-task corpus against this pin: 621,368 prompt tokens, 12,587 completion tokens, **total
cost $0.0846** — well under the $2 estimate. `run-20260817t185416z`'s report: 9/10 tasks
passed; the one failure (`go-config-retries`) ran to the `max_turns` cap rather than
converging (classified `harness` per KAN-797's mechanical rule; root cause traced through
the journal to a model self-correction failure, not a harness defect — see the KAN-800
card's comments and CLAUDE.md's Build status section for the full trace). Being capped by
turns rather than runaway token growth is exactly why this run came in so far under the
estimate: a stuck task pays for 20 turns of conversation, not for an unbounded retry loop.

The old arithmetic, kept for context: at $0.12/M prompt and $0.80/M completion, $2 buys
roughly 16M prompt tokens, or about 1.6M per task across the 10-task corpus — a much
longer, more context-heavy loop than this run actually needed. Treat $0.0846 as this run's
actual cost under normal conditions, and $2 as the safer order-of-magnitude figure to
budget against, since a corpus with more stuck tasks (each paying the full turn-cap cost)
would land somewhere between the two. A second real run's reported usage is the next data
point, not a replacement for this one.

Prefix caching is available on this endpoint at $0.07/M read but is **not** implicit
(`supports_implicit_caching: false`), so nothing is cached unless the client asks. ADR-0005
§4 already warns against designing the experiment around caching; note only that the lever
exists.

### Re-checking this, and what would invalidate it

Re-run the two `curl` commands above. The pin still holds if, and only if, all of these
are true:

1. `qwen/qwen3-coder-next` is still in the catalogue under that exact slug.
2. An endpoint with `tag: "parasail/bf16"` still serves it.
3. That endpoint still reports `quantization: "bf16"`.

If (2) or (3) has changed, the pin is **not** silently updated to whatever replaced it.
Results gathered under the old pin and the new one are two experiments, and ADR-0005 §2's
discard rule is what keeps them apart. Change the value here, date the change, and start a
new experiment series.

Between 2026-08-14 and 2026-08-18, nothing in this file had been exercised against a live
completion request — see "Live confirmation" below for what changed and what remains
unverified (the tool-call delta shape; §3 there).

**KAN-776 built the client that can ask, and did not ask.** The client landed on
2026-08-14 and every one of its tests runs against an `httptest` server, because
`OPENROUTER_API_KEY` was not set in the environment the card was built in and nothing in
`make ci` may spend money. So the pin was argued from the endpoints response above and
from documentation, and not from a completion, until KAN-852.

Asking is one command, and it costs cents:

```bash
make smoke-live          # one 16-token request against the pin; needs OPENROUTER_API_KEY
```

### Live confirmation, 2026-08-18 (KAN-852)

`make smoke-live` ran against the pin above with a real `OPENROUTER_API_KEY`. Output,
verbatim:

```
model requested: qwen/qwen3-coder-next
model reported:  qwen/qwen3-coder-next
provider field:  "Parasail"
finish reason:   "stop"
usage:           {Prompt:20 Completion:2 Total:22}
content:         "pong"
```

Wire transcript (SSE frames, `[DONE]` terminator):

```
data: {"id":"gen-1786988078-NGfmEgJ5Z2MYGfqmExLw","object":"chat.completion.chunk","created":1786988078,"model":"qwen/qwen3-coder-next","provider":"Parasail","choices":[{"index":0,"delta":{"content":"pong","role":"assistant"},"finish_reason":null,"native_finish_reason":null}]}

data: {"id":"gen-1786988078-NGfmEgJ5Z2MYGfqmExLw","object":"chat.completion.chunk","created":1786988078,"model":"qwen/qwen3-coder-next","provider":"Parasail","choices":[{"index":0,"delta":{"content":"","role":"assistant"},"finish_reason":"stop","native_finish_reason":"stop"}]}

data: {"id":"gen-1786988078-NGfmEgJ5Z2MYGfqmExLw","object":"chat.completion.chunk","created":1786988078,"model":"qwen/qwen3-coder-next","provider":"Parasail","service_tier":null,"choices":[{"index":0,"delta":{"content":"","role":"assistant"},"finish_reason":"stop","native_finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":2,"total_tokens":22,"cost":0.000004,"is_byok":false,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":0,"audio_tokens":0,"video_tokens":0},"cost_details":{"upstream_inference_cost":0.000004,"upstream_inference_prompt_cost":0.0000024,"upstream_inference_completions_cost":0.0000016},"completion_tokens_details":{"reasoning_tokens":0,"image_tokens":0,"audio_tokens":0}}}

data: [DONE]
```

The three things flagged above, resolved:

1. **The `provider` field's casing.** `"Parasail"` — Title case, exactly the display name
   from the endpoints listing, not `"parasail"` or the request slug `"parasail/bf16"`.
   ADR-0005 §2's discard rule compares it case-insensitively, which this observation says
   was the right call: nothing about the response guaranteed the request-slug casing would
   survive into the reply.
2. **Whether `parasail/bf16` routes.** Yes. The request pinned `provider.order:
   ["parasail/bf16"]` with `allow_fallbacks: false`, and the response's `provider` field
   came back `"Parasail"` rather than a routing failure — the pin names a real, reachable
   endpoint.
3. **The streaming tool-call delta shape.** Still unverified, as expected — this request
   carried no tool catalogue (`provider.Request` doesn't have one; that's KAN-844), so
   nothing here could provoke a tool call. The hand-authored fixtures' claim to follow
   OpenAI's contract remains on stated faith until KAN-844 lands and a live request can
   ask.

Cost of this one request: **$0.000004** (20 prompt + 2 completion tokens), reported by
OpenRouter itself in the frame above — the first real usage number in this repository,
replacing arithmetic with a measurement for exactly one request. It does not move the
per-corpus-run estimate below; that still waits on KAN-800.

## minimax/minimax-m2

- **Observed:** 2026-08-18 (unauthenticated OpenRouter API)
- **Card:** KAN-850
- **Registered as:** A/B candidate (README's `## Models` table). Maps to
  `DefaultConfigName` — a second harness configuration is slice 2
  (docs/SLICE-1.md, "Explicitly deferred"), and nothing found while choosing this
  pin gives a concrete reason the existing configuration is wrong for this model.

### What was observed

```bash
curl -sS https://openrouter.ai/api/v1/models | \
  python3 -c 'import json,sys; print("minimax/minimax-m2" in [m["id"] for m in json.load(sys.stdin)["data"]])'

curl -sS https://openrouter.ai/api/v1/models/minimax/minimax-m2/endpoints
```

`minimax/minimax-m2` **is** in the catalogue under exactly that slug. The README's own
header dates its table to 2026-08-11, a week stale by the time this was checked, so the
near-miss neighbours were re-checked rather than trusted: `minimax/minimax-m1`,
`minimax/minimax-m2.1`, `minimax/minimax-m2.5`, `minimax/minimax-m2.7`,
`minimax/minimax-m2-her`, `minimax/minimax-m3` and `minimax/minimax-01` are all real,
separately-priced models on OpenRouter today, and any one of them would have been a
plausible slug to pin by accident. `minimax/minimax-m2` is still the README's spelling and
still resolves.

Three providers served it on 2026-08-18. Prices are USD per million tokens; `uptime` is
OpenRouter's own 24-hour figure at the time of the look, and moves.

| Provider | `tag` | `quantization` | Context | Max output | Prompt | Completion | `seed`? | Uptime 1d |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| **Minimax** | `minimax/fp8` | **`fp8`** | 204,800 | 131,072 | $0.255 | $1.02 | **no** | 99.85% |
| Novita | `novita/fp8` | `fp8` | 204,800 | 131,072 | $0.30 | $1.20 | yes | 99.24% |
| Google | `google-vertex` | `unknown` | 196,608 | 196,608 | $0.30 | $1.20 | yes | 99.96% |

All three report `tools` and `tool_choice` in `supported_parameters`, so tool calling was
not a discriminator.

### Why Minimax, `fp8`

**No endpoint reports a full-precision quantization for this model at all.** Unlike
`qwen/qwen3-coder-next`, where a `bf16` endpoint existed and decided the choice outright,
the highest fidelity on offer here is `fp8`. That narrows the field to the two real
(non-`unknown`) quantizations: Minimax and Novita.

**Google's `unknown` is excluded on the same ground `qwen`'s StreamLake and Alibaba
were.** `quantizations: ["unknown"]` is a legal filter value and a worthless pin — it
fixes the label while leaving the served weights free to change underneath it.

**Between the two `fp8` endpoints, Minimax is strictly cheaper — on both halves of the
price.** $0.255/$1.02 against $0.30/$1.20, the same clean-cut case `qwen3-coder-next`'s
Parasail pin was: nothing to trade off between price and quantization tier, since both
endpoints report the same `fp8`.

**It is also the model's own first-party endpoint.** MiniMax (the company that trained
and released MiniMax-M2) serving its own weights is, if anything, a stronger claim to
fidelity than a third-party reseller's declared `fp8`, though the declared quantization
value is what the pin actually checks.

**The honest limitation: this endpoint does not support `seed`.** Novita and Google both
do; Minimax's `supported_parameters` list does not include it. ADR-0005 does not require
seed support — the qwen pin's own argument called it a variance-reduction bonus, not a
precondition — so this is recorded rather than treated as disqualifying. If seed control
over this arm becomes a requirement later, Novita's `novita/fp8` is the documented
alternative: same quantization, $0.045/$0.18 more per million tokens (prompt/completion),
and `seed` in its `supported_parameters`.

Uptime was not used as a criterion, per the same reasoning as the qwen pin: all three sit
above 99.2% and the figure moves between one fetch and the next.

### The quantization is reported, and this is worth being explicit about

`fp8` is the literal value in Minimax's endpoint record (`"quantization":"fp8"`), and it
is one of the twelve documented values `internal/provider/fixture`'s vocabulary accepts.
Had every endpoint reported no quantization, the honest pin would have been
`quantizations: ["unknown"]`, stated as such — it did not come to that here.

## z-ai/glm-5.2

- **Observed:** 2026-08-18 (unauthenticated OpenRouter API)
- **Card:** KAN-850
- **Registered as:** A/B candidate, long-context (README's `## Models` table). Maps to
  `DefaultConfigName` for the same reason `minimax/minimax-m2` does — a second harness
  configuration is slice 2, and nothing found here argues against the existing one.

### What was observed

```bash
curl -sS https://openrouter.ai/api/v1/models | \
  python3 -c 'import json,sys; print("z-ai/glm-5.2" in [m["id"] for m in json.load(sys.stdin)["data"]])'

curl -sS https://openrouter.ai/api/v1/models/z-ai/glm-5.2/endpoints
```

`z-ai/glm-5.2` **is** in the catalogue under exactly that slug. The near-miss neighbours
are real, separately-priced, currently-served models — `z-ai/glm-5.1`, `z-ai/glm-5`,
`z-ai/glm-5-turbo`, `z-ai/glm-5v-turbo`, `z-ai/glm-4.7`, `z-ai/glm-4.6`, `z-ai/glm-4.5` and
several more — and `z-ai/glm-5.2` itself has `:batch` and `:free` variants that are
separate catalogue entries. The README's own header dates its table to 2026-08-11; the
exact version string was re-checked rather than trusted, and it still resolves.

Thirty-one endpoints served it on 2026-08-18 — far more than either other registered
model. Prices are USD per million tokens; `uptime` is OpenRouter's 24-hour figure at the
time of the look, and moves. The table below is the endpoints that matter to the
argument, not all thirty-one; the two real quantization tiers on offer were `fp8` and
`fp4`, with `unknown` also present (DigitalOcean, Fireworks, Cloudflare, Friendli,
Together and others) and excluded on the same ground as every other pin in this file.

| Provider | `tag` | `quantization` | Context | Max output | Prompt | Completion | `seed`? | Uptime 1d |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| **Sail Research** | `sail-research/fp8` | **`fp8`** | 1,048,576 | 131,072 | $0.50 | $3.15 | yes | 99.57% |
| StreamLake | `streamlake/fp8` | `fp8` | 1,024,000 | 128,000 | $0.56 | $1.76 | **no** | 99.20% |
| Novita | `novita/fp8` | `fp8` | 1,048,576 | 131,072 | $0.7406 | $2.3276 | yes | 97.95% |
| Decart | `decart/fp4` | `fp4` | 1,048,576 | 1,048,576 | $0.684 | $2.28 | yes | 98.64% |
| DeepInfra | `deepinfra/fp4` | `fp4` | 1,048,576 | 163,840 | $0.75 | $2.40 | yes | 98.89% |
| Z.AI (native) | `z-ai/fp8` | `fp8` | 1,048,576 | 131,072 | $1.40 | $4.40 | **no** | 99.11% |

All six report `tools` and `tool_choice` in `supported_parameters`, so tool calling was
not a discriminator.

### Why Sail Research, `fp8`

**No endpoint reports a full-precision quantization for this model either.** The best
real quantization on offer is `fp8`, one step up from `fp4` in precision (8 bits of
mantissa/exponent budget against 4), so — absent a full-precision option — `fp8` is
treated as the more faithful representation of the released weights and preferred over
`fp4` the same way `qwen`'s pin preferred `bf16` over `fp8`. That excludes Decart and
DeepInfra despite both being cheaper on paper than most `fp8` rows.

**Among `fp8` endpoints, "cheapest" is not a clean sweep the way it was for `qwen` or
`minimax-m2`, and that is worth being explicit about rather than picking silently.**
Sail Research has the lowest *prompt* price ($0.50) but not the lowest *completion*
price (StreamLake's $1.76 beats Sail Research's $3.15). A raw per-token comparison of
the two components does not settle it on its own.

**What settles it: the actual token ratio this project has measured.** KAN-800's one full
corpus run reported 621,368 prompt tokens against 12,587 completion tokens — roughly
50:1 — because an agent loop spends most of its tokens reading files, tool output and
prior turns, not generating replies. Applying that ratio:

- Sail Research: 621,368 × $0.50/M + 12,587 × $3.15/M ≈ $0.3107 + $0.0396 = **$0.3503**
- StreamLake: 621,368 × $0.56/M + 12,587 × $1.76/M ≈ $0.3480 + $0.0222 = **$0.3701**

Sail Research is cheaper in aggregate under the ratio this project actually observed,
even though StreamLake's completion price alone looks better. A pin argued from the raw
per-component prices without this step would have been ambiguous; argued from the
observed traffic shape, it is not.

**Sail Research also supports `seed`; StreamLake does not.** Unlike the `minimax-m2` pin,
this is not a trade-off against price — the cheaper-in-aggregate endpoint is also the one
that supports it, so there is nothing to weigh here.

**Sail Research reports the model's full declared context, 1,048,576 tokens; StreamLake
truncates to 1,024,000.** `z-ai/glm-5.2` is registered specifically as the long-context
A/B candidate (README's Role column), so the endpoint that serves the whole advertised
window is the one that matches the reason this model is registered at all.

Uptime was not used as a criterion, per the same reasoning as the other two pins in this
file: the figure moves between one fetch and the next, and 99.57% against 99.20% is not
a difference worth deciding on.

### The quantization is reported, and this is worth being explicit about

`fp8` is the literal value in Sail Research's endpoint record (`"quantization":"fp8"`),
one of the twelve documented values `internal/provider/fixture`'s vocabulary accepts.
Nothing here was invented: every real endpoint for this model reports either `fp8` or
`fp4`, both real, and `fp8` was chosen for fidelity, not assumed.

### Re-checking either new pin

The same three-part check applies as for `qwen/qwen3-coder-next`, per model: the slug is
still in the catalogue under that exact spelling, the chosen `tag` still serves it, and
that endpoint still reports the quantization recorded here. If any of the three has
changed, the pin is **not** silently updated — change the value in
`internal/harness/registry.go`, date the change here, and start a new experiment series
per ADR-0005 §2's discard rule. Neither `minimax/minimax-m2` nor `z-ai/glm-5.2` has had a
live completion request driven against it yet (no `make smoke-live` equivalent exists per
model); both pins are argued from the endpoints response only, the same position
`qwen/qwen3-coder-next` was in before KAN-852.
