# ADR-0009: The ask tool's contract

- **Status:** Proposed
- **Date:** 2026-08-19
- **Deciders:** Jian (leejianrong2@gmail.com)

Proposed by an agent under KAN-945, not decided by one. Like
[ADR-0008](0008-shell-isolation-accepted-risk.md), this is a recommendation for Jian to
accept, amend or reject, and Status stays **Proposed** until he does.

Gives the `ask` tool its first real definition. [SLICE-1](../SLICE-1.md) names it once,
in the downstream list, and nowhere else: "slice 2 ... the ask tool (agent asks on
genuine ambiguity)". Nothing in the codebase says when a model may reach for it, what it
takes as arguments, or what happens on a surface with no human — and KAN-946
("implement and wire the ask tool") is already queued behind this card, so those
questions need settling once, here, rather than three times across three PRs.

## Context

**`internal/permission` decides authorization; `ask` is not an authorization
question.** `internal/permission`'s package doc states its whole job: "decides whether
an action the model proposed needs consent, and what the answer means." Its vocabulary —
`Operation`, `Kind`, `Verdict`, `Source` — exists to answer one question, "may this
action proceed," with one of three fixed answers: deny, allow, allow this and later ones
like it. `Gate.classify` has a fixed rule per operation (`run_shell` always asks, a write
outside the root asks, a read never asks), and every operation without a rule is refused
outright by the classifier's default case (`ErrUnknownOperation`) rather than silently
let through.

`ask` runs in the opposite direction. The model is not proposing to *do* something to
the tree that a human might refuse — it is asking the human for information the tree
does not contain. There is no fixed rule to write for it ("ask always asks" is not a
classification, it is the whole of what the tool is), and there is no way to represent
its answer as one of three enum values: a person's reply is prose, not a verdict.
Whatever mechanism `ask` uses, forcing it through `Operation`/`Kind`/`Verdict` would mean
either abusing an existing verdict to also carry a string, or widening `Verdict` for a
concept — "here is what the human said" — that has nothing to do with authorizing an
action. Both are the wrong direction to force a fit.

**The `Consenter`/`asker` pattern is the closest analog, and most of its shape is
reusable even though `internal/permission` itself is not.** `internal/engine/consent.go`
solves an adjacent problem: a front end must not import `internal/permission`
(ADR-0003 decision 2), so `ConsentRequest`/`ConsentAnswer` are engine-level copies of that
package's vocabulary, and `Consenter func(ctx, ConsentRequest) (ConsentAnswer, error)` is
the func value that crosses into `cmd/kopicode`. `asker` adapts a `Consenter` to
`permission.Asker` on the other side. `Options.Consent` (nil, or a `Consenter`) and
`Options.ConsentMode` (`ConsentInteractive`/`ConsentUnattended`) are what a caller of
`engine.Open` actually sets, and `mustAskPolicy` (`internal/engine/open.go`) is what turns
those two fields into a `permission.Policy` at session-build time.

That whole shape — a small request/answer type pair the engine owns, a func value
crossing the boundary, a mode enum recording who is really behind the func so the
journal's attribution is honest, and a nil-safe fail-closed default — has nothing to do
with `Verdict` being three-valued. It is general machinery for "the engine needs to pause
and let the surface decide something a policy object cannot decide for it." `ask` needs
exactly that machinery and none of `internal/permission`'s classification logic, because
there is no classification step: every `ask` call unconditionally needs an answer.

**KAN-885 already worked out what "no human is present" means, twice.** `denyHeadless`
(`cmd/kopicode/print.go`) refuses every consent request unconditionally because
`run --print` has no terminal; pairing it with `Options.ConsentMode =
engine.ConsentUnattended` is what stamps the resulting `journal.PermissionDecided`
`permission.SourcePolicy` instead of `permission.SourceUser`, so a headless refusal is
never recorded as though a person gave it. `permission.BenchPolicy` is the same idea for
`kopibench`: no human, so it answers its own questions and attributes every answer to
`SourcePolicy`. Both existed because the alternative — approving something because no one
was there to say no — is the one behaviour `internal/permission` exists to make
unreachable. `ask` has the same "no human present" case (`run --print`, `kopibench`), and
it needs the same discipline, but the *shape* of the honest answer differs: a shell
command refused for lack of a human is a legitimate deny. A question refused for lack of
a human is not a deny — there is nothing to deny — it is "nobody could answer this," and
the tool call still has to resolve to some text the model can read and act on, because
ending the whole exchange over one unanswerable tool call is a much harsher failure than
anything the permission gate does today.

