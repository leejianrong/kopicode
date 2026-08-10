# kopicode

A coding agent for the terminal.

Nothing is built yet. This README records the decisions already taken, so the
first commit of real code starts from them rather than re-deriving them.

## What makes it different

The loop runs on [Satay](https://github.com/leejianrong/satay-runtime), a durable
execution runtime that records every durable call to an append-only journal. So
**a kopicode session is a journal**, which means you can replay it, fork it at
the turn where it went wrong, and diff two attempts against each other.

That is the whole bet. Terminal coding agents are a crowded shelf and getting more
crowded; what none of them can do is let you reconstruct why the agent did
something, because the session is scrollback rather than a record. Satay's fork
and call-by-call compare already exist and work.

## Where it sits

```
kopicode (this repo)          sotong (later)
   terminal coding agent        always-on assistant
              \                /
            kopi-engine
              agent loop, tool dispatch, permissions,
              sessions, model routing
                       |
                     satay
              durable execution, journal, replay, fork
```

**[kopi-engine](https://github.com/leejianrong/kopi-engine) is shared with sotong
and lives in its own repo.** Two products
rather than two modes of one: something that acts while nobody is watching needs
a stricter safety model than something you are supervising, and that is an
architecture rather than a config flag.

## Constraints that already apply

- **Do not put the engine inside Satay.** Satay's
  [ADR-0025](https://github.com/leejianrong/satay-runtime/blob/main/docs/adr/0025-positioning-agents-first.md)
  decision 4 holds the no-agent-abstraction non-goal: Satay ships five durable
  primitives and cookbook examples, no loop framework, no provider adapters, no
  graph DSL. kopi-engine depends on Satay from outside. Merging it in would
  violate a merged ADR.
- **Satay is pre-1.0** (`0.1.0a3` on PyPI). Expect sharp edges, and expect to be
  the consumer that finds them. That is deliberate: Satay's roadmap is
  agents-first precisely because this is the workload it is meant to serve.
- **Apache-2.0**, matching the rest of the suite.

## Naming

`kopi` is coffee. `kopicode` is the terminal agent; `kopi-engine` is the shared
engine beneath it. `kopitiam` is **not** available for either, because it is
already the working name for the hosted plane in Satay's
[ADR-0026](https://github.com/leejianrong/satay-runtime/blob/main/docs/adr/0026-license-and-hosted-journal-plane.md).

## Status

Planned. See the [Abang page](https://leejianrong.github.io/abang-landing-page/)
for where this sits among the rest.
