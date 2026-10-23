# KOP-219: a verification line in the driver's report

A run with no verify command reports `stop: completed, exit_code: 0` while every `VerificationRun` was skipped (the textkit trial, seq 18 onward), and a driver reading only those cannot tell. The
`kopicode-driver` subagent and the Codex prompt in [`docs/agent-driver/README.md`](../../agent-driver/README.md) now ask for one more line, read from the last `VerificationRun` in the journal
(the subagent gained `Grep` for it).

Two driven runs of `s1-off-by-one` on 2026-10-23 (Claude Code driver, Haiku; `qwen/qwen3-coder-next`; journals beside this file), each with the line the driver reported:

| Setup | The driver's line | The journal |
| --- | --- | --- |
| the scenario's verify command | `verification: ran, last exit_code 0 at seq 37` (first wording; it also said "1 run") | `VerificationRun` seq 13 exit 1, seq 37 exit 0: the line was right, the count of runs was not, so the count was dropped from the format |
| `.kopicode/config.toml` removed | `verification: skipped: no_command at seq 65, so nothing was checked` | five `VerificationRun`, seq 27, 36, 45, 55, 65, all `skip: no_command`, `exit_code: -1` |

In the second run the session's own final message claimed the tests pass; the driver's report is what says nothing was checked. Not run: the Codex prompt (its wording is the
same `grep ... | tail -1`, not driven), and a driver on a different model.
