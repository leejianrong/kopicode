# `kopicode mcp` — kopicode as an MCP server

[ADR-0015](adr/0015-mcp-server-front-end.md) front end. `kopicode mcp` speaks the
[Model Context Protocol](https://modelcontextprotocol.io) over stdio, so an MCP-capable agent
(Claude Code, or any other) can run coding sessions with one line of configuration and no
bespoke client. It is a second protocol skin over the same session lifecycle `kopicode serve`
uses (`cmd/kopicode/session`); `serve`'s wire ([kopicode-serve-protocol.md](kopicode-serve-protocol.md))
is unchanged.

## Adding it to an agent

```bash
# Claude Code
claude mcp add kopicode -- kopicode mcp

# any MCP client that takes a command: command "kopicode", args ["mcp"]
```

The credential is read once from the process environment, as on every surface, so the
client must launch kopicode with `OPENROUTER_API_KEY` in it. If it does not pass yours through,
use its env option (Claude Code: `-e OPENROUTER_API_KEY=...`, which stores the key in that
client's config). The MCP wire
carries no credential and no per-session override.

`kopicode mcp` takes the same process-level flags as `serve`: `--policy-file`,
`--ask-policy-file`, `--consent-timeout`, `--debug`. A task never arrives on the command line; it arrives as a tool
call.

## Transport

One JSON-RPC 2.0 message per line on stdin/stdout, which is MCP's stdio binding. The
orchestrator owns the child's stdin and stdout; there is nothing to authenticate. Closing stdin
ends the process: every open session is cancelled and closed, and each record gets its closing
event. Diagnostics go to stderr only. Protocol revisions `2025-06-18`, `2025-03-26` and
`2024-11-05` are accepted; a client naming anything else is answered with the newest.

## Tools

| Tool | Does |
| --- | --- |
| `kopicode_start` | Opens a session in `dir` and runs `prompt` to completion. |
| `kopicode_submit` | Queues the next turn on an open session and waits for it. Never rejected as busy: a submit during a turn waits its place. |
| `kopicode_cancel` | Cancels the turn running now on a session, and only that one. |
| `kopicode_close` | Ends a session after the turns it has accepted, writes its closing event, releases its working tree so the id can be reused. |

`kopicode_start` arguments: `dir` and `prompt` and **`consent_mode`** (all required);
`containment_provided` (required, and true, for `unattended_policy`); `never_allow` (`auto`
only); `session` (generated as `mcp-<hex>` if omitted); `model`, `harness`, `harness_config`.
`consent_mode` is never defaulted — see [Consent](#consent). A misspelt argument is refused, not
ignored.

`kopicode_submit`: `session`, `prompt`. `kopicode_cancel` and `kopicode_close`: `session`.

### Results

`start` and `submit` block until the turn settles, as a `tools/call` must, and return the same
outcome `run --print`'s last line gives, as `structuredContent` and again as JSON text:

```json
{"session":"mcp-3f9a1c2b7d10","record":"/repo/.kopicode/sessions/…","stop":"completed","exit_code":0,"turns":3}
```

Each outcome also carries `usage` (context in use, tokens spent, provider-reported cost when every
request reported one; fields as in [serve's Usage section](kopicode-serve-protocol.md#usage-adr-0021)).

`record` is present only on the opening turn. A task that did not complete (`exit_code != 0`,
for example `stop: "cancelled"`) is an `isError` result: the call worked and the agent is told
how it ended. A caller's mistake (a missing `consent_mode`, an unknown session, a refused
`never_allow`) is also an `isError` result, with a message the agent can read and correct. Only
a malformed request or an unknown tool is a JSON-RPC protocol error.

## Events

When a `tools/call` carries `_meta.progressToken`, every journal event of the turn that call is
running is sent as a `notifications/progress`:

```json
{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"t1","progress":4,"message":"{\"kind\":\"tool_result\",…}"}}
```

`message` is the schema-1 `record` JSON that `run --print` and `serve`'s `session.event` emit —
no new event vocabulary and no second transcript. `progress` increases by one per event. Without
a token nothing is sent; the record on disk is identical either way, and is where to read a
session's closing event once no call is left to carry it.

## Cancellation

`notifications/cancelled` for a request id cancels the turn behind that call. Per the MCP spec the
cancelled request then gets **no response**; the session stays open for the next call.
`kopicode_cancel` does the same for a session by id and the waiting call reports the cancelled
stop.

## Consent

`kopicode_start` must declare `consent_mode`, exactly as `session.start` does on `serve`
([ADR-0016](adr/0016-live-remote-consent-for-agent-orchestrated-sessions.md),
[ADR-0017](adr/0017-auto-consent-mode.md)); omitting it is refused.

- **`auto`** — shell inside `dir` is allowed with no round trip, a fixed never-allow list (`sudo`,
  `rm` outside `dir`, forced `git push`, a download piped into a shell, writes outside `dir`) is
  refused with a reason, and `never_allow` adds to it. Not a sandbox. Details:
  [kopicode-serve-protocol.md § auto](kopicode-serve-protocol.md#auto-adr-0017).
- **`remote_interactive`** — each shell command or out-of-tree write is put to the client as an
  MCP **elicitation** (`elicitation/create`, with an `allow` / `allow_session` / `deny` choice),
  and the turn blocks on the answer for up to 60 seconds (`--consent-timeout`, 1s to 24h). A decline, a cancel, an error or a
  timeout all deny. Needs a client that declared the `elicitation` capability in `initialize`;
  `kopicode_start` refuses this mode otherwise. Decisions are journalled `source: "remote"`. The form
  also has an optional `note`: with `deny`, what the agent should do instead, shown to the model in
  the denial and journalled on the decision.
- **`unattended_policy`** — the process's `--policy-file` allowlist answers (ADR-0011); needs
  `containment_provided: true`. Journalled `source: "policy"`.

The `ask` tool is untouched: process-level, identical under every mode.
