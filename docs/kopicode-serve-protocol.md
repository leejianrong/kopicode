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

**Request, server → client** (ADR-0016). The one message shape this surface did not
originally need: the server itself mints an id, sends a request, and blocks the turn
until a matching reply arrives on stdin or a bounded timeout elapses. `consent.request`
(below) is the only method sent this way today.

```json
{ "jsonrpc": "2.0", "id": "c-1", "method": "consent.request", "params": { ... } }
```

The client answers it exactly as it would any request it received, on stdin, echoing
the id verbatim:

```json
{ "jsonrpc": "2.0", "id": "c-1", "result": { "answer": "allow" } }
```

A reply carries no `method` — the same shape any JSON-RPC response has — which is how
the read loop tells a reply apart from a malformed client-to-server request: a
method-less line carrying a `result` or an `error` is read as a reply and routed to
whatever is waiting on that id; one carrying neither is the ordinary "request has no
method" refusal.

## Usage (ADR-0021)

How much a session has used, so a client can show spend, enforce a ceiling, or checkpoint a
long-running session before the model's window fills. The same object appears three places:
as `usage` on every turn result, as `session.usage`'s result, and (one response's worth,
split) as `usage` on each `provider_response` event.

```json
<-- { "jsonrpc": "2.0", "id": 7, "result": { "session": "s1", "usage": {
      "context_tokens": 31204, "context_window": 262144,
      "prompt": 301200, "completion": 10800, "total": 312000, "cache_read": 120000,
      "cost_usd": 0.0123, "requests": 12, "turns": 12, "max_turns": 20, "token_budget": 2000000 } } }
```

| field | meaning |
|---|---|
| `context_tokens` | the prompt tokens of the **latest** request: what the model is holding now, and so how near its window is. `0` until the first response (`requests` is `0`) |
| `context_window` | the model's context size in tokens. **Absent** when this binary does not know it — never estimated. A client that wants a percentage computes it only when present |
| `prompt`, `completion`, `total` | cumulative over every request this process made for the session. `total` is what the token budget counts and grows much faster than `context_tokens`, because each request resends the whole history. Do not read `total` as "how full is the context" |
| `cache_read`, `cache_write` | subsets of `prompt` served from / written to the provider's cache. Absent when the route reports none |
| `cost_usd` | the sum of costs the provider reported, **present only when every request reported one**. Never computed from a price table; one request with no reported cost makes the whole sum absent, not partial |
| `requests` | provider responses that reported usage |
| `turns` | the session-wide turn number of the latest response |
| `max_turns` | the cap on one prompt's turns (it resets with each `session.submit`), from the harness configuration |
| `token_budget` | the cap on the whole session's `total` |

A `provider_response` event's `usage` is one response: `prompt`, `completion`, `total`, and
`cache_read`, `cache_write` and `cost_usd` when reported. `size` on the same event stays the total.

`session.usage` takes `{ "session": "s1" }` and answers immediately, **including while a turn is
running** (it is read inline like `session.cancel`, and never queues behind the turn). An id with no
open session is `-32000`, a missing one `-32602`. After a process restart a resumed session's numbers
restart from the first new response: they count this process's requests.

Features: `session.usage`, `usage.tokens_split`, `usage.context`, `usage.context_window`,
`usage.cost`.

## The five methods

ADR-0013 decision 3 fixed three; `session.close` (KAN-1795) is the fourth and `session.handoff` (ADR-0026) the fifth. There is
still no listing call and no credential in any params. A session lives until it is
closed with `session.close` or the process shuts down.

### `session.start`

Opens a session on a working tree, keyed by the caller-supplied `session` id, and runs
its first turn.

