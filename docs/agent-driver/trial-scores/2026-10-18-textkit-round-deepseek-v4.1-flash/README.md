# Trial score: one round of the textkit task (KOP-211)

A trial of the [rubric](../../rubric.md) on an existing journal, not one of the five scenarios and not a driven run: it was written by
`cuttlefish run-team` (the third of six rounds, `max_turns=12`), so `check.sh` does not apply. The journal is `events.jsonl` beside this file.

```
task: build the nine-module textkit package, five unittest tests per module, a commit each (cuttlefish round 3 of 6)
model: deepseek/deepseek-v4.1-flash   kopicode: v0.5.0-14-g5b8f4af   driver: cuttlefish (not Claude Code or Codex)   date: 2026-10-18
outcome: stop max_turns (exit 4), 12 provider turns, 156,408 tokens, $0.0096
```

| # | Score | Evidence |
| --- | --- | --- |
| R1 ended how | concern | `SessionEnded` seq 135: `max_turns`, exit 4. Expected here: the round was capped at 12 turns on purpose |
| R2 verification | concern | all 11 `VerificationRun` events (first at seq 18) have `skip: no_command`, `exit_code: -1`: the scratch repo had no verify command, so forced verification never ran. The model ran its own tests five times with `run_shell` |
| R3 edits | ok | 4 applied (seq 82, 87, 97, 111), 2 rejected (seq 72 on `textkit/diffs.py`, seq 102 on `tests/test_diffs.py`, both `anchor_drift`); each path had an applied edit afterwards (seq 82, 111) |
| R4 tool calls | ok | all 18 `ToolCallParsed` are route `native`, encoding `json_string`: nothing needed repair |
| R5 syntax gate | ok | 4 runs (seq 83, 88, 98, 112), all `exit_code` 0 |
| R6 permissions | ok | 5 `PermissionDecided`, all `allow` from source `remote`; no denial, nothing repeated |
| R7 looping | ok | no call repeated with the same result; the two rejections were followed by a read and a different edit |
| R8 final message | n/a | the last `AssistantMessage` is seq 79; the session then ran tool calls to the turn cap with no closing message, so there is no claim to check |
| R9 scope | ok | edits touched only `textkit/diffs.py` and `tests/test_diffs.py`, which the round's task covers |
| R10 cost | ok | 12 turns for the module the round was in; $0.0096 |

Findings:

1. **No verification ran, and nothing said so to the person driving it** (R2, seq 18 onward). A project with no detectable test command gets `skip: no_command`
   on every turn; a driver reading only the outcome (`stop`, `exit_code`) would see a clean run. The `skip` text is clear in the journal, and the rubric now
   asks for it, but the driven-run report block has no field for "verification: did not run". Candidate for the report block, not a kopicode change.
2. **Two `anchor_drift` rejections on a file the same session had just edited** (seq 72, 102). The messages say the anchor "matches no line in the file as it is
   now". The model recovered both times, so it is a cost (a turn each) and not a failure. Whether the drift came from the model editing against a stale read
   or from the file changing under it was not checked; the journal has the reads and edits to settle it.
3. **A round that hits the turn cap ends without a closing message** (seq 79 to 135). By design in `max_turns` rounds, but R8 cannot be scored for one.

None of these is filed as a card yet: they come from a cuttlefish round and not a driven run, so they are a check on the rubric (it produced concrete,
cited answers for nine of ten questions) and not a verdict on kopicode.
