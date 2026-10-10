# ADR-0030: `serve` outlives its client and says what each session is doing

*Accepted.* Builds on [ADR-0013](0013-agent-controlled-resident-session-surface.md) (the resident surface), [ADR-0016](0016-live-remote-consent-for-agent-orchestrated-sessions.md) and [ADR-0021](0021-usage-reporting.md). Written after reading how cuttlefish-crew drives `serve` today.

## Context

cuttlefish-crew runs one resident `kopicode serve` per project root, over stdio. It starts one session per role per round, closes it, and starts another. It answers `consent.request` and `ask.request` itself, polls `session.usage`, and reads each session's `events.jsonl` from disk because the event stream has no command output and cuts tool arguments at 120 characters. Its dashboard is built from its own episodic journal.

Today a `serve` child ends its sessions when its client's stdin closes. A cuttlefish restart therefore ends every open session, and the round in flight is re-run. At 20 to 100 projects, restarts of the supervising process will be more frequent than any one long task finishes.

## Decision

1. **`serve --listen <unix-socket>`** is a second transport beside stdio. The socket is created mode 0600. The process and its sessions keep running when a client disconnects. Stdio remains the default and keeps today's behaviour exactly.
2. **One client at a time per socket.** A new connection replaces the previous one. Pending `consent.request` and `ask.request` are re-sent to the new client. A request nobody answers still expires as it does now.
3. **`server.sessions`** lists every open session: id, directory, `state` (`idle`, `running`, `awaiting_consent`, `awaiting_answer`, `ended`), current turn, time of the last event, `usage` as `session.usage` returns it, the last stop reason, and the id of any pending request. The state is read from the live session table, not rebuilt by the client.
4. **`session.events`** with `after_seq` replays journal events after a sequence number, for any open session, blob-aware. `session.event` notifications carry their `seq`.
5. **Lifecycle.** A listening `serve` exits after `--idle-timeout` (default 30 minutes, 0 disables) with no sessions and no client, or on `server.shutdown`, which closes every session first. It refuses to start if the socket path is held by a live process.
6. **Feature names** (added with each method, never renamed): `serve.listen`, `server.sessions`, `session.events_since`, `server.shutdown`. Each gets a row in `docs/kopicode-serve-protocol.md`.
7. **No second transcript.** Everything these calls return is the live session table or the journal.

## Rejected

- **A kopicode dashboard.** cuttlefish owns the cross-project view; duplicating it was rejected in the fleet-view decision.
- **Keeping state only on the client.** Correct while the client is up, wrong after it restarts, and it needs a second copy of what each agent is doing.
- **A TCP listener.** Larger attack surface for no need; cuttlefish and kopicode run on the same host.

## Consequences

- cuttlefish can reconnect after a restart and carry on mid-round instead of re-running it. Whether it does is its own decision; stdio keeps working.
- Orphan processes become possible, which is what the idle timeout and `server.shutdown` are for.
- The scale cost of several sessions in one process is unmeasured. A load test with many concurrent mock-provider sessions is the first card of the epic.
- The needs-attention states are what a fleet view cares about most at 100 projects.

## Status of the build

- **Decisions 3 and 4 are built** (KOP-185 `server.sessions`, KOP-186 `session.events`): the session table
  with its five states, and a blob-aware replay with `after_seq`, `limit` and `more`. Notifications already
  carried `seq`.
- Two choices the text left open. An ended session stays in the table (the newest 256) and its events stay
  readable, so a client that reconnects can see how it finished; and `session.events` pages by `limit` rather
  than returning everything, because a replayed event holds its whole text.
- **Decisions 1 and 2 are built** (KOP-184): `serve --listen`, one client at a time, pending requests re-sent.
  Details the text left open: a response goes to the connection its request came from and is dropped if that
  client was replaced (ids belong to their client); the path is claimed with a lock file rather than by
  connecting to it, because a probe connection would itself replace the live client; and `SIGINT` or
  `SIGTERM` closes the sessions and removes the socket. Unix only.
- Decision 5 (`--idle-timeout`, `server.shutdown`) is not built yet (KOP-187).
- The `mcp` front end shares the session manager but has no tool for either; the ADR names `serve` only.
