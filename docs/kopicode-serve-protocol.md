# The `kopicode serve` protocol

- **Surface:** [`cmd/kopicode/serve.go`](../cmd/kopicode/serve.go)
- **Cards:** KAN-1027–1031 (EPIC-131)
- **Rule it satisfies:** [ADR-0013](adr/0013-agent-controlled-resident-session-surface.md)
  (decisions 1–6), reusing [ADR-0011](adr/0011-unattended-invocation-policy-gate.md)'s
  policy gate and [ADR-0009](adr/0009-ask-tool-contract.md)'s ask contract

`kopicode serve` is the resident session surface: a fourth rendering of the one engine,
for an orchestrator that spawns kopicode as a child process and drives it over stdio,
holding one process open across a sequence of tasks rather than paying process-startup
cost per task. The orchestrator owns the child's stdin/stdout, so the OS's own
process-ownership model is the access control and there is nothing to authenticate.

This file documents the wire it speaks. The register is [`provider-pin.md`](provider-pin.md)'s:
the ADR is the decision of record and does not change; this describes the concrete
message shapes a client sends and receives, which are operational. Where the two
disagree, the code is the truth — the serve surface's own doc comment and its tests are
closer to it than this page.

## Transport and framing

Newline-delimited JSON-RPC 2.0. **One JSON message per line**, on the child's stdin
(client → server) and stdout (server → client). This reuses `run --print`'s NDJSON
discipline rather than adding LSP's `Content-Length` framing — the same binary should
not carry two incompatible conventions. `stderr` carries only diagnostics (`--debug`),
never protocol.

Every message the server emits carries `"jsonrpc": "2.0"`, and a client is expected to
send the same; it is the only version this surface speaks. A line that will not parse gets
a parse-error response and the loop continues — one bad message does not end the process.

```
--> {"jsonrpc":"2.0","id":1,"method":"session.start","params":{"session":"s1","dir":"/repo","prompt":"fix the failing test"}}
<-- {"jsonrpc":"2.0","method":"session.event","params":{"session":"s1","event":{"kind":"session_started", ...}}}
<-- {"jsonrpc":"2.0","method":"session.event","params":{"session":"s1","event":{"kind":"assistant_message", ...}}}
<-- {"jsonrpc":"2.0","id":1,"result":{"session":"s1","record":"/repo/.kopicode/sessions/s1","stop":"completed","exit_code":0,"turns":1}}
```

## Message shapes

**Request** (client → server). `id` is echoed back verbatim on the response so the client
can match them; `params` is method-specific.

```json
{ "jsonrpc": "2.0", "id": 1, "method": "session.start", "params": { ... } }
```

**Response** (server → client). Exactly one of `result` and `error` is present. The `id`
is the request's; for an error that could not be tied to any request (an unparseable
line) it is JSON `null`.

```json
{ "jsonrpc": "2.0", "id": 1, "result": { ... } }
{ "jsonrpc": "2.0", "id": 1, "error": { "code": -32002, "message": "..." } }
```

**Notification** (server → client). No `id`, and no reply is expected. The only
notification this surface emits is `session.event` (below).

```json
{ "jsonrpc": "2.0", "method": "session.event", "params": { ... } }
```

## The three methods

The set is fixed at ADR-0013 decision 3's three: there is no `session.close` (a session
lives until the process shuts down), no listing call, and no credential in any params.

### `session.start`

Opens a session on a working tree, keyed by the caller-supplied `session` id, and runs
its first turn.

| param | type | required | meaning |
|---|---|---|---|
| `session` | string | yes | the id to key the session by; the client's own, so it can cancel a turn whose start has not yet returned |
| `dir` | string | yes | the working tree to run in |
| `prompt` | string | yes | the first turn's task |
| `model` | string | no | model-id override (else the repo config / built-in default) |
| `harness` | string | no | harness-config override |

**Result** (`turnResult`): the turn's outcome, projected the way `run --print`'s last line
is.

