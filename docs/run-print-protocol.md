# The `kopicode run --print` protocol

- **Surface:** [`cmd/kopicode/print.go`](../cmd/kopicode/print.go)
- **Rule it satisfies:** `docs/SLICE-1.md` build step 14 (affordance U2)

This file documents the wire `run --print` speaks, for a headless caller that consumes
it without reading the Go source — a CI job, a bench harness, or an orchestrator like
cuttlefish-crew's `AgentBackend` wrapping the binary. The register is
[`kopicode-serve-protocol.md`](kopicode-serve-protocol.md)'s: this describes the
concrete message shapes and the stable vocabulary a consumer can code against; where it
disagrees with `cmd/kopicode/print.go`'s own doc comment or its tests, the code is the
truth.

## Transport and framing

Newline-delimited JSON on stdout, one object per line, `kind` on every line. `stderr`
carries only a human-readable summary line and, with `--debug`, engine diagnostics —
never anything a consumer needs to parse. **Nothing on stdout is ever truncated**: a
value over the journal's spill threshold reports its size rather than its bytes (see
`size` below), but nothing that does appear on the stream is clipped.

**Line 1 is the stream header**, the only line that is not a journal-derived event:

```json
{"kind":"stream","schema":1,"record":"/repo/.kopicode/sessions/<id>"}
```

`schema` is the wire's version, bumped when the *meaning* of a field changes — adding a
field is not a version change. `record` is the journal directory this session's full
history lives in.

**Every other line is an event**, each one the fields of one `internal/journal` payload
copied across at the moment the record accepted it — the append happens first and only
what was actually recorded is announced (ADR-0002 decision 2: this stream cannot say
anything the journal does not hold). Zero-value fields are omitted (`omitempty`), so a
line carries only what its `kind` populates.

```json
{"kind":"session_started","seq":1,"detail":"<session id>","text":"<model id>","reason":"<harness config hash>"}
{"kind":"user_message","seq":2,"turn":1,"text":"fix the failing test","size":20}
{"kind":"tool_call_parsed","seq":5,"turn":1,"tool":"write_file","detail":"{\"path\": ...}","reason":"native"}
{"kind":"turn_cancelled","seq":8,"turn":1,"text":"context canceled","reason":"provider_stream"}
{"kind":"session_ended","seq":9,"reason":"completed","exit_code":0}
```

Three fields need stating because their zero value is meaningful and omission would
otherwise read as false:

- `exit_code` is present only when the event carries one at all — never zero-and-
  therefore-fine.
- `ran` is present only on `syntax_gate`, where `false` is a real answer: a gate that
  did not run must not read as a pass.
- A `provider_response` also carries `usage`: `prompt`, `completion`, `total`, and `cache_read`,
  `cache_write` and `cost_usd` when the provider reported them (an absent `cost_usd` is unknown, never
  zero). `size` on that event is still the total. See [serve's Usage section](kopicode-serve-protocol.md#usage-adr-0021).
- `size` is the size the *record* holds, not the length of `text`. Content over the
  journal's spill threshold arrives with an empty `text` and a real `size`; nothing is
  truncated, because nothing over the threshold is carried inline in the first place.

## The exit code is on the stream, and so is why

The last line is always `session_ended`, and its `exit_code` is exactly the code the
process itself exits with — both come from the same `engine.Stop`, so a consumer
reading the stream never has to guess an outcome from a bare integer. The one exception
is a failure before the session record exists at all (an unknown model, a missing
credential) — stdout is empty by design there, and the reason is on stderr, mapped to
exit code 2 (usage) or 4 (harness) below.

### Exit codes

| code | meaning |
|---|---|
| 0 | success — the model replied in prose, asking for no tool |
| 1 | the task was not completed — the loop ran cleanly and did not finish (cancelled, budget exhausted, or the project's own verification rejected the tree) |
| 2 | usage error — an unknown model/harness, a bad flag, a malformed `--policy-file`; nothing was opened, locked, or written |
| 3 | provider error — a call that produced no usable reply after the client's own retries |
| 4 | harness error — kopicode itself broke, and the fail-closed default for any outcome nobody mapped |

### `session_ended.reason` values

`reason` is finer than the exit code alone — two distinct stops both map to exit 3/4's
"error" family the same way exit 1 covers three distinct stops. This table is the full,
stable vocabulary (mirrors `internal/engine/stop.go`'s `Stop` enumeration, which is the
source of truth if the two ever disagree):

| `reason` | `exit_code` | meaning |
|---|---|---|
| `completed` | 0 | the model replied in prose, asking for no tool |
| `cancelled` | 1 | the run's context was cancelled (Ctrl-C, a caller's timeout) |
| `verification_failed` | 1 | the model declared the task done over a tree the project's own verification command rejects |
| `budget_exhausted` | 1 | the token budget ran out |
| `max_turns` | 4 | the turn cap was reached |
| `error` | 3 | a provider call that produced no usable reply, after the client's own retries |
| `error` | 4 | kopicode itself broke: a journal write refused, a dispatcher/catalogue mismatch, a tool result that could not be placed in the conversation |

No other value is ever emitted. A future `Stop` value not in this table is a
documentation bug, not a value a consumer needs to plan for defensively — but if one is
ever seen, treat it as harness-bucket (exit 4) territory until this file is updated.

### `session_ended.text` carries the real failure, not just its label

**This is the field that answers "what actually happened," and it is easy to miss**:
`reason` and `exit_code` are a classification, but the *diagnostic* — the provider's
HTTP status and response body, the verification command that rejected the tree, the
harness error's own message — is on `session_ended`'s `text` field, present whenever the
stop is not a plain `completed`. This is the same value `run --print` also writes to
stderr as a one-line summary, so a consumer does not need `--debug` or a separate
stderr capture to get it — it is already on the NDJSON stream a `--print` consumer is
already parsing.

An expired API key surfacing as an HTTP 401, for example, ends the stream like this:

```json
{"kind":"session_ended","seq":4,"reason":"error","exit_code":3,"text":"engine: provider call turn 1 attempt 1: provider: http 401: {\"error\":{\"code\":401,\"message\":\"...\"}}","size":92}
```

`text` here is the wrapped error verbatim — provider status and body included — which
is enough to tell "the key expired" apart from "the provider is down" (a 5xx, after
retries) apart from "the model hit a tool error" (a `harness` reason with a different
`text`), without re-running the session with `--debug` and capturing stderr by hand.

A `verification_failed` stop's `text` is a one-line summary (`` `go test ./...` exited 1
``); the full command output is on the earlier `verification` event in the same stream,
not repeated here.

## Compatibility

The schema is versioned from its first commit because a headless surface is a
compatibility surface the moment anything reads it — `internal/bench` and `kopicode
serve` (see [`kopicode-serve-protocol.md`](kopicode-serve-protocol.md), which reuses
this exact per-event projection) are both readers today. Adding a field is not a
version change; changing what an existing one means is.
