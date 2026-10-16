# Spike: a second client on a live `serve` session, and steering a running turn

- **Card:** KOP-201. **Asked by:** cuttlefish-crew (CUT-91, `docs/research/session-control-spike.md` in that repo).
- **Status:** a spike. Nothing here is built; the wire change is an ADR outline at the end, not an accepted ADR.
- **Read against:** [ADR-0030](adr/0030-resident-serve-and-session-state.md) and [the serve protocol](kopicode-serve-protocol.md).
  Where this page and the code disagree, the code is right.

## What cuttlefish found in Codex

On `codex app-server --listen ws://`, two clients shared one thread. Both received the same status and item
stream; one called `turn/steer` on the other's running turn and the text landed. Both received every approval
request, and the first answer was final. The listener binds to loopback; a phone needs a private network in front.

## What `serve` does today

The server holds **one** connection (`server.cur`, `cmd/kopicode/serve.go`). A new connection closes the old
one (`replace`, `serve_listen.go`).

| Question | Today | Where |
|---|---|---|
| A second client **watching** a session | **No.** A second connection replaces the first. Reading the record is possible without a connection: `session.events` replays from a `seq`, and `events.jsonl` is on disk | ADR-0030 decisions 2 and 4 |
| An observer that cannot answer consent | **No** such connection kind | n/a |
| Who gets `consent.request` with two clients | Not defined. `sendRequest` writes to `cur` and remembers the request in `sent`; `attach` re-sends `sent` to whoever replaces `cur`. With one client the question does not arise | `serve.go` `sendRequest`, `attach` |
| **Input into a running turn** | **No.** `session.submit` queues behind the turn (FIFO); `session.cancel` ends it. Nothing reaches a turn already running | `session/session.go` queue |
| The per-root lock | Independent of clients. `engine.Open` takes the flock on `<tree>/.kopicode/lock`; a connection holds nothing. A second client that calls `session.start` or `resume` on a tree an open session holds gets `-32005`, which is correct | `internal/lock` |
| TCP or `ws://` with a token | **No.** ADR-0030 rejected a TCP listener ("larger attack surface for no need") | ADR-0030 *Rejected* |

So of the five things asked, the lock already behaves, and the other four are missing.

## What each would take

**1. Steering a running turn.** The engine loop (`internal/engine/loop.go`, `Run`) checks `ctx`, the turn cap
and the token budget at the top of each iteration, then sends the next provider request. That is a natural
checkpoint: a steer waiting in a queue is appended there as a user message (`asm.AppendUser` plus a
`user_message` journal event) before the request goes out. It lands at the **tail** of the request, so ADR-0031's
and KOP-172's cached prefix is untouched, and it is a journal event, so a replay and a `resume` rebuild it.

- It cannot interrupt a model call or a shell command already running; `session.cancel` does that. Latency is
  one tool batch or one model response.
- Open point: a steer that arrives just as the model answers in prose. Recommend that a pending steer stops the
  loop from completing, so one more turn runs, and that it counts against `max_turns`. The alternative, dropping
  it, loses the user's words.
- Guard like Codex's `expectedTurnId`: `session.steer` carries the `turn` it was meant for and is refused with
  a usage error if that turn has finished, so a late steer is never silently turned into a new prompt.
- Needs **one** client. cuttlefish's dashboard can steer through cuttlefish's own connection, so this does not
  wait for anything below.

**2. Observers.** A second connection that sends `server.observe` joins a set that receives every
`session.event` and may call only the read methods (`server.sessions`, `session.events`, `session.usage`).
Everything else is refused with a new code. A connection that never says `observe` is a controller and keeps
today's replace-the-last behaviour, so no existing client changes. `server.write` fans out to the set. No engine
change. Slow observers must not stall a turn: a bounded per-connection buffer, dropping the connection when it
fills (it can catch up with `session.events`, which is why `seq` exists).

**3. Two controllers, and consent.** The harder case and the one with a safety edge. Options:

- *Controller only* (recommended): `consent.request` and `ask.request` go to the one controller, as now. An
  observer never sees one to answer. Nothing about consent changes, and ADR-0016's "unanswered denies" holds.
- *Everyone, first answer wins* (Codex): every attached controller is asked, the first reply settles it, and a
  `consent.resolved` notification tells the others to drop their card. It needs a decision on what `source`
  the journal records, and it makes a stray second client able to approve a shell command. ADR-0016 rejected
  loosening consent for convenience; this should have its own ADR if it is ever wanted.

A person's terminal beside a dashboard is then: dashboard is the controller, the terminal is an observer that can
also send `session.steer` (steering is input to the model, not consent). That needs steer to be allowed from an
observer-with-input connection, which is a third connection kind and should wait for evidence it is wanted.

**4. A network listener.** ADR-0008's trust model is one developer on their own machine. A TCP listener adds
a remote attacker who may drive a shell tool, and the consent model assumes whoever is on the wire is trusted.
A token is the floor, not the answer. `ws://` also needs a websocket library, which breaks "dependencies stay
near zero" (the standard library has none); plain NDJSON over TCP would not, but nothing speaks it except
cuttlefish. The unix socket already reaches a private network through `ssh -L` or `socat`. **Do not build.**
Revisit when there is a client that needs it and an ADR that states the threat model.

## Recommendation

Build, in this order, each its own card:

1. **`session.steer`** (single client). Smallest step, serves cuttlefish alone, needs one engine change and one wire
   method. Feature `session.steer`.
2. **Observers** (`server.observe`), only if cuttlefish confirms a dashboard that watches a session another
   connection controls, instead of reading `events.jsonl` and `session.events`. Feature `serve.observe`.
3. **Two controllers** only with a stated need, and consent stays with one.
4. **Network listener:** do not build.

The first step is a test that a steer sent while a tool batch runs is in the next provider request, once, and in
the journal.

## ADR outline (proposed ADR-0037, not accepted, nothing implemented)

**Title.** `serve` accepts input into a running turn, and a connection kind that only watches.

**Context.** ADR-0030's one-client rule and FIFO `session.submit` leave no way to say something to a running
turn, or to watch it from a second place.

**Decisions to settle.**

1. `session.steer {session, turn, prompt}`: queued on the session, consumed at the top of the engine loop before
   the next provider request, appended as a user message. Refused with `-32003` when no turn is in flight or `turn`
   is not the running one. Recorded as `user_message` with `steered: true` (additive field; unknown fields are
   preserved).
2. A pending steer prevents a prose answer from completing the exchange; the turn count and `max_turns` apply.
3. `server.observe`: the connection joins the observer set; read methods only; a new error code for the rest;
   bounded buffer, dropped on overflow; consent and ask requests never go to an observer.
4. Connections that do not observe keep ADR-0030's replace-the-last behaviour.
5. No network listener (ADR-0030's rejection stands); the reasons above are recorded.

**Rejected.** First-answer-wins consent across clients; `ws://` and a bundled websocket library; steering by
cancelling and re-submitting (loses the in-flight tool result).

**Consequences.** Features `session.steer` and `serve.observe` and rows in the protocol doc; a turn's
`user_message` can now arrive mid-turn, which a client that assumes one per prompt must tolerate (a
`protocol` bump is not needed: it is additive).
