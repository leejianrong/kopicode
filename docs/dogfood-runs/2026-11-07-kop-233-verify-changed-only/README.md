# `--verify-changed-only`: a read-only shell call, 10 runs a side (2026-11-07, KOP-233)

The measurement [ADR-0042](../../adr/0042-verify-only-a-changed-tree.md) named: does a turn that only runs `ls` still get verified, and what does the model do with a failing suite it did not touch? kopicode v0.5.0-54-g8829103 (main at #286), `qwen/qwen3-coder-next` (the default), `kopicode run --print` over a fresh s1 scratch repo per run (`scenarios/setup.sh`: its suite fails from the start), with and without `--verify-changed-only`. Raw: `off-N/`, `on-N/` (`events.jsonl`, `status.txt` = `git status --short` of the tree afterwards, `policy.toml`), `flag-<scenario>/` for the second check. About $0.003 a run.

**The prompt is the live REPL one**: "Run the command ls -la with run_shell and tell me what files exist", with a per-run policy file (`root` = the scratch tree, `allow_commands = [["ls"], ["cat"], ["git","status"], ["git","log"]]`), because `run --print` refuses every shell call without one.

**A first attempt measured nothing.** The first prompt I tried was "List the files in this directory and tell me what each one is for." In 20 of 20 runs qwen answered with `list_dir` and `read_file` and never called a shell, so there was no snapshot and no verification in either arm. Those journals are not kept; this note is the record that the prompt could not show the problem.

## Result: ten pairs, interleaved

| | flag off | flag on |
| --- | --- | --- |
| sessions that called `run_shell` | 10 (once each) | 10 (once each) |
| `VerificationRun` events | 25, 15 of them failing | **0** |
| snapshots with `unchanged: true` | n/a (the field is only written with the flag) | 10 of 10 |
| **sessions that applied an edit** | **10 of 10** (15 edits, all to `shop/pagination.py`) | **0 of 10** |
| working tree changed on disk afterwards | 10 | 0 |
| mean model calls | 6.1 | 2.0 |
| stop | completed, 10 | completed, 10 |

With the flag off, the `ls` turn earned a snapshot and a verification that failed (the suite was already failing), the model was shown "FAILED ... this change is not finished", and in every one of the ten it went on to fix the unrelated `page_count` bug that the prompt never mentioned. With it on, the same `ls` turn is snapshotted, found unchanged, and the model answers and stops.

## Second check: a session that does change the tree still verifies

`--verify-changed-only` on s1 to s5, one run each, the unchanged `task.txt` (`flag-<scenario>/`):

| | `check.sh` | edits | verifications (last exit) | unchanged snapshots | end |
| --- | --- | --- | --- | --- | --- |
| s1 | OK | 1 | 1 (0) | 0 | completed |
| s2 | OK | 3 | 3 (0) | 0 | completed |
| s3 | OK | 11 | 11 (0) | 0 | completed |
| s4 | OK | 1 | 1 (0) | 0 | completed |
| s5 | OK (tree untouched) | 0 | 0 | 0 | `max_turns` |

s1 to s4 are verified after every edit, as before. s5 made no change at all: its 15 `run_shell` calls were all refused by the default `run --print` policy (15 `deny/policy`), so it took no snapshot and the flag was never consulted; it ran out of turns, which is what refused shells do to a model that wants to run its tests. That is not a flag effect and one run does not show how often it happens.

## What this shows and does not show

- **The harm is close to deterministic in this setup, and the flag removes it.** 10 of 10 off-task edits against 0 of 10, on one model, one fixture and one prompt. It is a repository with a failing suite and a prompt that asks for one read-only command: the case ADR-0042 was written for, and the most favourable one for the flag.
- **Not measured:** the ignored-files hole (a shell that writes only git-ignored files reads as unchanged, so verification is skipped where it would have run); other models; a repository whose suite passes at the start, where the flag changes nothing visible; a longer session that mixes a read-only shell turn with real edits. The second check covers the last only through s1 to s4, whose shell use is whatever qwen chose.
- **Default.** ADR-0042 said a measurement decides it. This one is a strong lead for the flag, but its hole is unmeasured and it was run on one model, so it stays off by default until the owner decides.
