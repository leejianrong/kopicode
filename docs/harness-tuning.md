# Tuning the harness

kopicode ships one binary that already knows how to run every model it supports,
with a hand-tuned harness configuration per model. Most of the time you never
touch any of this. But the built-in settings are fixed at build time, and
sometimes you need one of them to be different — most commonly, a task that needs
more turns than the default cap of 20.

A **declared harness configuration** (ADR-0010) is how you change a bound without
rebuilding. It is a small TOML file that starts from a built-in configuration and
overrides the few fields you name.

## The turn cap, the common case

If a session stops with `max_turns` (exit code 4), it ran out of turns before it
finished. Both surfaces now tell you how to raise it, but here it is in full.

Write a file — call it `harness.toml`:

```toml
base = "default"
max_turns = 40
```

Then point kopicode at it:

```bash
kopicode --harness-config harness.toml "…your task…"
```

That is the whole thing. `base = "default"` inherits every setting of the built-in
`default` configuration; `max_turns = 40` changes just the one.

## What you can override

All four are existing harness fields — declared configs choose values for fields
that already exist, they do not add new ones.

| key | meaning | bound |
|---|---|---|
| `max_turns` | the loop's turn cap | greater than 0 |
| `token_budget` | the whole session's prompt+completion token allowance | 0 or more (0 = unbounded) |
| `repair_budget` | how many repair round trips a malformed tool call gets | 0 or more (0 = no repair) |
| `max_tokens` | the per-reply completion cap sent to the provider | greater than 0 |

`base` is required and must name a built-in configuration. To see the current set,
run kopicode with a name that does not exist — the error lists them:

```bash
kopicode --harness not-a-real-name "x"
```

The reader is strict on purpose: an unknown key, a `[table]` header, a
non-integer value, or an out-of-bounds value is a startup error (exit 2) that
names the file and line. A silently-ignored `max_turns` is exactly the confusion
this feature exists to remove, so a declared config never ignores a key it does
not understand.

There is a copy-paste template at
[`docs/examples/harness-config.toml`](examples/harness-config.toml).

## Two ways to name it, one precedence

You can name a declared config on the command line or in the repository config,
and it is the same harness axis as `--harness`/`harness =` — the two are two ways
to choose the harness, so naming both at one level is an error rather than a
silent winner.

- **Per invocation:** `--harness-config <path>`. A relative path is resolved
  against the directory the session runs in.
- **Per repository:** in `.kopicode/config.toml`,

  ```toml
  harness_config = "harness.toml"
  ```

  A relative path here is resolved against the config file's own directory
  (`.kopicode/`), so the declared config lives beside it.

The command line beats the repository config, which beats the model's built-in
default — the same precedence `--model`/`--harness` already follow. There is no
environment variable at any level, by design (ADR-0007 decision 3): the arm a run
used has to be visible in shell history or a diff, not ambient.

## Local-only: a declared config never anchors a published number

A declared configuration is yours, on your machine, and it is deliberately kept
out of the set of things kopicode publishes benchmark numbers about (ADR-0010
decision 3). The mechanism is not a policy you have to remember: a declared
config's resolved name is `declared:<base>`, and because the name is part of the
harness config hash, a declared arm can never share a hash with — and so never
pool with — a built-in one. Every session still records its full configuration
and hash in the journal, so your own runs remain reproducible against your own
binary; what a declared config cannot do is stand in for a built-in in a number
anyone else is meant to reproduce. A paired `kopibench` report says so outright
when either arm is a declared config.

## What this is not (yet)

ADR-0010 also describes `kopitune`, an automated search over harness settings, and
an unofficial-corpus mode. Neither is built yet. This page covers only the
declared-config half: writing values for the fields that already exist and
pointing kopicode at them.