| field | type | meaning |
|---|---|---|
| `session` | string | the session id |
| `record` | string | the journal directory this session opened — **start only** |
| `stop` | string | why the turn stopped (see [Stops](#stops)) |
| `exit_code` | number | the process exit code that stop maps to |
| `turns` | number | how many turns this exchange used |

### `session.submit`

Runs the next turn on an already-open session. It **queues** rather than rejecting: if a
turn is already in flight for the session, this one waits its place in that session's FIFO
queue and runs when the ones before it finish. Same-session turns therefore serialize;
different sessions run concurrently.

| param | type | required | meaning |
|---|---|---|---|
| `session` | string | yes | an open session's id |
| `prompt` | string | yes | the next turn's task |

**Result**: the same `turnResult` as `session.start`, without `record` (only start opens
the journal).

### `session.cancel`

Cancels the session's **in-flight** turn — the identical mechanism the REPL's Ctrl-C
drives. It targets the turn running now and only that one: a turn still queued behind it
keeps its place and runs later. It does **not** end the session, which stays open for
further submits. A session that is idle between turns has nothing to cancel; the signal is
still acknowledged.

| param | type | required | meaning |
|---|---|---|---|
| `session` | string | yes | the session whose in-flight turn to cancel |

**Result** (`cancelResult`): `{ "session": "...", "cancelled": true }`. This says the
signal was delivered; the cancelled turn reports its own `stop: "cancelled"` through its
own `session.start`/`session.submit` response.

## The `session.event` notification

Every non-delta journal event a session produces is teed to the client as a
`session.event` notification, tagged with the session id. This is how the client watches a
turn as it runs.

```json
{ "jsonrpc": "2.0", "method": "session.event",
  "params": { "session": "s1", "event": { "kind": "assistant_message", ... } } }
```

- `event` is the **same per-event record projection `run --print` emits** (schema 1). The
  event vocabulary — `session_started`, `assistant_message`, tool calls and results,
  verification, `session_ended`, and the rest — is `run --print`'s; a serve client and a
  `--print` consumer read one event language.
- Streaming text deltas are dropped: they are not in the record, and this stream carries
  only what the record holds. The reconciled `assistant_message` is emitted when the turn
  settles.
- `session_ended` for every open session is emitted at shutdown (stdin close), the
  record's other bookend.

The notification stream is the journal's projection, not a second transcript: everything
here is derived from journal events the engine already appended.

## Stops

`stop` is the turn's outcome; `exit_code` is what that stop maps to (the same five codes
`run --print` exits, from [SLICE-1](SLICE-1.md) build step 14).

| `stop` | `exit_code` | meaning |
|---|---|---|
| `completed` | 0 | the model replied in prose, asking for no tool |
| `cancelled` | 1 | the turn's context was cancelled (`session.cancel`, shutdown) |
| `verification_failed` | 1 | the model stopped over a tree its verification command rejects |
| `budget_exhausted` | 1 | the token budget ran out |
| `max_turns` | 4 | the turn cap was hit |
| `error` | 3 | a provider error that survived the client's retries |
| `error` | 4 | a harness error |

## Error codes

The first four are JSON-RPC's reserved values; the server-defined ones sit in the reserved
`-32000..-32099` range so a client can branch on the code rather than parse the message.

| code | name | when |
|---|---|---|
| -32700 | parse error | a line that is not valid JSON |
| -32600 | invalid request | a message with no method |
| -32601 | method not found | a method outside the three above |
| -32602 | invalid params | a method's params are missing or malformed |
| -32000 | unknown session | `session.submit`/`session.cancel` named an id with no open session |
| -32001 | session exists | `session.start` named an id already open in this process |
| -32002 | open failed | `engine.Open` refused: a bad model, a missing credential |
| -32003 | usage error | the arm could not be resolved (an unknown model or harness) |
| -32005 | session locked | `session.start`'s `dir` is already held by another live session |

`-32004` is retired: it was `session busy`, a `session.submit` while a turn was in flight,
which the per-session queue (KAN-1030) replaced — a submit now queues instead of failing.

The session-locked case is [`internal/lock`](../internal/lock)'s one-session-per-working-tree
rule (`engine.ErrSessionLocked`): a second session on a tree another live session holds is
refused here, promptly, rather than blocking. Two sessions on **different** trees never
collide, which is what lets an orchestrator run many at once.

## Concurrency model

One worker goroutine per session drains that session's FIFO queue, so a session's turns
serialize — they can never race the engine's context assembler, which belongs to one loop
— while different sessions' workers run truly concurrently. `engine.Open` runs inline on
the read loop (a fast, assembler-free local step), which keeps the read loop free during
the turns themselves and is what makes a turn cancellable while it runs.

## Flags, credentials, and consent

`serve` takes no positional arguments — a task arrives over the wire as `session.start`'s
prompt, never on the command line. All flags are process-level and apply to every session
the process opens.

| flag | meaning |
|---|---|
| `--policy-file <path>` | an ADR-0011 declared-allowlist policy governing shell/write consent; unset means refuse everything |
| `--ask-policy-file <path>` | an ADR-0013/KAN-1028 ask-policy file whose note answers the model's `ask` calls; unset means the fixed "no human is present" refusal |
| `--debug` | engine diagnostics on stderr (off by default) |

- **Credentials** are read once from the process's environment (`OPENROUTER_API_KEY`), by
  `engine.Open` exactly as every other surface reads it. The wire carries no credential
  and no per-session override.
- **Consent** is unattended: with no `--policy-file`, serve refuses shell and writes; with
  no `--ask-policy-file`, it dead-ends `ask` — the same fail-closed defaults headless
  `run --print` uses. Real containment of model-authored shell is the orchestrator's job,
  not serve's (ADR-0008 / ADR-0011).
