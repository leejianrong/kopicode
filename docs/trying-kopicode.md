# Trying kopicode

A first-session guide: what to point kopicode at, what to type, and what you should
see happen. It assumes you have already installed kopicode and set
`OPENROUTER_API_KEY` — if not, do the [README quickstart](../README.md#quickstart)
first (steps 1–2).

Everything below uses the default model, `qwen/qwen3-coder-next`. A one-line fix costs
roughly **$0.04**; the two walkthroughs together are well under $0.20.

## What makes a good first task

kopicode is at its best when three things are true, and frustrating when they aren't:

1. **A real repository with a test suite.** kopicode runs the project's own tests after
   it edits (forced verification) and won't report success over a suite it can't make
   pass. A repo with no tests gives that mechanism nothing to check.
2. **One specific, described bug — not "improve this."** "`Ordinal(-1)` returns `-1th`
   but should return `-1st`" gives the model a target and a way to know it's done.
   "Make this code better" gives it neither.
3. **A small blast radius.** A first task you can read the diff of in ten seconds is how
   you build trust in the tool before you hand it something bigger.

The two walkthroughs below are real — they are the tasks in
[`docs/dogfood-runs/`](dogfood-runs/), replayable because the bugs still exist upstream.

## Walkthrough 1 — a Go one-liner (`go-humanize`)

Needs Go on your machine. `github.com/dustin/go-humanize`'s `Ordinal` mishandles
negative numbers: `Ordinal(-1)` returns `-1th` instead of `-1st`.

```bash
git clone https://github.com/dustin/go-humanize.git
cd go-humanize
kopicode
```

At the prompt, describe the bug and the contract:

```
Ordinal(-1) in this package returns "-1th" but it should return "-1st" (and
similarly -2 should be "-2nd", -3 "-3rd", -11/-12/-13 should stay "th"). Find the
bug and fix it, and make sure the existing tests still pass.
```

What happens: the model greps for `Ordinal`, reads `ordinals.go`, makes an
anchor-based edit, and kopicode runs `go test ./...` on its own as verification. In the
recorded run the model's *first* attempt failed that check — the `verification:` line
said so — and it corrected itself before reporting done. That is the loop working, not
failing: a wrong edit is caught by the tests, not shipped.

When it finishes, see what it did:

```bash
git diff
```

## Walkthrough 2 — a JavaScript fix (`bytes.js`)

Needs Node. `visionmedia/bytes.js` strips trailing zeros inconsistently:
`bytes(1536, { decimalPlaces: 3 })` returns `1.5KB` but `bytes(1075, { decimalPlaces: 3 })`
returns `1.050KB`.

```bash
git clone https://github.com/visionmedia/bytes.js.git
cd bytes.js
npm install          # the test suite (mocha) is a dev dependency
kopicode
```

```
bytes(1536, { decimalPlaces: 3 }) returns '1.5KB' but bytes(1075, { decimalPlaces: 3 })
returns '1.050KB' -- the trailing-zero stripping is inconsistent. Find the bug and fix
it so trailing zeros are stripped consistently (expected '1.5KB' and '1.05KB').
```

Here kopicode discovers `npm test` (from `package.json`'s `scripts.test`) and runs it as
verification. Note one honest thing from the recorded run: even after the tests passed,
that session ran to the **turn cap** and exited with code `4` rather than a clean `0`.
A turn-capped session is not the same as a failed one — check `git diff` and the test
result, not just the exit code. It is also the kind of thing worth watching for as you
tune a harness.

## What you'll see while it runs

- **The consent prompt.** Editing files inside the project is not gated — that's the
  agent's job. But if the model decides to run a *shell command* itself, kopicode stops
  and asks; a bare Enter denies. See the [README](../README.md#4-first-run) for the exact
  prompt and what `y`/`a` do. (Forced verification — kopicode running the project's own
  test command — runs automatically and is not the same as a model-authored shell call.)
- **`[note] record: …`** — the path to this session's journal under `.kopicode/`. Every
  request, tool call, edit, and verdict is there as newline-delimited JSON if you want to
  see exactly what happened.
- **`Ctrl-C`** cancels the turn in flight — the model's reply or a running command —
  without ending the session. You get the prompt back.
- **The `verification:` line** is kopicode running the tests. `passed`, `FAILED`, or
  "nothing ran, so nothing here is a pass" when a repo has no discoverable test command.

## Point it at your own repository

The same shape works anywhere: `cd` into a repo with tests, run `kopicode`, and describe
one concrete change. Optionally drop a [`docs/examples/`](examples/) config in as
`.kopicode/config.toml` to pin the model (or a `verify` command) for everyone who runs
kopicode there — though the default model needs no config at all.

## See the harness's own numbers

kopicode ships a frozen 13-task corpus under [`bench/tasks/`](../bench/tasks/) — real
buggy fixtures, each with a test oracle checked in both directions (fails before the fix,
passes after). Two ways to run it:

```bash
make bench-smoke   # the corpus against the mock provider — zero tokens, no key needed
make bench         # the corpus against the real pinned model — costs money (~$0.08)
```

`bench-smoke` is a plumbing check (the replayed fixtures answer in prose, so 0/13 pass by
design — it proves the rig runs end to end). `make bench` is the real measurement.

## The other surfaces

The interactive REPL is one of three renderings of the same engine:

- `kopicode run --print "<task>"` — headless, one shot, newline-delimited JSON on stdout.
  Schema is documented in `cmd/kopicode/print.go`.
- `kopicode serve` — a resident process another agent drives over JSON-RPC on stdio; see
  [`docs/kopicode-serve-protocol.md`](kopicode-serve-protocol.md).

Both refuse shell and out-of-project writes by default (nobody's at a terminal to
consent); a `--policy-file` is how an orchestrator declares what an unattended run may do.
