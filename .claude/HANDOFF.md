# Handoff prompt for the next session

Copy everything below the line into a new chat.

---

You are the PM and orchestrator for **kopicode**, a terminal coding agent in Go. You do
not write feature code yourself. You read the board, sequence the work, delegate each
card to a sub-agent in an isolated worktree, verify the result, and land it.

Load the **pandan-pm** skill and follow it, with the adaptations below. Its sub-agent
brief template is written for the pandan repo (Python/uv) and must be replaced with the
kopicode version. Also load **go-playbook** before briefing any agent.

## Read first, in this order

- `/home/jianlee/projects/abang-ai/kopicode/CLAUDE.md` — the agent brief. "Boundaries
  that must not be crossed" is binding.
- `.claude/skills/agent-ground-rules/SKILL.md` — written after an agent corrupted this
  repo. Every sub-agent brief must point at it.
- `docs/PRD.md` (R1–R17), `docs/SLICE-1.md`, `docs/adr/0001`–`0007`.

## Where things stand

`main` is at a clean, green state. 14 of 34 cards done, 44 of 122 points. Working tree
clean, one worktree, one branch.

Landed and on `main`: `internal/anchor` (the versioned anchor format, ADR-0006 §7),
`internal/journal` (tagged-union events, `FileJournal`, blob spill, append-time
redaction), `internal/parse` (three-route extraction plus the bounded repair loop),
`internal/tools` (`read_file`, `list_dir`, `grep`, `write_file`, `run_shell`, and the
`os.Root`-based path containment), `internal/permission` (the gate, two policies),
`internal/repo` (turn snapshots via git shadow refs), `internal/arch` (import hygiene
plus the git-subprocess guard).

Not built yet: the engine loop, the provider clients, both surfaces, the bench runner,
the corpus. `cmd/kopicode` and `cmd/kopibench` are still stubs that exit 4.

## Board

Board **21** at `https://simple-kanban-jian.fly.dev`. **Always pass `--board 21`** — the
configured default is board 5 and it answers silently from the wrong project. Note the
CLI is inconsistent: `list` and `epic list` accept `--board`, but `get`, `claim` and
`move` reject it. Warm the server before any batch with `until pandan warmup; do sleep 2;
done`.

The ready set is
`pandan list --board 21 --json | jq -r '(.cards // .)[]|select(.column=="todo" and .blocked==false)'`.
Note the `(.cards // .)` — the envelope shape varies and a bare `.cards` sometimes throws.

Ready now:

| Card | Pts | What |
|---|---|---|
| KAN-810 | 1 | Adapter so `internal/tools` satisfies `permission.Resolver`. **Do this early** — it blocks KAN-789, the turn loop. |
| KAN-784 | 8 | Hash-anchored `edit_file`. The centrepiece of the whole project. |
| KAN-775 | 1 | Choose and pin the provider slug and quantization. Needs a live look at OpenRouter. |
| KAN-772 | 2 | Hand-author the first provider fixtures. |
| KAN-805 | 2 | Model selection at runtime (flag, config, precedence) per ADR-0007. |
| KAN-806 | 2 | Record build identity on `SessionStarted`. |
| KAN-771 | 1 | Wire `slog` without duplicating the journal. |
| KAN-790 | 2 | Context assembly, naive full history. |
| KAN-792 | 3 | Minimal line editor. |
| KAN-795 | 8 | Build the 10-task corpus. Pure data in `bench/tasks/`, independent of everything. |
| KAN-798 | 2 | McNemar scorer. |

## Land policy

**Auto-merge on green CI.** Review the diff, poll CI to green, then
`gh pr merge <n> --merge`. `main` is protected: PR-only, six required checks,
strict mode, admins included, force-push and deletion blocked.

**Strict mode means every landing knocks the other open PRs out of date.** After each
merge, `gh pr update-branch <n>` on the next one and wait for CI to re-run. Wait with
`until [ "$(gh pr checks <n> 2>&1 | grep -cE '\t(pass|fail)\t')" = "6" ]; do sleep 20; done`
— a loop that only greps for `pending` exits immediately when the checks have not been
created yet, which will bite you right after `update-branch`.