**KAN-844's catalogue mechanism is the tool's other half.** Every real tool is an
argument struct in `internal/engine/catalogue.go` tagged `json`, `kopicode:"required"`
and `kopicode_desc`, wired into the dispatch table by `entryOf` in
`internal/engine/dispatch.go`, and duplicated as a literal
`[]provider.ToolDefinition` inside `internal/harness/harness.go`'s
`defaultHarnessConfig.ToolCatalogue`, held equal to the real derivation by
`internal/engine/toolcatalogue_test.go`. The system prompt documents every tool in
`ToolSet`'s order under its own `###` heading, and `internal/harness/prompt_test.go`'s
`TestSystemPromptDocumentsEveryTool`/`TestSystemPromptDocumentsEveryToolArgument` hold the
prose to the struct tags. Whatever `ask` becomes as a catalogue entry has to fit this
exact shape or it is not really a tool the model can call the way every other tool is
called.

One concrete constraint fell out of reading that mechanism rather than being obvious in
advance: `parse.ParamType` already declares `TypeArray` (`internal/parse/schema.go`), but
`internal/engine/catalogue.go`'s `paramType` — the reflection-derived mapping `schemaOf`
uses — only handles `reflect.String`, `reflect.Bool`, `reflect.Int` and
`reflect.Float64`. A struct field of kind `reflect.Slice` hits `paramType`'s `default`
case, which panics at package init ("an unmapped type... would be declared
`parse.TypeAny`, which can never fail a type check"). So a structured "suggested answers"
argument is not free today; teaching `paramType` to handle a string slice is a small,
separable, mechanical prerequisite this ADR does not ask KAN-946 to do, because decision 1
below does not need it.

## Decision

### 1. The tool's shape (task item a)

`ask` is a catalogue entry exactly like every other tool: two arguments, both strings,
both following the existing struct-tag convention.

```go
askArgs struct {
    Question string `json:"question" kopicode:"required" kopicode_desc:"the single question to put to the human, phrased so it can be answered in one reply"`
    Context  string `json:"context" kopicode_desc:"what you have already tried or found and why you are stuck; shown to the human alongside the question so they do not have to read the whole session to answer it"`
}
```

Tool description: *"Ask the human a clarifying question when the repository cannot
answer it, and receive their reply as this call's result. Use sparingly — read and grep
first; most ambiguity is resolved by looking, not by asking."* That second sentence is
not decoration. `ask` is the last tool in `ToolSet` (decision 5) precisely because it
should be the model's last resort, and the description should say so where the model
reads it, not just where a human designing the prompt does.

**No `choices`/suggested-answers argument, now.** Considered and rejected in this pass,
not forever — see Alternatives. Free text already carries everything a fixed choice set
would: a model that wants to offer options writes them into `question` itself ("reply
with A, B, or the default, C"). Adding a real structured argument means teaching
`paramType` about `reflect.Slice` first, which is real but small and separate work; there
is no reason to gate this contract on it.

**The result is an ordinary tool result — nothing new on the wire.** The human's answer
text becomes the value of `journal.ToolResult.Output` for this call, through the same
`modelText` path every other tool's output takes, and the model reads it exactly the way
it reads `read_file`'s output: as the next thing in the conversation, not as a
specially-shaped reply. Two reasons. First, uniformity: the loop's contract with the
model is "you called a tool, here is text back," and `ask` inventing a second contract
(a choice index, a structured answer object) would be the one tool call the model has to
parse differently from every other. Second, precedent: `internal/permission`'s answer to
"how does a human's decision reach the record" is already text-shaped where it counts —
`Decision.Reason` is a free string, not a coded value — and forcing `ask`'s answer into
anything narrower than what a permission `Reason` already gets would be a stricter
contract for the case that needs the least structure.

### 2. Crossing the boundary (task item b)

**`internal/permission` is not extended.** No `OperationAsk`, no `KindAsk`, no new
`Verdict` meaning. The reasoning is the Context section's: there is no classification
rule to add (every `ask` call unconditionally needs an answer, so there is nothing for a
`Policy` to decide beyond "did one arrive"), and `Verdict`'s three values cannot carry
prose without becoming something other than a verdict. Widening `internal/permission` for
a direction it was never about would blur the one line CLAUDE.md states as a structural
promise — "the engine decides *that* permission is required and what a decision means" —
by making that package also decide what a *question* means, which is not the same kind of
fact.

**A sibling mechanism, at the same layer, in the same shape, and nowhere near
`internal/permission`.** New engine-level types, parallel to `ConsentRequest` /
`ConsentAnswer` / `Consenter` / `ConsentMode` but declared independently rather than
reused, because reusing the same Go types for two different directions is exactly the
confusion this decision avoids one layer up:

```go
// AskRequest is a question the model asked, as a surface needs it.
type AskRequest struct {
    // Question is the text to show the human.
    Question string
    // Context is the model's optional supporting text — what it tried, why
    // it is stuck. Empty means the model gave none.
    Context string
}

// AskAnswer is what a surface returned.
type AskAnswer struct {
    // Text is the human's reply, verbatim. Empty is a legitimate answer — a
    // human who read the question and pressed Enter answered "nothing to
    // add," which is not the same event as the question going unanswered.
    Text string
}

// Answerer answers a question the model asked.
//
// Shaped like Consenter and crosses the boundary the same way, for the same
// reason: ctx first because an interactive implementation blocks on a human,
// and a turn the user just interrupted must abandon the question rather than
// answer it stale. It returns free text rather than a bounded enum because
// retrieving information, not authorizing an action, is the whole point.
// An implementation that cannot get an answer returns an error — see
// decision 4 for what that means downstream, which is deliberately *not*
// "the exchange ends."
type Answerer func(ctx context.Context, req AskRequest) (AskAnswer, error)

// AskMode says who Answerer's replies are attributed to, mirroring
// ConsentMode for the identical reason: attribution is not the surface's
// fact to assert about itself, and a self-reported "I am a human" is not a
// control.
type AskMode uint8

const (
    AskInteractive AskMode = iota // zero value: a human, relayed by a surface
    AskUnattended                 // nobody is present; Answerer answers on its own
)
```

`engine.Options` gains `Ask Answerer` and `AskMode AskMode`, resolved at `Open` the same
place `Consent`/`ConsentMode` are — but more simply, because there is no `Gate`/`Policy`
pair to build. `mustAskPolicy`'s mirror needs no `permission.NewAsk` call and no `asker`
adapter; it only has to decide, once, which `journal` `Source` string an answer gets
attributed to from `AskMode`, exactly as `mustAskPolicy` maps `ConsentMode` onto
`permission.Source` today. A nil `Options.Ask` is fail-closed by construction, the same
direction as a nil `Consenter`: `Open` substitutes a built-in `Answerer` that always
returns an error saying so, rather than leaving a nil func value for the dispatch code to
call and panic on.

**Dispatch: `ask`'s `toolEntry.op` is `permission.OperationRead`, `mutates` is `false`.**
It changes nothing on the tree, so it earns no `TurnSnapshot` and needs no path
resolution — the `action` closure `entryOf` takes can be `nil`, since there is nothing
about an `ask` call for `permission.Action` to carry. Tagging it `OperationRead` means
`Gate.classify`'s existing rule ("reads never ask") lets it straight through with
`Outcome{Allowed: true}` and zero `PermissionRequested`/`PermissionDecided` events — which
is correct, not a bypass: there was never a permission question here for the gate to
skip. The actual pause-and-collect-an-answer step happens entirely inside `ask`'s own
`run` closure, which calls `e.cfg.Ask` directly — the same way `edit_file`'s closure calls
`journalEdit` directly for logic no generic dispatch step can express.

**A refusal is non-fatal, and turns into an ordinary tool result.** `entry.run`'s error
return is reserved for the harness's own fatal failures (CLAUDE.md, `dispatch.go`'s own
doc comment: "a tool that failed is an observation, not an exception"). When `e.cfg.Ask`
returns an error — no human present, the terminal broke, the turn was cancelled while the
question sat on screen — `ask`'s closure catches it and returns a normal `toolOutcome`
whose `err` field carries it, exactly the shape a denied `run_shell` call already takes
through `faultOf` (a `permission.ErrDenied` is classified `tools.FaultTask`, "the harness
worked exactly as designed and the answer was no," not `FaultInternal`). The model sees a
plain-text explanation as this call's result and the turn continues; nothing about `ask`
failing ends the exchange, which is the property decision 4 depends on.

### 3. Journal event shape (task item c)

**Two events, `AskRequested` and `AskAnswered`, mirroring `PermissionRequested` /
`PermissionDecided`'s split, for the same reason.** The model's question and whatever
comes back are two separate moments that can be arbitrarily far apart in wall-clock time
— a REPL blocks on a real human at a real terminal — and a single combined event would
have no way to represent a question the turn was cancelled before anyone answered, the
same gap a single `PermissionAsked` event would leave for a shell command nobody got to
decide on. Splitting them lets `AskRequested` exist alone on the record when the turn is
interrupted mid-question, which is not a hole in the journal, it is the honest shape of
what happened.

```go
// AskRequested records that the model paused the turn to ask the human a
// question. It is written before the engine calls Options.Ask, so a turn
// cancelled while the question is outstanding leaves this event with no
// matching AskAnswered — representable, not a gap.
type AskRequested struct {
    CallID   string `json:"call_id"`
    Question Text   `json:"question"`
    // Context is the zero Text when the model gave none — a meaningful zero,
    // not a forgotten field; see the note below for what that means for
    // whoever wires the completeness-guard fixture.
    Context Text `json:"context"`
}

func (AskRequested) Type() Type { return TypeAskRequested }

// AskAnswered records what came back and who is answerable for it.
type AskAnswered struct {
    CallID string `json:"call_id"`
    // Answer is the text returned to the model as this call's ToolResult.
    // Recorded again here — the same duplication EditApplied.Diff already
    // has with ToolResult.Output for an edit — because Source is not
    // otherwise attributable: without a self-sufficient event, "a human
    // answered this" and "nobody was present and the harness said so" read
    // as the identical ToolResult until someone cross-references CallID
    // against the PermissionDecided-style record this type exists to be.
    Answer Text `json:"answer"`
    // Source is "user" or "policy" — PermissionDecided's own vocabulary,
    // for the same reason: a headless run's canned refusal must never be
    // indistinguishable from a person answering.
    Source string `json:"source"`
    // Refused is true when nobody actually answered: no human present, or
    // the turn was cancelled while the question was open. Answer then
    // carries the reason shown to the model, not a person's words.
    Refused bool `json:"refused"`
}

func (AskAnswered) Type() Type { return TypeAskAnswered }
```

These are sketched here to pin the contract down precisely and are **illustrative only**:
they are not added to `internal/journal/payload.go`, not given `TypeAskRequested`/
`TypeAskAnswered` constants in `internal/journal/event.go`'s registry, and not wired into
any dispatch code. KAN-946 does that. `event.go`'s own rule — "payload types are added
without a bump" — means landing them later moves nothing about `SchemaVersion`.

**Both types are checked against the journal's two structural guards, in prose, since
they cannot be checked by the enforcing tests until they are real:**
`TestNoPayloadFieldCanHoldACredential` walks every field name for a denylist of
credential-shaped substrings (`secret`, `credential`, `password`, `header`, `env`, …) —
`CallID`, `Question`, `Context`, `Answer`, `Source`, `Refused` match none of them.
`TestNoPayloadFieldIsAFreeFormBag` refuses any field of `reflect.Kind` map — neither
struct has one. Both hold by inspection today and both tests will hold them mechanically
once KAN-946 registers the types and adds them to `payload_test.go`'s `fixtures()`.

**One note for whoever wires this for real:** `AskRequested.Context`'s zero value is
meaningful (the model gave none), which is exactly the shape
`internal/journal/completeness_test.go`'s `TestFixturesHaveNoUnexplainedZeroFields` is
built to catch and exempt correctly — it will need a `zeroExemptions` entry the same way
`SessionEnded.ExitCode`'s `0` already has one, with a reason, not a silently-passing
fixture that happens to leave it unset.

### 4. `run --print` and `kopibench` — no human present (task item d)

**The refusal is entirely inside the `Answerer`, never inside `internal/permission`,
because `ask` was never routed through the gate that decides consent.** This is the
concrete payoff of tagging `ask` `OperationRead` in decision 2: `Gate.classify` lets every
`ask` call through unconditionally, so `BenchPolicy` and `denyHeadless`'s existing
refusal logic — built entirely around `Operation`/`Kind` — never sees an `ask` call at
all and needs no new case.

`cmd/kopicode/print.go` gets `denyHeadless`'s sibling:

```go
// denyHeadlessAsk answers every ask request by saying nobody is present.
//
// Same posture as denyHeadless, one layer over: there is nobody to ask, so
// the honest answer names that rather than inventing one, and it is not an
// error that ends the session — see AskAnswered below and dispatch.go's
// handling of Answerer's error return.
func denyHeadlessAsk(context.Context, engine.AskRequest) (engine.AskAnswer, error) {
    return engine.AskAnswer{}, errors.New("no human is present to answer this question")
}
```

wired the same place `denyHeadless` is: `opts.Ask = denyHeadlessAsk`, `opts.AskMode =
engine.AskUnattended`. The resulting `AskAnswered` carries `Source: "policy"`, `Refused:
true`, and the model's `ToolResult` reads the refusal's text — worded so the model knows
to proceed on its own judgement rather than retry the same call, matching the existing
`run_shell`/consent refusal's "a refusal is an answer: do not send the same call again"
line already in `prompt_default.md`. The exact wording is a prompt-writing decision for
KAN-946/whatever lands the prompt section, not settled here; the *behaviour* — non-fatal,
in-band, attributed to policy, never a retry target — is settled here and should not be
re-opened.

`kopibench` gets the identical treatment, symmetric with `BenchPolicy`'s existing
"no human, so it answers its own questions and says so" posture: either a
runner-supplied `Answerer` that refuses unconditionally, or nothing at all, since a nil
`Options.Ask` already resolves to the same fail-closed default `engine.Open` substitutes
everywhere.

**Why this is a runtime behaviour and not a catalogue-time exclusion.** The other way to
solve "no human on this surface" would be to leave `ask` out of the `ToolSet` a headless
session is given. That does not work here, and the reason is structural rather than a
matter of taste: a harness configuration is resolved from the **model id**, not from
which surface opened the session (ADR-0007 decisions 2 and 5 — `--model`, then
`.kopicode/config.toml`, then the built-in default; no surface axis anywhere in that
chain). `cmd/kopicode`'s own interactive REPL and its own `run --print` mode are the exact
same binary resolving the exact same model to the exact same `harness.Config`, so the
same `ToolSet` is what both get. Making `run --print` receive a different `ToolSet` than
the REPL for the identical model would mean inventing a fourth thing harness resolution
depends on — surface identity — which is precisely the kind of fundamental re-decision
this ADR exists so KAN-946 does not have to make. The fix has to live behind the
`Answerer`, because that is the one seam that already varies per surface without
touching harness resolution at all.

### 5. Harness configuration growth (task item e)

Landing `ask` for real touches every place KAN-844 wired a tool through:

- **`ToolSet`** gains `"ask"`, appended last. Order in `ToolSet` is presentation order in
  the prompt (KAN-843) and the order a model is shown its options; `ask` belongs after
  `run_shell` because it is meant to be the last resort — the description in decision 1
  says so, and the ordering says it again.
- **`ToolCatalogue`** gains the literal `provider.ToolDefinition` for `ask`, matching
  `askArgs`'s tags exactly, held equal to the real derivation by whatever
  `toolcatalogue_test.go` becomes once it also has to cover this entry.
- **`prompt_default.md`** gains a `### ask` section in `ToolSet`'s order, documenting
  `question` and `context` and stating the refusal case explicitly — the model needs to
  know in advance that some sessions have nobody to answer, and what a refusal there
  means, the same way the anchor-rejection reasons are each given their own recovery
  instruction. `TestSystemPromptDocumentsEveryTool` and
  `TestSystemPromptDocumentsEveryToolArgument` hold the section to the struct tags;
  `TestSystemPromptFitsItsBudget`'s 8 KiB cap is what would notice if the section runs
  long.
- **`internal/engine/catalogue.go`** gains `askArgs` and **`dispatch.go`** gains the
  `entryOf` registration with the `run` closure described in decision 2. No change to
  `dispatch`'s generic per-call flow (`ToolCallRequested` → `ToolCallParsed` → the entry's
  `run` → `ToolResult`) — `ask`'s closure adds `AskRequested`/`AskAnswered` around its own
  call to `e.cfg.Ask`, the same way `edit_file`'s closure adds `EditApplied`/`EditRejected`
  around its own call to `journalEdit`.
- **`AdvertiseNativeTools` and `ParseRoutes` are untouched.** `ask` is dispatched, parsed
  and repaired through the exact same three extraction routes and the exact same wire
  catalogue every other tool uses. It introduces no new route and no new wire shape.

**The hash moves once, and that is expected, not a regression to explain away.**
`ToolSet` and `ToolCatalogue` are both in `Config`'s hash preimage (ADR-0007 decision 6:
"the tool set as presented to the model: names, schemas, descriptions"), so adding `ask`
to either moves `defaultHarnessConfig`'s hash and — because `minimaxM2Config` is built by
copying `defaultHarnessConfig` (`harness.go`'s `minimaxM2Config` function) — moves that
configuration's hash too, together. This is a **configuration value** changing, the same
category KAN-843's system-prompt landing was, not an **encoding** change like KAN-844's
new struct fields were — so `PreimageVersion` does not move, only the hash value does,
exactly the precedent CLAUDE.md records for the prompt ("`PreimageVersion` stayed at 1,
and nothing was invalidated because no bench run has happened"). The same is true here as
of this writing: no bench run has happened at either configuration's current hash, so no
existing result is invalidated by the move.

**Turning `ask` on is a per-configuration choice, not a global capability, exactly like
`AdvertiseNativeTools`.** This ADR settles the tool's shape and the mechanism that
crosses the boundary; it does not mandate that every registered harness configuration
must carry `ask` forever. A future configuration that wants to withhold it — a model
observed to call it too eagerly, a bench-only variant that would rather fail the task
than pause it — gets to do that the same way `MinimaxM2ConfigName` already diverges from
`defaultHarnessConfig` in exactly one field (`minimaxM2Config`'s doc comment: "copying the
base and changing exactly that field"): copy the base, drop `"ask"` from `ToolSet` and its
matching entry from `ToolCatalogue`. No new ADR is needed for that decision, the same way
none was needed for `AdvertiseNativeTools` or `ParseRoutes` varying by configuration.

## Alternatives rejected

- **Extending `internal/permission` with `OperationAsk`/`KindAsk` and a `Verdict`-shaped
  answer.** Rejected in decision 2: there is no classification rule to write (every
  `ask` call unconditionally needs an answer), and `Verdict`'s three fixed values cannot
  carry a human's prose reply without either abusing one of them or widening the type for
  a concept that has nothing to do with authorizing an action — the same category error
  CLAUDE.md's "engine decides policy" line exists to prevent, one layer inside the
  package that line is about.
- **A single combined event instead of `AskRequested`/`AskAnswered`.** Rejected: it
  cannot represent a question the turn was cancelled before anyone answered, which
  `PermissionRequested` alone (without a matching `PermissionDecided`) already can for
  the identical reason. Collapsing the split loses exactly the case that split exists
  for.
- **Reusing `ConsentRequest`/`ConsentAnswer`/`Consenter` literally**, adding a fourth
  `ConsentAnswer` value that carries text. Rejected: `ConsentAnswer` is a closed
  three-valued enum with a 1:1 mapping onto `permission.Verdict`
  (`asker.Ask`'s `switch` is exhaustive over exactly those three), and a fourth value
  that means something structurally different (a string payload, not a bounded choice)
  would break that mapping's cleanliness for every consumer that assumes the enum is
  closed and verdict-shaped. A parallel, independently-declared pair costs a few lines of
  duplication and buys back the property that neither type has to anticipate the other's
  use.
- **A structured `choices` argument** (an array of suggested answers the model offers).
  Rejected for now: `internal/engine/catalogue.go`'s `paramType` has no case for
  `reflect.Slice` and panics at package init on one today, even though
  `parse.ParamType.TypeArray` already exists as a value. Free text already carries
  everything a fixed choice set would — the model spells options into `question` itself —
  so there is no capability gap this would close, only a schema nicety, and it is left as
  a separable, later card rather than a reason to hold up the contract.
- **Excluding `ask` from `ToolSet` on a headless surface**, resolved at harness-selection
  time. Rejected in decision 4: harness configuration resolves from the model id, not
  from which surface opened the session (ADR-0007 decisions 2 and 5), so there is no axis
  to key the exclusion on without inventing a new one — exactly the fundamental
  re-decision this ADR exists to keep off KAN-946's plate.
- **Blocking the turn indefinitely under `run --print` when `ask` is called, on the theory
  that a human might be watching the JSON stream and could answer some other way.**
  Rejected outright. `run --print` accumulates nothing and holds no session state by
  design (KAN-794); a headless surface that could hang forever waiting on an answer
  nobody is positioned to give is a worse failure mode than a wrong or refused answer, and
  it matches nothing else in this project's fail-closed, never-silent posture.
- **Treating `Answerer`'s error return as fatal**, ending the exchange the way a provider
  or harness failure does. Rejected: a `run_shell` call denied consent does not end the
  session either, and there is no reason `ask` should be held to a stricter standard than
  the tool it is most structurally similar to. The model asked a question nobody could
  answer; that is an observation, the same as any other tool's honest failure, not a
  reason to stop.

## Consequences

- `internal/permission` gains nothing from this card: no new `Operation`, no new `Kind`,
  no new `Verdict` meaning, no new test surface. That absence is the point, not an
  oversight — restated here because the instinctive first move ("an interactive question
  is a permission concept") is exactly the move this ADR argues against.
- The harness config hash moves once `ask` lands in `ToolSet`/`ToolCatalogue`, for both
  registered configurations today (`default` and `minimax-m2-v1`, since the latter is
  built from the former). Expected and inert: no bench run exists yet at either current
  hash.
- A model that never calls `ask` pays a small, fixed, one-time cost: the prompt section's
  share of the 8 KiB budget and the catalogue entry's share of the request payload.
  Nothing about the loop's behaviour changes for a session that never invokes it.
- The three-bucket classifier (ADR-0006 §3) is not extended by this card. An `ask` call
  refused under a headless run (`Source: "policy"`, `Refused: true`) is data that will
  exist once KAN-946 wires this, and it is worth someone's attention when it does — a
  bench task that legitimately needs a human mid-run is a task the corpus should probably
  not have scored as answerable headless in the first place — but deciding whether that
  becomes its own attribution rule, folds into `unattributed`, or is left as a raw signal
  for now is not decided here.
- The REPL's rendering is not implemented here, and CLAUDE.md's "surfaces decide
  presentation" rule means it should not be decided by this ADR either — only the
  concrete shape it has to satisfy is: print the question (and context, when given) as
  ordinary lines the way `cmd/kopicode/repl/consent.go`'s `renderRequest` does, read one
  line as the literal answer with no `y`/`n`/`a` interpretation, and treat only
  `Ctrl-C`/`EOF`/context-cancellation as unanswerable — an empty line a human actually
  submitted is a real answer, not a refusal, which is a deliberate departure from
  consent's "empty means no" convention: free text has no safe default to fall back on,
  where a refused permission does.
- Whoever implements `AskRequested`/`AskAnswered` for real should expect
  `internal/journal/completeness_test.go`'s zero-field guard to flag `Context` on a
  fixture where the model gave none, and should resolve it with a reasoned
  `zeroExemptions` entry rather than inventing a placeholder value — this is named above
  so it reads as expected, not as a fixture bug to chase.
