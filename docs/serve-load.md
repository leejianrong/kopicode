# One `serve` process, many sessions: what it costs

Measured for KOP-182, the scale question ADR-0030 left open: what a resident `serve` pays for holding many sessions at once, and whether that changes the design before the listener work. The test is `TestServeLoadManyConcurrentSessions` in `cmd/kopicode/serve_load_test.go`. It drives the stdio transport (the code a socket serves) against a local mock provider that waits 150 ms before it answers, so no token is spent and turns overlap measurably. It is skipped under `-short`, runs 10 sessions by default, and `KOPICODE_LOAD_SESSIONS=1,10,50,100,200 go test -count=1 -v -run TestServeLoadMany ./cmd/kopicode/` reproduces the table.

One run each on a 16-core WSL2 machine, no `-race`. Treat the figures as orders of magnitude, not as a benchmark: they move by tens of percent between runs, and the provider is a local mock, so a real model's turns are far longer than 150 ms.

| sessions | heap per session | goroutines per session | fds per session | open all (serial) | second turn, all at once | first event after submit p50 / max | `server.sessions` mid-turn p50 / max | shutdown of all |
|---|---|---|---|---|---|---|---|---|
| 1 | 262 KiB (one-off warm-up) | 6 | 5 | 223 ms | 186 ms | 5 ms / 5 ms | 50 µs / 106 µs | 13 ms |
| 10 | 52 KiB | 4.2 | 5 | 704 ms | 204 ms | 20 ms / 20 ms | 95 µs / 436 µs | 135 ms |
| 50 | 37 KiB | 4.0 | 5 | 2.8 s | 227 ms | 16 ms / 32 ms | 168 µs / 3.8 ms | 486 ms |
| 100 | 36 KiB | 4.0 | 5 | 5.6 s | 259 ms | 22 ms / 47 ms | 281 µs / 1.4 ms | 916 ms |
| 200 | 33 KiB | 4.0 | 5 | 11.9 s | 272 ms | 25 ms / 56 ms | 405 µs / 1.6 ms | 2.1 s |

## What it says

- **Idle sessions are cheap.** About 35 KiB of heap, 4 goroutines and 5 file descriptors each. Of those, one goroutine and three descriptors are the session's own (its worker, its journal and lock files); the rest is the idle keep-alive connection to the provider, which here has both ends in the test process (a real provider has one end: 2 goroutines and 1 descriptor). 200 sessions is about 7 MB of heap. Memory is not the constraint.
- **Turns on open sessions overlap fully.** With 200 sessions each starting a turn at once, all 200 were in the provider together and the last reply came 272 ms after the submit, against 150 ms for one. One worker per session scales.
- **A request that must not queue behind a turn does not.** `server.sessions` answered in under 2 ms at 200 sessions while every one of them was mid-turn, and it saw all 200 `running`.
- **Event latency is flat.** The first event of a turn reaches the client in tens of milliseconds at 200 sessions. Every session's notifications go through one connection's write lock, and at this rate it does not show.
- **Shutdown is linear and clean**: about 10 ms a session, and the process returns to its starting goroutine and file-descriptor counts.

## What it found, and what was done

1. **A data race in `server.sessions`** (fixed here). `Manager.Sessions`, added in KOP-185, read `Engine.Turns()` from the read loop while the session's own loop was writing the counter. It only shows with a session mid-turn, which is exactly when a client asks. `Engine.Turns` now reads an atomic mirror the loop stores to; `TestServeSessionsMayBePolledDuringATurn` fails under `-race` without the fix.
2. **Every finished session kept a connection** (fixed here). Each provider client clones `http.DefaultTransport`, so each session has its own connection pool, and `Session.Close` never closed it. A closed session left one idle connection, 3 goroutines and 2 file descriptors until the transport's 90 s idle timeout. For a process that lives for days and opens sessions all the time that is a steady trickle held for 90 s each, not a leak, but it was the only thing the process did not give back. `Session.Close` now closes the client's idle connections.
3. **Opening a session is serial, and blocks the read loop** (not changed; see below). `session.start` resolves the selection and calls `engine.Open` inline on the read loop under the manager's start lock. One open costs about 55 to 70 ms here (four `git rev-parse` subprocesses, the lock and the journal). Ten opens in parallel take about a third of the time of ten in sequence (176 ms against 500 ms in a probe). So a client that opens 100 sessions at once waits about 5.6 s for the last, and **while it does, nothing else on that connection is answered**, `session.cancel` and `server.sessions` included.

## Does it change the design?

ADR-0030's design stands: the numbers give no reason to run sessions in separate processes, to pool them, or to limit how many one process holds. Memory and goroutines are small, turns overlap, and the inline table read works.

Item 3 is the one real cost, and it is a follow-up, not a reason to rethink. Moving `engine.Open` off the read loop is not a one-line change: a client may send `session.submit` for an id straight after `session.start`, so an asynchronous start has to keep requests for one session in order while letting different sessions open in parallel. That ordering rule needs deciding on its own, so it is a card (see KOP-188) rather than part of this one.

## What this does not measure

- A real provider: real turns are seconds to minutes, so the overlap here is a floor on what a fleet sees, and rate limits, which a mock does not have, would bite first.
- Tool calls (a shell or a test run per turn) and the subprocess load they add; the mock answers in prose.
- A long session: each session here has two short turns, so growth of one session's memory with its history is `docs/token-growth.md`'s question, not this one's.
- The socket transport under load: it serves the same code and one connection, but the test drives stdio.
