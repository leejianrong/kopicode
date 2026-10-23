# s3-rename-across-files after KOP-215

The rerun KOP-215 asked for, against the runs in [`2026-10-18-s3-rename-across-files-claude-code/`](../2026-10-18-s3-rename-across-files-claude-code/README.md). Same
driver (Claude Code, Haiku, the `kopicode-driver` subagent), same model (`qwen/qwen3-coder-next`, temperature 0, seed 1), same scenario, 2026-10-23, one run per setting.
Journals: `control/events.jsonl` (kopicode built from `origin/main` 53099f5) and `after/events.jsonl` (that plus the change below).

## The change

An anchor hashes a line with both neighbours (ADR-0006 decision 7), so an edit re-anchors the line above and the line below it without touching their text. The
result of an applied `edit_file` now ends with those two lines as `old -> new anchor, line, text`, and the system prompt says so in one bullet. Nothing about matching changed: a
stale anchor is still refused as `anchor_drift` and the file untouched (`TestEditReportsTheNeighboursItReanchored` asserts both the mapping and the refusal).

A first version printed the new anchors of the edited lines and both neighbours as a plain list. It was measured and dropped: the model spent two of the listed anchors on the wrong
line, and those edits **applied**, because a fresh anchor is valid wherever it points (a first-version run: `check.sh` FAIL, 13 turns, a stray comment and a blank line
swallowed). A refusal is recoverable and a wrong-line edit that lands is not, so the shipped form only maps an anchor the model already holds.

## Result

| | control (before) | after |
| --- | --- | --- |
| `check.sh` | OK | OK |
| `EditRejected` as `anchor_drift` | 3 (seq 42, 56, 70) | 1 (seq 72) |
| `EditApplied` | 11 | 14 |
| provider turns | 7 | 10 |
| tokens | 48,099 | 84,110 |
| `VerificationRun` failed / total | 2 / 3 | 3 / 5 |
| `SessionEnded` | completed, exit 0, seq 124 | completed, exit 0, seq 159 |

The `accounts/store.py` rejection is gone: the `def` line was edited in one turn and the docstring under it in the next (control: applied seq 32, then refused seq 42; after: applied seq 32, applied seq 42), and in the
after run the model used the mapped anchor and the edit applied. The other two rejections in the control, and the one left (seq 72, `tests/test_accounts.py`), are the same
shape inside one turn: the model sends nine `edit_file` calls in a single reply (control `ToolCallParsed` seq 41 to 78, after seq 41 to 80, each after one `ProviderRequest`, seq 37), so a later call is written before
the earlier call's result exists. Nothing returned in a result can reach a call already written, and loosening the match is out of scope. A sentence telling the model to change adjacent lines in
one edit, and that calls in one reply do not see each other, was tried in a third run (`check.sh` OK, 7 turns, 12 edits applied, still one rejection at seq 72, the same
edits through seq 81), so it did not change the behaviour and was not kept.

## What this does not show

- **It is not a clear win.** Rejections fell from 3 to 1, but the after run used more turns, more edits and more tokens than the control. Most of that is the model: in both runs it issued edits
  whose anchor named a different line from the one it meant, and those applied (control seq 74, after seq 52, 62 and 76 replaced a `def` or `with` line), then it re-read and repaired the files. The after run
  had more of those; whether that is the change or the sampling is not known from one run each.
- One run per setting, one model, one driver. The Codex driver and `deepseek/deepseek-v4.1-flash` were not rerun for this card.

## Afterwards

The final round on the merged build ([`2026-10-23-final-round`](../2026-10-23-final-round/README.md)) had **3** rejections on s3 again, all in one reply. The run above that showed 1 was within this scenario's spread
(control 3, here 1, later 3); what the change fixed is the cross-turn case only, and the in-reply case is KOP-222.
