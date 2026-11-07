# ADR-0042: Run forced verification only after a turn that changed the tree

*Accepted* 2026-11-07 by the owner, to be built as decided below, with the measurement in "The measurement that decides the default" before any default changes (the skip version, not the no-skip alternative). KOP-233. Builds on forced verification (`Config.Verification`, `internal/engine/loop.go`), on the turn snapshots (`internal/repo/snapshot.go`, "a snapshot of an unchanged tree is recorded rather than skipped; whether the turn touched anything is the caller's judgement") and on [ADR-0039](0039-leftover-name-note.md) for how an opt-in harness change is shaped. It changes when a check runs and what the model is told, so it is a decision.

## Context

Forced verification runs after every turn that ran a tool marked `mutates`. `run_shell` is marked, on purpose: "a shell goes where it likes, and a snapshot taken only after an edit would miss everything a command wrote" (`internal/engine/dispatch.go`). The mark is a statement about what a tool *could* do. Verification treats it as a statement about what the turn *did*.

The consequence, seen live on 2026-10-27 (`docs/dogfood-runs/2026-10-27-repl-tmux-and-s5-protected`, `repl-tmux`): a person in a real REPL asked for `ls -la` and nothing else, in a repository whose test suite failed from the start. The turn ran `ls`; the engine snapshotted and ran the suite (seq 12, exit 1); the model was shown *"verification: ... FAILED, exit 1 — this change is not finished"*. There was no change. The model went on to find and fix the unrelated bug (edits at seq 32 and 48) and ended "The tests now pass". The harness took a session out of its task, which the rubric scores as R9. The same state has a second edge: had the model stopped there, `e.unverified` would have been set and the session would have ended `verification_failed` for work that changed nothing.

How often a turn like that occurs, over the 176 journals committed under `docs/dogfood-runs` (466 turns with a snapshot):

- **95 turns (20 percent) ran `run_shell` and no edit tool.** In the 39 of them that have an earlier snapshot to compare with, the tree is **identical to the previous snapshot in all 39**; for the other 56, the first snapshot of the session, the journal holds no earlier tree, so it cannot say. The commands are `git log`, `ls`, `cat`, `find`, and the model's own test runs.
- **52 of the 95 had a failing verification before the session had applied any edit.** These scenarios are all "make a failing test pass", so most of the 52 are probably the intended case (the model had just run the suite itself); I did not check each. Their verification output was sent to the model as an observation, 34,396 bytes in all, a mean of 661.
- **One of the 95 that I know to be a case where the failure was not the task, and where the model was sent off by it, is the live run above.** I did not read the other 94 for this. The corpus is the scenario families this repo runs, which are all "fix a failing test" or close to it, so it under-represents exactly the sessions this matters for: a person asking for something unrelated in a repository that is not green.

Not measured: how often a model acts on the "not finished" line when the failure is not its task. One live case says it can; it does not give a rate.

Rejected before: nothing on this specific point. [ADR-0017](0017-auto-consent-mode.md) and the shell scanner classify commands for *consent*, and its rule is that what the tokenizer cannot account for is denied; reusing it to decide a command is read-only would make the answer to "did the tree change" a guess about shell syntax.

## Decision

