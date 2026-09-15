# Re-dogfood: did KAN-1385 make `.agents/skills` discovery reliable?

- **Date:** 2026-09-15
- **What this tests:** KAN-1385, the strengthened skill-discovery trigger. The
  KAN-1384 dogfood found discovery was incidental — it fired on a task whose first
  move was `list_dir .` and missed one whose first move was `grep`. KAN-1385
  rewrote the `## Skills` section from a passive pointer into an imperative to
  `list_dir` `.agents/skills` *before the first move, whatever the task looks
  like*. This run asks whether that prose change actually made discovery fire on
  the task shape that missed before.
- **Binary:** built from `origin/main` at `bf5b7a7` (the KAN-1385 merge, PR #150),
  `make build`, tree clean. `session_started` records the harness config hash
  `2eb2124667…`, which is KAN-1385's post-rewrite default hash — so every session
  below genuinely ran the new prompt, not the old one.
- **Model / provider:** `qwen/qwen3-coder-next`, resolved to Parasail via the pin
  — the same arm as the KAN-1384 dogfood and the corpus, so this is a like-for-like
  comparison.
- **Setup:** the same throwaway `greet` Go CLI in its own git repo, with the same
  two skills at the convention path: `readme-playbook` (relevant to the README
  task) and `go-table-test` (relevant to the test task; a distractor otherwise).
  `go-table-test/SKILL.md` is copied verbatim from `docs/examples/skills/`;
  `readme-playbook/SKILL.md` reproduces the guidance the KAN-1384 run cited.
- **Verdict up front:** the prose change **moved behaviour but did not fix
  discovery.** The README task still discovers and uses its skill. The
  test-coverage task still misses — **consistently, across three runs** — even
  though the strengthened prompt now nudged its first move from `grep` to
  `list_dir .` (which puts `.agents/` right in front of the model). Prompt-only is
  necessary-but-not-sufficient for this model: the imperative is read and not
  followed.

## Runs

### Run 1 — "Write a README.md for this project." ✅ discovered, as before

Session `20260915T082059Z-…`, 6 turns, `ended:completed`, verification passed.
Tool sequence (`run1-readme-task.ndjson`):

| Turn | Tool | Target |
|---|---|---|
| 1 | `list_dir` | `""` (repo root) — surfaces `.agents/` |
| 2 | `read_file` | `main.go`, then `go.mod` |
| 3 | `list_dir` | `.agents` |
| 4 | `list_dir` | `.agents/skills` — sees both skills |
| 5 | `read_file` | **`.agents/skills/readme-playbook/SKILL.md`** |
| 6 | `write_file` | `README.md` |

Unchanged from KAN-1384's run 1: the model browses the root, drills into
`.agents/skills`, reads the right skill, leaves the distractor. This task already
worked, and it still does.

### Run 2 — "Add test coverage for the greet function." ❌ missed, three times

Three sessions (`run2-test-coverage-task-{a,b,c}.ndjson`), each 5 turns,
`ended:completed`, verification passed. **All three produced the identical tool
sequence**, and `grep -c "agents/skills"` over each record is `0`:

| Turn | Tool | Target |
|---|---|---|
| 1 | `list_dir` | `.` (repo root) |
| 1 | `grep` | `func greet` |
| 2 | `read_file` | `main.go` |
| 3 | `grep` | `_test.go` |
| 4 | `list_dir` | `.` with `*_test.go` |
| 5 | `write_file` | `main_test.go` |

The model **never listed `.agents/skills`** and **never read `go-table-test`**,
in any of the three runs.

## What changed, and what didn't

The one thing the prompt *did* change is visible in run 2's first move. In
KAN-1384 the test task opened with `grep func greet` and never listed anything at
the root at all. Here it opens with `list_dir .` — the imperative pushed the model
toward a listing instinct. But `list_dir .` on the repo root shows `.agents/` as
one entry, and the model walked straight past it to `grep`, exactly as
`## Working` step 1 tells it to ("`grep` for the symbol, `list_dir` for the
layout, `read_file` for the region"). So the nudge landed on the wrong listing:
the model now lists, but it lists the root for the symbol's sake, not
`.agents/skills` for a skill's sake, and never takes the second step.

The likely cause is a conflict inside the prompt. The `## Skills` section says
"list `.agents/skills` before your first move"; `## Working` step 1, a few lines
later, describes the narrow grep-first move the model actually makes on a
"find this function" task. On the README task — which invites broad exploration of
the whole project — the two do not conflict and discovery fires. On the
test-coverage task — which invites narrow targeting of one symbol — step 1 wins and
skills are skipped.

One nuance that does *not* rescue the mechanism: the test the model improvised is
itself roughly table-shaped (`func TestGreet` with a `tests := []struct{…}`
slice), so on this particular easy task `go-table-test`'s guidance would have
changed little. That is luck, not discovery. A skill that is never opened cannot
help on the tasks where its guidance *would* have mattered, and "the model
happened to already do the right thing" is not a property you can lean on.

## Conclusion and follow-up

KAN-1385 is a real improvement — the prose is stronger and the passive sentence it
replaced was strictly worse — but this run is honest evidence that **a prompt
sentence alone does not make discovery reliable on `qwen/qwen3-coder-next`.** The
model reads the imperative and does not act on it when the task pulls it toward a
narrow first move. Three identical misses is a consistent failure, not sampling
noise.

This is the "observed problem, not a hypothetical" that ADR-0014 / KAN-1021 named
as the bar for escalating past a bare prose convention. Two candidate next steps,
filed as a follow-up card:

1. **Cheaper, still prompt-only, still shape (a): fold discovery into `## Working`
   as the literal first step**, so the instruction the model follows on a narrow
   task *is* "list `.agents/skills` first," removing the conflict with step 1
   rather than adding a competing section above it. Unproven — it would need its
   own re-dogfood — but it costs nothing to try.
2. **More robust, an engine change: surface the available skills' `name` +
   `description` lines once at session start** (still shape (a): no new tool, no
   auto-injected skill *bodies*, so ADR-0012's context-growth objection is
   bounded to a handful of lines). This removes the dependency on the model
   volunteering a `list_dir` at all — the names are simply in front of it — at the
   cost of a small, bounded context addition and an enumeration step in the
   engine. If global skills (`~/.agents/skills`, KAN-1402) are ever added, this is
   the same surfacing path.

## Reproducing

```bash
# built from origin/main @ bf5b7a7, make build
GREET=$(mktemp -d)/greet && mkdir -p "$GREET/.agents/skills"
# greet: a ~30-line flag-parsing CLI; skills copied as above; git init + one commit
cd "$GREET"
OPENROUTER_API_KEY=… kopicode run --print "Write a README.md for this project."   # discovers
OPENROUTER_API_KEY=… kopicode run --print "Add test coverage for the greet function."  # misses
# inspect the record: grep -c "agents/skills" shows 4 for the README task, 0 for the test task
```
