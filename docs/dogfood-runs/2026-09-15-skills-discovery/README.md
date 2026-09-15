# Dogfood: does kopicode discover and use `.agents/skills/` skills?

- **Date:** 2026-09-15
- **What this tests:** EPIC-132, the skills mechanism (ADR-0014, KAN-1034/1035),
  end to end — not that the prompt sentence *exists*, but that a real model driven
  by kopicode actually finds a relevant `SKILL.md`, reads it before improvising,
  and lets it shape the work.
- **Binary:** built from `origin/main` at `34b68a5` (the commit that carries
  KAN-1034's `## Skills` sentence in `internal/harness/prompt_default.md`). A build
  from the repo's own stale local `main` would not have the sentence and the test
  would be meaningless.
- **Model / provider:** `qwen/qwen3-coder-next`, resolved to Parasail via the pin —
  the same arm the corpus is measured against.
- **Verdict up front:** the mechanism **works, but does not fire reliably.** Same
  model, same repo, same skills present: it discovered and correctly used the
  relevant skill on one task (README) and ignored an equally-relevant skill on the
  next (test coverage). Discovery today is *incidental* — it depends on the model
  happening to list the repo root — and the passive prompt sentence is not strong
  enough to make it happen every time.

## Setup

A throwaway Go CLI (`greet`, a 30-line flag-parsing program) in its own git repo,
with two skills placed at the convention path the mechanism documents:

```
.agents/skills/readme-playbook/SKILL.md    # relevant to a README task
.agents/skills/go-table-test/SKILL.md       # relevant to a test task; a distractor otherwise
```

`readme-playbook` is copied from the `leejianrong/claude-skills` repo (authored for
Claude Code's `.claude/skills/` layout); `go-table-test` is the documented example
from `docs/examples/skills/`. Both carry `name` + `description` YAML frontmatter, and
kopicode read both without modification — the Claude Code `SKILL.md` format is
compatible with the `.agents/skills/` convention exactly as ADR-0014 claims.

Each task was run headless with `kopicode run --print`, whose fail-closed default
refuses shell and out-of-repo writes but allows `read_file`, `list_dir`, `grep` and
in-repo `write_file`. Both chosen tasks need only reading and an in-repo write, so no
`--policy-file` was required. Evidence is the session journal; the raw `--print`
records are committed next to this file.

## Run 1 — "Write a README.md for this project." ✅ mechanism worked

Session `20260915T020723Z-42c0ad9c`, 7 turns, `ended:completed`, verification passed.
Tool sequence from the journal (`run1-readme-task.ndjson`):

| Turn | Tool | Target |
|---|---|---|
| 1 | `list_dir` | `""` (repo root) — **surfaces `.agents/` in the listing** |
| 2 | `read_file` | `main.go`, then `go.mod` |
| 3 | `list_dir` | `.agents` |
| 4 | `list_dir` | `.agents/skills` — sees both skills |
| 5 | `read_file` | **`.agents/skills/readme-playbook/SKILL.md`** |
| 6 | `write_file` | `README.md` |

Every success criterion is met:

- **Discovered** the skills directory unprompted by the task (the task never mentions
  skills), following the system-prompt sentence.
- **Selected the right skill** — it read `readme-playbook` and left the
  `go-table-test` distractor untouched.
- **Read before improvising** — the `SKILL.md` read (turn 5) precedes the `README.md`
  write (turn 6).
- **The skill shaped the output.** The final assistant message says, unprompted:
  *"Created a README.md following the playbook: value prop first, quick start with
  copy-pasteable commands, usage with concrete examples, and an explicit license. No
  badges, no API dump…"* — which is `readme-playbook`'s actual guidance. The produced
  README follows that structure (value prop → quick start → usage → license) with no
  badge wall. kopicode's forced verification then ran `go test ./...` (exit 0) even
  though the model never requested shell.

## Run 2 — "Add test coverage for the greet function." ❌ mechanism did not fire

Session `20260915T020929Z-917a6e53`, 5 turns, `ended:completed`, verification passed.
Tool sequence (`run2-test-coverage-task.ndjson`):

| Turn | Tool | Target |
|---|---|---|
| 1 | `grep` | `func greet`, then `greet` in `*.go` |
| 2 | `read_file` | `main.go` |
| 3 | `list_dir` | `*_test.go` (glob, not the root) |
| 4 | `write_file` | `main_test.go` |

The model **never listed `.agents/skills`** and **never read `go-table-test`** —
`grep -c "agents/skills"` over the record is `0`. It improvised, and the improvised
test contradicts the skill's core convention: `go-table-test` prescribes one function
per behaviour with a `[]struct` case table run through `t.Run`, and the model instead
wrote six separate `TestGreetX` functions with zero `t.Run`/table markers. So this is
not "read the skill and chose to ignore it" — the skill was never opened.

## Why the difference, and what it means

The two runs diverge on their **first move**, and that is the whole story. Run 1
opened with `list_dir .`, which puts `.agents/` right in front of the model; it pulled
the thread from there. Run 2 opened with `grep` for the symbol — a perfectly
reasonable first move for "find the function," and exactly what the prompt's own
"Working" section suggests first — and so it never saw the root listing that would
have revealed the skills directory.

The `## Skills` sentence is passive: *"This repository **may** package… **When one
looks relevant**, `read_file` it."* For the model to judge relevance it must already
know the skill exists, and nothing makes it look. Discovery therefore rides on the
model incidentally listing the root, which some task shapes do and others don't. That
is a real gap between "the mechanism is present" (true, and ADR-0014's zero-tool design
is vindicated on run 1) and "the mechanism is dependable" (not yet).

## Follow-ups (candidate cards)

1. **Strengthen the trigger** (KAN-1385). Make skill discovery a step the model takes regardless
   of task shape — e.g. the prompt instructing it to `list_dir .agents/skills` at the
   start of a task when the directory exists, or surfacing the available skill
   `name`/`description` lines once at session start (still shape (a): no new tool, no
   auto-injection of skill *bodies*, so ADR-0012's context-growth objection stands).
   This is the "observed problem, not a hypothetical" trigger ADR-0014/KAN-1021 named
   as the bar for escalating past a bare directory convention.
2. **Wider replication.** Two runs and one model are an existence proof of both
   outcomes, not a rate. A small matrix (a few task shapes × a couple of models)
   would turn "fires sometimes" into a number worth acting on.

## Reproducing

```bash
# built from origin/main @ 34b68a5
mkdir -p <repo>/.agents/skills && cp -r <skill> <repo>/.agents/skills/
cd <repo> && OPENROUTER_API_KEY=… kopicode run --print "Write a README.md for this project."
# then inspect .kopicode/sessions/<id>/events.jsonl for a read_file of the SKILL.md
```