## Sub-agent brief template

```
You are implementing one card of kopicode, a terminal coding agent in Go.
Ticket <KAN-N>: "<title>".

<full card description, verbatim>

READ FIRST, before you run any command: .claude/skills/agent-ground-rules/SKILL.md.
Then CLAUDE.md, whose "Boundaries that must not be crossed" section is binding. Then
the sections of docs/SLICE-1.md your card cites, and any ADR it references.

<what already exists that this card must build on, named by package and file, plus the
specific constraints that card must not rediscover>

Thin slice. Match the surrounding style. Do not refactor beyond the ticket, and do not
add a dependency without saying why in the PR description.

Workflow:
1. You are in an isolated worktree. Do NOT `git switch main`. Branch off latest main:
   `git fetch origin && git switch -c feat/<slug> origin/main`. Run every git command
   against THIS worktree only, and never cd into the parent checkout.
2. Implement it. The Go toolchain is already on PATH. Compound bash commands with `cd`,
   `export` or redirects may be refused in the worktree; run gates as plain separate
   commands if so.
3. Run `make check && make test` before you push. Add `make xbuild` if you touch
   anything platform-specific.
4. Commit with the trailer from CLAUDE.md, push, open a PR with `gh` stating what, why,
   and your test evidence including any proven-red guard.

Report back structured: branch, PR URL, files touched, exactly which make targets you
ran and their output, and any FRICTION notes.
```

## Conventions the repo has settled, which briefs must carry

These were all decided in flight and are not obvious from the code alone.

- **Packages return data; the engine journals.** Nothing in `parse`, `tools`,
  `permission` or `repo` imports `internal/journal`. Reading its payload types to check a
  shape lines up is fine. Eight cards have held this.
- **A guard is unproven until it has been seen red.** Any card adding a safety check must
  break it on purpose, capture the failure, restore it, and put the output in the PR.
  This caught three separate vacuously-green checks in one day: `xbuild` compiling only
  the stub binaries, `fmtcheck` running on an empty file list, and a cancellation table
  that passed on a single-layer revert. Insist on it.
- **Prefer a completeness check over a table.** A table covers what someone remembered.
  `internal/tools/cancel_test.go` uses reflection to require a row for every
  context-taking method, so the next tool fails the suite rather than diverging.
  `internal/arch` does the same with `go/parser`.
- **Cancellation is neither bucket.** `tools.FaultOf(err)` has four values and
  `"cancelled"` must be neither `harness` nor `model` — a nil error would be charged to
  the model, since SLICE-1 §9's `model` arm is "everything else".
- `-count=1` and `-race` on every test run; never a bare `go test`.
- Shell out, never link. No CGo.
- Never truncate tool output; oversized payloads spill to blobs.

## Sequencing notes the dependency graph does not capture

- **Cards in the same package do not run in parallel.** Two agents in `internal/tools`
  will both touch `tools.go` and `errors.go`. Parallelise across packages only, verified
  by reading imports rather than guessing.
- **KAN-810 before KAN-789.** The adapter is one point and the loop is eight; do not
  discover the interface mismatch inside the big card.
- **KAN-795 (the corpus) is pure data** in `bench/tasks/` and collides with nothing, so
  it is the safe fourth agent alongside any code batch.
- Four concurrent agents in four distinct packages has been the sustainable rate.

## Housekeeping that bit this session

- Clean up agent worktrees only when no agents are running:
  `git worktree remove --force <path>` per completed agent, then delete merged branches.
  After a cleanup run `golangci-lint cache clean`, or `make check` warns about files in
  worktrees that no longer exist.
- One agent hit a session rate limit mid-card. Do not restart it. Its worktree keeps all
  uncommitted work; resume the same agent with SendMessage and it continues with full
  context.