| param | type | required | meaning |
|---|---|---|---|
| `session` | string | yes | the id to key the session by; the client's own, so it can cancel a turn whose start has not yet returned |
| `dir` | string | yes | the working tree to run in |
| `prompt` | string | yes | the first turn's task |
| `model` | string | no | model-id override (else the repo config / built-in default) |
| `harness` | string | no | built-in harness-config name override |
| `harness_config` | string | no | path to a declared harness-config file (ADR-0010), the same axis as `harness`; a relative path resolves against `dir`. Passing both `harness` and `harness_config` is a usage error |
| `consent_mode` | string | **yes** | `"remote_interactive"`, `"unattended_policy"` (ADR-0016) or `"auto"` (ADR-0017) — see [Consent modes](#consent-modes). There is no default; omitting it is a usage error |
| `containment_provided` | boolean | iff `consent_mode` is `"unattended_policy"` | the caller's explicit acknowledgment that it supplies real process/container containment for this session (ADR-0011 decision 4). Required and must be `true` for that mode; ignored for the other two |
| `consent_timeout` | string | no | how long this session's `consent.request` waits for an answer before it is denied, as a Go duration (`"5m"`), within `1s` to `24h`; overrides `--consent-timeout` for this session only. Only meaningful under `"remote_interactive"` — sending it under any other mode is a usage error |
| `ask_mode` | string | no | `"remote"` (ADR-0020) puts the model's `ask` tool to the client live as an [`ask.request`](#askrequest-server--client-adr-0020) instead of the process `--ask-policy-file` or the fixed "no human is present" refusal. Needs `consent_mode: "remote_interactive"`; any other mode, or any other value, is a usage error. Omit it for today's behaviour |
| `max_turns` | integer | no | turns one prompt may take before the session stops with `max_turns` (ADR-0022). Positive (zero and below are a usage error, not "unset"); omit for the harness default of 20. The count restarts at every prompt (`session.submit`). Moves the session's `harness_config_hash` |
| `token_budget` | integer | no | tokens (prompt + completion) the **whole session** may spend before it stops with `budget_exhausted`; `0` is unbounded, omit for the default of 2,000,000. Never resets, so a long-lived session must raise it or restart. Moves the hash |
| `read_only` | boolean | no | refuse every file write for this session (ADR-0019): `write_file` and `edit_file` are denied by the gate itself, inside the root or outside it, in every mode, and the client is never asked. Shell is **not** made read-only — it stays governed by `consent_mode` — so this is not a sandbox. A usage error under `"auto"`, which runs shell unasked |
| `handoff` | string | no | a handoff document (ADR-0026), as [`session.handoff`](#sessionhandoff) returned it, to start from. The session is told it once, as its first message, under a `<handoff>` tag; its journal records it (a `project_instructions_loaded` with scope `handoff`) and its verification starts as not run whatever the document says. Feature `session.handoff` |
| `handoff_from` | string | no | with `handoff`: the id of the session the document came from, recorded as `parent_session` on this session's `session_started`. Ignored without `handoff` |
| `never_allow` | array of strings | no | extra never-allow entries for `consent_mode: "auto"` (ADR-0017), each `"command [token ...]"`; see [`"auto"`](#auto-adr-0017). Adds to the built-in list, never removes from it. Sending it under any other mode is a usage error |

**Result** (`turnResult`): the turn's outcome, projected the way `run --print`'s last line
is.

| field | type | meaning |
|---|---|---|
| `session` | string | the session id |
| `record` | string | the journal directory this session opened — **start only** |
| `stop` | string | why the turn stopped (see [Stops](#stops)) |
| `exit_code` | number | the process exit code that stop maps to |
| `turns` | number | how many turns this exchange used |
| `usage` | object | the session's usage as the turn settled — see [Usage](#usage-adr-0021) |

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

### `session.handoff`

Drafts a handoff document ([ADR-0026](adr/0026-handoff.md)) for an open session: **one model
call with no tools**, made from the session's own history, then a facts block kopicode writes
from the journal (files written and deleted, the last verification, denied calls, usage). It
queues behind the turns the session has already accepted, like `session.submit`, and
`session.cancel` reaches it the same way. It **does not end the session** and does not touch its
conversation; the call's spend counts toward the session's token budget. The document is written to
`.kopicode/handoff/<session>.md` on the server's disk and journaled as `handoff_written`.

To continue the work, close the session (or not) and `session.start` another with
`handoff` set to the returned `document`. **When to hand off is the caller's policy**: kopicode
never does it on its own.

| param | type | required | meaning |
|---|---|---|---|
| `session` | string | yes | an open session's id |
| `goal` | string | no | what the next session is for, in the caller's words; it steers the draft and is recorded |

**Result**: `{ "session": "...", "goal": "...", "document": "...", "path": "..." }`; `goal` is
absent when none was given. A session that has run no turn answers `-32003`; a draft that fails
(the provider is down, the call was cancelled) answers `-32603`.

Feature: `session.handoff`, which covers this method and `session.start`'s `handoff` and
`handoff_from`.

### `session.close`

Ends **one** session while the process stays up. The close queues behind any turns the
session has already accepted, so they run to completion and answer first (send
`session.cancel` first to end it immediately). Once accepted, a further `session.submit`
to that session is refused with `-32000`. The session's `session_ended` is then written
and announced as a `session.event` — its `text` carries the failure detail for a turn that
did not complete cleanly — the working-tree lock is released, and the id is free to
`session.start` again. The response follows the announcement.

| param | type | required | meaning |
|---|---|---|---|
| `session` | string | yes | an open session's id |

**Result**: `{ "session": "...", "closed": true }`.

## `server.hello`

Asks what this binary supports, so a client can require a minimum without scraping
`kopicode serve --help`. No params. The result is the same object `kopicode version --json`
prints:

```json
--> { "jsonrpc": "2.0", "id": 1, "method": "server.hello" }
<-- { "jsonrpc": "2.0", "id": 1, "result": { "version": "v0.3.0", "commit": "…", "tree_state": "clean",
      "source": "ldflags", "protocol": 1, "features": ["allow_commands", "consent_mode.auto", …] } }
```

`version` is a git describe for humans and must not be parsed; `tree_state` is the machine-readable
dirty bit. `protocol` moves only for a change that breaks an existing client. `features` is a sorted
list of stable lower-case dotted names, added in the change that ships a capability, never renamed,
and removed only with a protocol bump. Current names: `allow_commands`, `ask.request`, `consent.note`, `consent_mode.auto`,
`consent_mode.remote_interactive`, `consent_mode.unattended_policy`, `consent_request.command`,
`consent_timeout.flag`, `consent_timeout.session`, `mcp`, `provider_url.flag`, `server.hello`, `session.close`,
`session.handoff`, `session.limits`, `session.read_only`, `session.usage`, `usage.context`, `usage.context_window`, `usage.cost`,
`usage.tokens_split`.
`cmd/kopicode/capabilities_test.go` ties the list to the consent modes and methods in the code.

`provider_url.flag` (ADR-0027) means `kopicode serve --provider-url URL` and `kopicode mcp --provider-url URL`
are accepted. Every session the process opens then sends its requests to that OpenAI-compatible endpoint
instead of OpenRouter, and each `session.start` must name a `model` the endpoint serves. The credential is
`KOPICODE_PROVIDER_API_KEY` (optional for a loopback host); `OPENROUTER_API_KEY` is never sent to it. Such
sessions are unpinned, their `session_started` event carries `provider_host`, and they report no context
window, so `usage.context_window` is absent for them.

## `ask.request` (server → client, ADR-0020)

Sent only for a session started with `ask_mode: "remote"`. The model called its `ask` tool; the
question is relayed so a person can answer it, and the answer goes back to the model mid-turn.

```json
--> { "jsonrpc": "2.0", "id": "c-3", "method": "ask.request",
      "params": { "session": "s1", "question": "tabs or spaces?", "context": "gofmt is not set up" } }
<-- { "jsonrpc": "2.0", "id": "c-3", "result": { "text": "tabs" } }
```

`question` and `context` are model output and reach a person: untrusted, as `detail` is. `text` may
be the empty string (a person who had nothing to add); a reply with no `text`, or with an `error`,
counts as unanswered. The wait is bounded by the session's consent timeout (`--consent-timeout`, or
`session.start`'s `consent_timeout`). **Expiry is not a denial**: the model is told "no human is present
to answer this question", the headless refusal, journalled as refused, and the turn goes on. An answer is
journalled with `source: "remote"`.

## `consent.request` (server → client, ADR-0016)

Sent only for a session whose `consent_mode` is `"remote_interactive"`. Every
permission-requiring decision the engine would otherwise need a declared policy to
answer instead bubbles live to the client, one request per action, with nothing
declared ahead of time.

```json
--> { "jsonrpc": "2.0", "id": "c-1", "method": "consent.request",
      "params": { "session": "s1", "kind": "run_shell", "tool": "run_shell",
                  "detail": "/bin/sh -c uv run pytest -v", "reason": "", "resolved": "" } }
<-- { "jsonrpc": "2.0", "id": "c-1", "result": { "answer": "allow" } }
```

| param | type | meaning |
|---|---|---|
| `session` | string | which session is asking |
| `kind` | string | `run_shell` or `write_outside_root` |
| `tool` | string | the tool name as the model called it |
| `detail` | string | what is being consented to: for `run_shell`, the argv the engine will run joined by single spaces, which is always `/bin/sh -c <line>`; for `write_outside_root`, the absolute path |
| `reason` | string | why the gate is asking, when the policy layer has one to give |
| `resolved` | string | for a write, the path after symlink resolution, when it differs from `detail` |
| `command` | string | for `run_shell` whose argv is `/bin/sh -c <line>`: the exact `<line>`, no prefix and no joining. Absent otherwise |
| `argv` | array of strings | for a shell action, the exact argv, element for element. Absent for a write |

A client should match and display `command` (or `argv`), not parse `detail`; `detail` stays
for compatibility.

For `run_shell`, `detail` is **not** the bare command line: the engine runs every shell
command as `/bin/sh -c <line>` and puts that whole argv here, so the line the model wrote
follows the `/bin/sh -c ` prefix. The words are joined by a single space with no quoting, so
the join says nothing about where the line's own spaces were. A client that matches commands
should strip that exact prefix once and treat the rest as untrusted model output, and deny a
`detail` that does not start with it. (Found by a live consent client whose policy matched
the bare line and so denied every real command.)

A reply may also carry `result.note` (feature `consent.note`): free text saying what to do
instead, meaningful only with `"deny"`. The model reads it in the denial ("permission denied: …; the user
suggests instead: use a venv and uv"), and the journal records it as `PermissionDecided.note`. It is
ignored with `"allow"` and `"allow_session"`, and absent means a bare refusal. It is relayed text from
whoever answers: treat it as untrusted wherever you display it, as `detail` is.

```json
<-- { "jsonrpc": "2.0", "id": "c-1", "result": { "answer": "deny", "note": "use uv and a venv" } }
```

`result.answer` is one of `"allow"`, `"allow_session"`, `"deny"` —
`engine.ConsentAnswer`'s three values spelled out as text, the same way this protocol
already renders `decision`, `source` and `stop`. A reply with no matching id (a race
against a timeout, or a stray line) is dropped silently rather than reported as an
error: the message was well-formed, it simply arrived for a question that had already
settled one way or another.

**A request that goes unanswered denies, after 60 seconds by default**, and is never treated
as though granted (ADR-0016 decision 5). `--consent-timeout <duration>` (for example `5m`)
changes it for the process, between 1s and 24h; zero or unbounded is refused, because a bound
is the point. A denial this way is attributed exactly like
an explicit `"deny"` reply — see [Consent modes](#consent-modes) on `Source`.

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
- `session_ended` is emitted when a session is closed with `session.close`, and for every
  session still open at shutdown (stdin close), the
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
| `budget_exhausted` | 1 | the session's cumulative token budget ran out; every later prompt stops the same way, so start a new session (resume it, or pass a larger `token_budget`) |
| `max_turns` | 4 | the turn cap was hit; the session is intact, so send another prompt to continue (the count restarts) |
| `error` | 3 | a provider error that survived the client's retries |
| `error` | 4 | a harness error |

## Error codes

The first four are JSON-RPC's reserved values; the server-defined ones sit in the reserved
`-32000..-32099` range so a client can branch on the code rather than parse the message.

| code | name | when |
|---|---|---|
| -32700 | parse error | a line that is not valid JSON |
| -32600 | invalid request | a message with no method |
| -32601 | method not found | a method outside the five above |
| -32602 | invalid params | a method's params are missing or malformed |
| -32000 | unknown session | `session.submit`/`session.cancel`/`session.handoff`/`session.close` named an id with no open session, or `session.submit` named one whose close is already accepted |
| -32001 | session exists | `session.start` named an id already open in this process |
| -32002 | open failed | `engine.Open` refused: a bad model, a missing credential |
| -32003 | usage error | the arm could not be resolved (an unknown model or harness); or `session.start`'s `consent_mode` is missing/unrecognised, or `"unattended_policy"` is requested without `containment_provided: true` (ADR-0016); or `never_allow` is sent without `consent_mode: "auto"`, or holds a malformed entry (ADR-0017); or `session.handoff` named a session that has run no turn |
| -32603 | internal error | `session.close` could not write the session's `session_ended`; or `session.handoff`'s draft failed |
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

## Flags and credentials

`serve` takes no positional arguments — a task arrives over the wire as `session.start`'s
prompt, never on the command line.

| flag | meaning |
|---|---|
| `--policy-file <path>` | an ADR-0011 declared-allowlist policy; content for any session that declares `consent_mode: "unattended_policy"` (below). Unset means refuse every shell command and write outside `dir` under that mode |
| `--consent-timeout <duration>` | how long a `consent.request` waits for its answer before it is denied; default `60s`, between `1s` and `24h`. A session can override it with `session.start`'s `consent_timeout` |
| `--ask-policy-file <path>` | an ADR-0013/KAN-1028 ask-policy file whose note answers the model's `ask` calls, for every session regardless of `consent_mode`. Unset means the fixed "no human is present" refusal |
| `--debug` | engine diagnostics on stderr (off by default) |

**Credentials** are read once from the process's environment (`OPENROUTER_API_KEY`), by
`engine.Open` exactly as every other surface reads it. The wire carries no credential and
no per-session override.

`ask` is untouched by ADR-0016: it stays process-level and identical under both consent
modes below. Only the permission gate — `run_shell` and a write outside `dir` — is what
`consent_mode` decides between.

## Consent modes

Every `session.start` must declare `consent_mode`. There is no default that grants
capability (the same posture ADR-0011 decision 3 already held this surface to): an
unconfigured invocation refuses everything, and a caller has to say explicitly which of
the three mechanisms below it wants.

### `"unattended_policy"` (ADR-0011)

The process's `--policy-file` answers every permission check for this session, exactly
as before ADR-0016 existed. The session.start call must also carry
`containment_provided: true` — an explicit acknowledgment that the caller is supplying
real process/container containment; kopicode never does this itself (ADR-0011 decision
4). Omitting the acknowledgment is a usage error, not a silent proceed.

A `--policy-file` has a `root` (the absolute directory writes are confined to) and one or both
of two allow keys.

**`allow_commands`** (ADR-0018) is what to reach for. Each entry is a command name and its
leading arguments, matched on whole tokens, and a command line is permitted only if *every*
command in it matches:

```toml
root = "/abs/path/to/the/repository"
allow_commands = [["uv", "run", "pytest"], ["git", "status"], ["ls"], ["head"]]
```

`cd /repo && uv run pytest -v 2>&1 | head -100` is permitted by that file; `uv run pytest; rm -rf x`,
`uv run pytest | tee f` and `ls $(make)` are not, because `rm`, `tee` and `make` are not listed.
`cd` inside `root`, a redirect that writes inside `root` or to `/dev/null`, and `2>&1` need no
entry; the filters a model pipes into (`head`, `tail`, `grep`) do. The never-allow rules of
[`"auto"`](#auto-adr-0017) still apply on top, so an entry can narrow what runs but never widen it
past them. An entry that starts other commands (`env`, `xargs`, `sh`, `eval`, `sudo`, `find`, …)
is refused when the file loads. An entry is a statement that you trust that command with whatever
arguments it is given: `["python"]` permits `python -c`, so write the narrowest prefix that serves.

**`allow`** is the original exact-match set, for a caller that knows the precise line it will
issue. `run_shell` executes `/bin/sh -c <command-line>`, so an entry is written in exactly that
argv shape and matched byte-for-byte; it is the right tool for a line that must not vary, and the
only way to permit one of the never-allow commands:

```toml
root = "/abs/path/to/the/repository"
allow = [["/bin/sh", "-c", "go test ./..."]]
```

`[["go", "test", "./..."]]` never matches there, and any variation the model emits (an added flag, a
`cd sub &&` prefix) is a different line and is refused. Prefix-matching the *characters* of the line
is something the project has rejected four times (see
[ADR-0016](adr/0016-live-remote-consent-for-agent-orchestrated-sessions.md)'s Context, and
[ADR-0018](adr/0018-tokenized-allowlist.md) for why tokens succeed where characters cannot). For
open-ended work where the commands cannot be known in advance, `"auto"` or `"remote_interactive"`
is the better fit.

Every resulting `permission_decided` event is attributed `source: "policy"`.

### `"remote_interactive"` (ADR-0016)

Nothing is declared ahead of time. Every permission-requiring decision bubbles live to
this client, per action, as a `consent.request` (above), and the client answers each one
as it arrives. This is the mode for open-ended work, where an orchestrator cannot know
in advance every command a model will phrase.

Every resulting `permission_decided` event is attributed `source: "remote"` — never
`"user"` or `"policy"`, because kopicode cannot verify whether a human or another model
answered on the far end of the channel.

### `"auto"` (ADR-0017)

For a developer running an agent on their own repository who does not want to answer
every command. No `consent.request` is ever sent. The harness answers each permission
check itself, from a fixed rule, and a session that declares it has said it accepts that
rule: it is never a default, and a `session.start` without `consent_mode` is still refused.

- A `run_shell` whose working directory is inside `dir`, and which matches nothing on the
  never-allow list below, is **allowed**.
- A match is **denied** with a reason naming the rule, and is not asked about. The model
  is told the refusal will not change on retry.
- Every write outside `dir` is **denied**. (A file tool targeting a path outside `dir` is
  the other thing the gate asks about; auto mode has no answer to that but no.)

Every resulting `permission_decided` event is attributed `source: "auto"` — never `"user"`
(nobody was asked), never `"policy"` (no caller-declared rule matched), never `"remote"`.
Every allowed command gets its own event; auto mode never answers `allow_session`.

**The built-in never-allow list.** The command line (`/bin/sh -c <line>`) is tokenized and
every command in it is checked, including inside `;`, `&&`, `||`, `|`, newlines,
`( … )`, `$( … )`, backticks, `<( … )`, `sh -c '…'` and `eval '…'`, and behind `env`,
`nice`, `timeout`, `xargs`, `find -exec` and the like:

| Rule | Refuses |
| --- | --- |
| privilege escalation | `sudo`, `doas`, `su`, `pkexec`, `runuser` |
| `rm` | any target outside `dir` (after `cd`, `..`, symlinks and `~`); a recursive `rm` of `dir` itself; a recursive `rm` whose target is computed at run time; `--no-preserve-root` |
| forced push | `git push` with `--force`, `-f` (also inside a flag cluster such as `-fu`), `--force-with-lease`, `--force-if-includes`, `--mirror`, a `+refspec`, or an argument computed at run time |
| download into a shell | `curl`/`wget`/`fetch`/`aria2c` piped to `sh`, `bash`, `zsh`, `dash`, … ; `sh <(curl …)`; `bash -c "$(curl …)"`; `eval`/`source` of a download |
| global or destructive installs and git (ADR-0023) | `pip`/`pip3`/`python -m pip install` outside a virtualenv (a `…/bin/pip` or `…/bin/python` path, or an `activate` sourced earlier in the same line, counts as inside one) and `pip install --user` anywhere; `npm`/`pnpm`/`yarn`/`bun` with `-g`/`--global`/`global`; `git reset --hard`; `git clean -f`; `git checkout .`, `git checkout -- .`, `git restore .`; `git commit --no-verify` (or `-n`) and `git push --no-verify` |
| permissions | `chmod -R 777`/`a+rwx`; a recursive `chmod`, `chown` or `chgrp` whose target is outside `dir` or computed at run time |
| redirection outside `dir` | `>`, `>>`, `&>`, `2>` and friends to a path outside `dir`, or computed at run time (`/dev/null`, `/dev/stdout`, `/dev/stderr` are allowed) |

**Every refusal from ADR-0023 names what to do instead** (`uv add <pkg>`, a project venv, `git stash`,
fix what the hook reports), because a model that is only told no tends to retry in a neighbouring
spelling. **This list grew in v0.4: a cuttlefish session that relied on `consent_mode: "auto"` and, for
example, ran a bare `pip install` or `git commit --no-verify` is now refused.** The refusal is
the ordinary auto denial, so nothing on the wire changes.

**It fails closed.** A command line the tokenizer cannot account for is denied, not
guessed at: an unterminated quote or substitution, a heredoc or here-string, a command
whose name is computed at run time (`$CMD`, `$'\x73udo'`), `eval` or `sh -c` of a computed
string, a line nested deeper than 8 levels, or one too large to analyse. The reason says
which. A model that hits one should use a file tool (`write_file` instead of a heredoc) or
spell the command out.

**Extending it.** `never_allow` entries are `"command [token ...]"`: they match a command
whose name (by basename) is the first token and whose arguments contain the remaining
tokens in order, so `"terraform apply"` also matches `terraform -chdir=infra apply` and
`env terraform apply`. A word computed at run time matches any token, since it could be that
token. An entry holding shell syntax (`; & | < > ( ) $ \` quotes) is a usage error. At most
64 entries of 200 bytes. Entries only add; nothing removes or narrows a built-in rule.

**What it is not.** It judges the request the model wrote, not what the process then does.
A command it allows runs with the full authority of the user who started kopicode:
`python -c`, `make`, a script already in the tree, `find … -delete`, `cp`/`mv`/`tee` into
another directory, an alias or `git -c alias.…`, or a download saved to a file and run in
a second command are all outside what it can see. It is a guard against the obvious
catastrophes and against smuggling one in behind an innocuous first word, not a sandbox;
real containment is the caller's job (ADR-0011 decision 4). `--policy-file` does not apply to
an `"auto"` session.