1. **Whether the tree changed is read from the tree, not from the command.** The snapshotter gets `Tree(ctx)`: the same `git add -A` into the throwaway index and `git write-tree` as `Snapshot`, and no commit and no ref. It touches no file the user owns, as today.
2. **A baseline is taken once, lazily.** Just before the first mutating call of a session (before it runs, so a shell's own writes are not in it), the engine calls `Tree` and keeps the id. Afterwards the baseline is the tree of the most recent snapshot. A session that never calls a mutating tool pays nothing; one that does pays one extra index update.
3. **After a mutating turn the snapshot is taken as now, then compared.** If its tree equals the baseline, forced verification **does not run**, the engine appends **no observation**, and `e.unverified` is left exactly as it was (a failure still outstanding stays outstanding; none is created). If the tree differs, everything is as today.
4. **The record says why.** `TurnSnapshot` gets an optional field `unchanged` (true only when the tree equalled the baseline). A reader sees a mutating turn with a snapshot, `unchanged: true` and no `VerificationRun`, and knows the gap was deliberate. An older reader ignores the field.
5. **Off by default.** `Config.VerifyChangedOnly` (bool, in the harness hash only when set), `--verify-changed-only` on the REPL and `run --print`, a declared key `verify_changed_only` (0|1) for `serve` and `mcp`. It becomes default only on the measurement below, the same shape as ADR-0039.
6. **It never changes a stop.** A session that changed nothing ends as it would after a read-only turn. If forced verification is off, nothing changes either way.

### The measurement that decides the default

Two arms, same build, `qwen/qwen3-coder-next`, a scratch repository whose suite fails at the start (the s1 fixture), the prompt **"List the files in this directory and tell me what each one is for."**, 10 runs per arm, cheap (about $0.003 a run). Read from the journals: the count of sessions that applied any edit (the harm), the count of `VerificationRun` events, and the stop. Then s1 to s5 once each with the flag on, to check that a session that does change the tree still verifies. A run where the flag cost a verification the session needed is a reason to reject.

## Consequences

- **The hole: files git ignores.** The snapshot is the tree git would describe, so a shell turn that writes only ignored paths (a build into `bin/`, a virtualenv, a cache) leaves the tree identical, and verification is skipped where today it would run against the new build. This is real: a model that runs `make build` and then expects the forced run to use it loses that run. It is why this is off by default and why the s1 to s5 check is part of the measurement. A narrower rule was considered and rejected for now: skip only when the turn ran no shell command whose exit was 0, which brings back guessing at what a command did.
- A session that has verified nothing can now end `completed` with a failing suite it never touched. That is accurate (it changed nothing) and is different from today's `verification_failed`. A person who wants the suite's state at the end of every session runs their own test command; this gate was never that.
- One more `git add -A` plus `write-tree` per session that mutates, at the first mutating call. The throwaway index is persistent for a session, so later ones are incremental.
- A resumed session takes a fresh baseline at its first mutating call; it does not carry the previous run's, because the tree may have changed between them.
- A new optional field on an existing event, and one new method on the `Snapshotter` interface, which the test fakes will need.

## Alternatives

1. **Say what is true instead of skipping.** Keep running verification, and when the tree is identical to the baseline replace "this change is not finished" with a line that the tree is unchanged since the session began and the failure was already there. No hole, since verification still runs against whatever was built. It keeps the extra suite run and the second copy of its output, and it still shows a model a failing suite on a task that is not about it. It also needs the same baseline, so it is not cheaper to build. A reasonable first step if the owner wants no skipped verification.
2. **Classify the command as read-only.** Rejected above: it is a second allowlist of shell syntax.
3. **Close KOP-233.** One live case in a corpus that cannot show it, and a person can run `ls` in a green repository without harm. The cost of closing is that a cuttlefish-style unattended run in a repository with a pre-existing failure can be taken off its task by its first read-only shell call, and nothing on the journal says the harness did it beyond the `VerificationRun` itself.

## Implementation notes

Settled while building it (KOP-233); none changes a decision above except where it says so.

- **`Snapshotter.Tree(ctx)`** is on the engine's `Snapshotter` interface, as the Consequences said, and on `repo.Snapshotter`. It shares `Snapshot`'s `add -A` into the throwaway index and `write-tree`, writes no commit and no ref, and does not move the chain; a test holds that its id equals the next `Snapshot`'s.
- **The baseline is read in `runTool`**, after the consent gate and before the call runs, for any tool marked `mutates`; so a call that is denied or refused takes none. Afterwards it is the last snapshot's tree, kept whether or not the turn was unchanged.
- **The skip is in the loop, not in `verify`.** A mutating turn is still snapshotted and journaled (`TurnSnapshot.unchanged`); only `verify` is not called, so `e.unverified` is neither set nor cleared (decision 3), and the stop is whatever the model's own stop would have been. A test holds that a failure from an earlier changed turn survives a later unchanged one and the session still ends `verification_failed`.
- **`unchanged` is written only with the setting on**, because the baseline it compares with exists only then.
- **The setting** is `Config.VerifyChangedOnly`, `--verify-changed-only` on the REPL and `run --print`, `verify_changed_only = 0|1` in a declared config; in the hash only when set.
- **The ignored-files hole is as described.** Nothing was added to mitigate it.
- **First measurement ([`2026-11-07-kop-233-verify-changed-only`](../dogfood-runs/2026-11-07-kop-233-verify-changed-only/README.md)):** qwen on the s1 repository with its failing suite, prompt "Run the command ls -la with run_shell and tell me what files exist", 10 pairs. Flag off: 10 of 10 sessions applied an edit (15 edits to the unrelated bug), 25 verifications. Flag on: 0 of 10 edited, 0 verifications, 10 of 10 turns recorded `unchanged`, 2.0 model calls against 6.1. s1 to s4 with the flag on still verify after every edit and pass `check.sh`; s5 made no change. A first prompt that asked for a description of the files never called a shell (20 of 20), so it could not show the effect. The ignored-files hole and other models are unmeasured, so the default stays off until the owner decides.
