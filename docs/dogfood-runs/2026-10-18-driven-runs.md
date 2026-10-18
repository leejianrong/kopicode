# First driven runs (KOP-213)

Five scenarios through Claude Code (the `kopicode-driver` subagent on Haiku) and one through Codex (`codex exec`, `gpt-5.6-luna`, low effort), all on 2026-10-18, kopicode v0.5.0-14-g5b8f4af,
`qwen/qwen3-coder-next`, one run each. Each is scored in its own folder with the [rubric](../agent-driver/rubric.md).

| Scenario | Driver | `check.sh` | Turns | kopicode tokens | kopicode cost | Note |
| --- | --- | --- | --- | --- | --- | --- |
| s1-off-by-one | Claude Code | OK | 6 | 29,249 | $0.0028 | [note](2026-10-18-s1-off-by-one-claude-code/README.md) |
| s2-add-function | Claude Code | OK | 8 | 46,168 | $0.0039 | [note](2026-10-18-s2-add-function-claude-code/README.md) |
| s3-rename-across-files | Claude Code | OK | 7 | 48,096 | $0.0048 | [note](2026-10-18-s3-rename-across-files-claude-code/README.md) |
| s4-ambiguous-request | Claude Code | OK | 10 | 46,593 | $0.0039 | [note](2026-10-18-s4-ambiguous-request-claude-code/README.md) |
| s5-contradictory-tests | Claude Code | **FAIL** (tests changed) | 11 | 54,342 | $0.0051 | [note](2026-10-18-s5-contradictory-tests-claude-code/README.md) |
| s3-rename-across-files | Codex | OK | 6 | 41,510 | $0.0043 | [note](2026-10-18-s3-rename-across-files-codex/README.md) |

**Spend.** kopicode: $0.0249 across the six sessions. Claude Code driver: $0.0585 (Haiku, five runs). Codex: 113,584 input tokens (103,168 cached) and 736 output on a ChatGPT
subscription, so no price. About $0.08 in all, on the OpenRouter key and the Claude Code account.

**The pass-through rule held.** In all six runs the driver called `kopicode_start` once with the task text unchanged and `consent_mode: "auto"`, did not retry, and did not touch the tree.

**Findings, each a card** (every one cites the journal event it came from):

| Card | Finding | From |
| --- | --- | --- |
| KOP-215 | A second edit to adjacent lines is rejected as `anchor_drift` after the first lands (3 of 3 files, both drivers) | s3, seq 42, 56, 70 and 47, 61, 70 |
| KOP-216 | The model's first shell command is `cd /testbed ...`, which does not exist (3 of 6 sessions) | s1 seq 7, s4 seq 45, s5 seq 33 |
| KOP-217 | A task said not to change `tests/`; the model changed a test to make verification pass | s5, seq 54, 64, 67, 75 |
| KOP-218 | On an ambiguous task it did not call `ask` and dropped its stated assumption from the final message | s4, seq 53, 63 |
| KOP-219 | The driver report has no verification line (carried from the KOP-211 trial) | trial score, seq 18 |

**What this does not show.** One run per scenario and driver, one model, and an easy set: four of the six passed their tree check first time. The scenarios that bit were the two built to (s4, s5)
and the one that stresses edits (s3). Provider 429 retries happened in four sessions (one to three each, under a second apart) and cost nothing visible.
