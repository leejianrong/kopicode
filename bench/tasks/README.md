# The kopicode task corpus

Ten small coding tasks with unit-test oracles. This is the thing every A/B number
in this project is measured against, so most of what follows is about the
properties that make a number trustworthy rather than about the tasks themselves.

**These numbers are internal-only.** A pass rate from this corpus is not
comparable to SWE-bench, to a published leaderboard, or to anything outside this
repository — the tasks were written here, by us, for this harness. ADR-0005 §6 is
explicit about it: what the rig measures is the *delta* between two arms over the
identical task set, and the only externally comparable figure would come from the
SWE-bench-Verified anchor run, which is not in slice 1. Do not quote a slice-1
number as a capability claim.

## Layout

```
bench/tasks/
  corpus.json              the corpus manifest: version, digest, run order
  <task-id>/
    task.json              the task manifest
    repo/                  the starting tree — the only part the agent sees
bench/_solutions/
  <task-id>/               reference fix, as an overlay on repo/
```

The leading underscore on `_solutions` is load-bearing twice over. The Go tool
ignores directories whose names begin with `_`, so the reference fixes are not
compiled, vetted, formatted or linted as part of this module; and they sit
outside `bench/tasks/` so that a run confined to its own task root cannot read
the answer. Keeping the agent inside `bench/tasks/<id>/repo` is the runner's job,
and it is the reason the solutions are not stored next to the task.

## What a task is

Every task carries:

- **A starting tree** (`repo/`) that is a complete, self-contained project. Go
  tasks have their own `go.mod` — which also keeps them out of `go list ./...`,
  so the root module's gates never try to build or run a corpus task.
- **A statement**, written the way a user would actually type it: a symptom or a
  request, not a diff. `TestTaskStatementsDoNotLeakTheFix` keeps them short and
  free of code blocks, because a statement that hands over the change measures
  transcription rather than coding.
- **An oracle**: an argv (never a shell string), its working directory being
  `repo/`, plus the environment it needs and a timeout. Non-zero before the fix,
  zero after.
- **Traits**, which are the corpus composition constraints made checkable:
  `requires_read` for a task that cannot be done without reading first, and
  `multi_file` for one whose fix spans files.
- **Notes**, which say what the task depends on and why it discriminates. For
  humans; never sent to the model.

## Determinism, and what each task needs

An oracle that depends on the network, the clock, the machine, or a toolchain
the CI image lacks turns every downstream A/B number into noise. So:

- **Two toolchains only, both guaranteed.** Go — the harness is written in it, so
  it cannot be missing where `kopibench` runs — and `python3` with the standard
  library, which every CI image and developer machine this project targets has.
  `internal/corpus` rejects a task that declares anything else.
- **No network, structurally.** Go oracles run with `GOPROXY=off` and
  `GOFLAGS=-mod=readonly`; the modules have no requirements, so there is nothing
  to fetch even if the proxy were reachable. Python tasks import only the
  standard library. A test asserts these are set rather than trusting that a task
  happens not to reach out.
- **No clock, no randomness, no filesystem beyond the tree.** No task reads the
  time, generates a random value, or touches anything outside `repo/`.
- **The oracle does not modify the tree it judges.** Python oracles set
  `PYTHONDONTWRITEBYTECODE=1` so a run does not leave `__pycache__` behind and
  change the diff being scored.
- **Byte-stable checkout.** `.gitattributes` marks `bench/**` as binary for
  end-of-line purposes, so a Windows checkout does not silently rewrite every
  file and change the corpus digest.

Each task's `notes` field states its own requirements. The runner should refuse
to score a corpus where a declared requirement is missing rather than skipping
the task, because a skipped task breaks the pairing that ADR-0005 §1 depends on.

## Freezing and versioning

ADR-0005 makes the corpus an experiment-series boundary: results are comparable
within one corpus and pooling across two is a false claim. Two fields carry that.

- **`corpus_version`** identifies the series. Bump it whenever the task set or
  any task's content changes.
- **`digest`** is a SHA-256 over every task file — sorted, length-prefixed,
  slash-separated paths, Markdown and `corpus.json` itself excluded. It is
  computed from what is on disk, so it cannot be wrong by omission.

`corpus.Load` verifies the digest and **fails** when it does not match, which is
what turns "the corpus is frozen" from a convention into something the compiler
of experiments can check. An intentional change is therefore two deliberate
steps: bump `corpus_version`, and record the new digest. The failure message
prints the computed value, so:

```
go test -race -count=1 -run TestLoadRealCorpus ./internal/corpus/
```

is how you get it.

**A result must record both.** `corpus_version` says which series it belongs to;
`digest` proves the bytes were the ones that version names. A result carrying
only the version records an intention, not a fact.

Markdown is excluded from the digest deliberately: fixing a typo in this file
should not invalidate an experiment series.

## The run order is fixed on purpose

`corpus.json` lists the tasks in the order they run, and the loader cross-checks
that list against the directories on disk in both directions — an unlisted
directory and a listed non-directory are both hard errors. ADR-0005 §4's
sequential testing with early stopping needs a stable order to be meaningful,
and directory iteration order is not one.

## Verifying a task in both directions

An oracle that passes on the *unfixed* tree measures nothing and inflates every
arm's pass rate identically, which is the worst kind of broken because it looks
like a working benchmark. One that fails even after a correct fix caps every arm
for reasons that have nothing to do with the model.

`TestOracleFailsBeforeAndPassesAfter` in `internal/corpus` checks both directions
for every task the loader finds, by copying `repo/` to a temp directory, running
the oracle, overlaying the reference solution and running it again. It is behind
the `integration` build tag, so `make test-all` carries it and `make test` stays
fast.

It is not ceremony. Writing this corpus, that test caught three tasks whose
suites could not pass even with the correct fix, one of which had a "before"
direction that was passing vacuously.

## Adding a task

1. Create `bench/tasks/<id>/repo/` with the starting tree and `task.json`.
2. Add the reference fix under `bench/_solutions/<id>/`, as whole files that
   overlay `repo/`. An overlay rather than a patch, so applying one needs no
   patch tool and behaves identically on every platform.
3. Add the id to `corpus.json` in the position you want it to run.
4. Bump `corpus_version`, run the corpus tests, and record the digest the failure
   prints.
5. Run `make test-all`. The new task is verified in both directions because the
   suite discovers tasks from the directory — nothing lists them by hand, so an
   eleventh task cannot be silently unvalidated.

Keep it small. The corpus runs once per arm, an A/B series is many arms, and
`make bench` has to stay near $2.

## Notes for the bench runner

Not built yet — that is KAN-796 — but three constraints belong with the data:

- **The agent's repo root is `bench/tasks/<id>/repo`**, and it must not be
  allowed above it. The reference solutions live outside the corpus tree on that
  assumption.
- **Keep the Go build cache warm.** A worktree per task with a temp `HOME` gives
  each task a cold `GOCACHE`, which turns a two-second suite into a
  thirty-second one, ten times per arm. Pass `GOCACHE`/`GOMODCACHE` through while
  keeping `HOME` temporary.
- **Record `corpus_version` and `digest` in every result**, next to the provider
  pin ADR-0005 §2 already requires.
