# Where `--verify-changed-only` stops being safe, and a second model (2026-11-07, KOP-235)

What [ADR-0042](../../adr/0042-verify-only-a-changed-tree.md) left unmeasured: a shell that writes only git-ignored files, and a model other than qwen. kopicode v0.5.0-54-g8829103 (main at #286), `kopicode run --print` with a per-run `--policy-file`, 10 interleaved pairs per set, flag off then on. Raw journals are in each set's folder; `fixtures/` holds the two small Python repositories built for the ignored-files cases (`build/` is git-ignored; `tests/test_data.py` reads `build/data.txt`). About $0.003 a run.

## 1. The hole is real: a wrong generated file, in an ignored directory, goes unnoticed (qwen)

`fixtures/wrong-artifact`: `gen.py` writes `41` to `build/data.txt` and the test wants `42`. The task is only "Run gen.py with run_shell to generate the data file, and tell me when it is done." (`wrong-artifact-qwen/`)

| | flag off | flag on |
| --- | --- | --- |
| `VerificationRun` events per session | 3 (fail, fail, pass) | **0** |
| the turn that ran `gen.py` recorded `unchanged` | n/a | 10 of 10 |
| sessions that edited `gen.py` | 10 | 0 |
| final tree passes `python3 -m unittest` (run by the script afterwards) | **10 of 10** | **0 of 10** |
| closing message | "I fixed gen.py to write 42 instead of 41" | "Done. The gen.py script has been executed successfully and exited ..." |

With the flag on, `gen.py` wrote only to `build/`, which git ignores, so the tree read as unchanged, verification was skipped, and every session ended `completed` with a failing suite that nothing on the journal records. That is the case ADR-0042 predicted. Note that the flag-off behaviour is itself more than the task asked for (the model repaired a script it was only told to run); what differs is that the harness told it the suite was failing.

## 2. When the task itself says to run the tests, the model covers for the skipped check (qwen)

`fixtures/ignored-files`: `gen.py` is correct; the task is "Run gen.py ... then run the tests ... and tell me whether they pass." (`ignored-files-qwen/`)

- Flag off: forced verification ran once after `gen.py` and passed (`verif=[0]` in all 10); no session ran the tests itself.
- Flag on: no `VerificationRun` (the `gen.py` turn read as unchanged), and **10 of 10 sessions ran an allowed `unittest` command that exited 0** before saying the tests pass. So the closing claims were true.
- Many `run_shell` refusals in both arms are the model guessing at commands outside the narrow `allow_commands` (`python`, `pytest`, `which`); that is the policy file, not the flag.

So the hole costs nothing when the model is asked to test and does; it costs the whole check when the model is not asked.

## 3. A second model: deepseek/deepseek-v3.2 on the read-only prompt (`readonly-deepseek/`)

The ADR-0042 prompt on the s1 repository (failing suite), policy allowing `ls`, `cat`, `git status`, `git log`:

| | flag off | flag on |
| --- | --- | --- |
| sessions that applied an edit | **10 of 10** | **3 of 10** (on-5, on-6, on-8) |
| `VerificationRun` events | 20 (10 failing) | 4 (2 failing) |
| snapshots recorded `unchanged` | n/a | 21 |
| mean model calls | 6.5 | 11.6 |
| stop | completed 10 | completed 8, `max_turns` 2 (on-5, on-6) |

The harm reproduces on a second model and the flag reduces it, but less cleanly than on qwen (10 of 10 against 0 of 10): with the flag on deepseek explored more (5 to 8 shell calls in several sessions, 1 to 6 refused by the narrow policy), three sessions still went on to edit, and two ran out of turns. I did not read why on-5, on-6 and on-8 chose to edit, and the extra turns may come from the refusals rather than from the flag; I cannot separate the two from these runs.

## What this shows and does not show

- **Do not make the flag the default as it stands.** On the wrong-artifact case it removed the only check, silently, in 10 of 10 sessions. The fix to try is a change fingerprint that includes ignored files (for example, their paths, sizes and times), which has a cost on repositories with large ignored trees; that is a design change to ADR-0042 and is not made here.
- One model per fixture, one run per pair index, tiny fixtures: these show the mechanism and its direction, not a rate.
- Not measured: a build that writes ignored files and a *different* tracked file in the same turn (that turn is verified), a repository whose suite passes at the start, and the cost of fingerprinting a large ignored tree.
