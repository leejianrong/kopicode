# KOP-218: naming the assumption when a task is ambiguous

s4 (`s4-ambiguous-request`: "Make the exporter support the other format too", when there is only CSV). On 2026-10-18 qwen did not call `ask`, said "JSON" mid-run (seq 53) and closed with "the exporter now
supports both CSV and JSON" (seq 63), as if the task had been clear. Over MCP `ask` is refused ("no human is present") by design, so the remedy has to work without it. Same setup as the earlier runs:
Claude Code driver (Haiku, `kopicode-driver`), `consent_mode: "auto"`, one run per setting; journals beside this file.

## What was tried

1. **A prompt line** (the closing-summary instruction gains "and any assumption you made because the task was unclear", paid for by shortening the `ask` paragraph).
2. **A reminder on the verification pass line**, which is the last thing the model reads before its closing summary:
   "`<cmd>` (configured) passed. In your closing summary, state any assumption you made where the task was unclear." Only on a pass (a failure or a skip is not the moment anyone is summarising).
   This is the change that ships; the prompt line is not.

| Run | `ask` called | Final message names the assumption | Journal |
| --- | --- | --- | --- |
| qwen3-coder-next, before (2026-10-18, and again today) | no | **no** ("The exporter now supports both CSV and JSON formats", seq 63; today seq 89) | [2026-10-18](../2026-10-18-s4-ambiguous-request-claude-code/), [qwen-before](qwen-before/events.jsonl) |
| qwen, prompt line only | no | **no** (seq 90) | [qwen-prompt-line-only](qwen-prompt-line-only/events.jsonl) |
| **qwen, reminder on the pass line** | no | **yes**: "**Assumption**: ... I assumed JSON as the most common complementary format to CSV ... let me know and I'll adjust" (seq 62) | [qwen-reminder](qwen-reminder/events.jsonl) |
| deepseek-v4.1-flash, before | yes (seq 44, refused) | yes, in substance: "I picked the format myself ... I asked, but no human was available" (seq 127) | [deepseek-before](deepseek-before/events.jsonl) |
| deepseek, prompt line only | yes (seq 61) | yes | [deepseek-prompt-line-only](deepseek-prompt-line-only/events.jsonl) |
| deepseek, reminder | yes (seq 52) | yes ("Assumption: ... I chose TSV ... If you meant JSON ... say which") | [deepseek-reminder](deepseek-reminder/events.jsonl) |

## What it shows, and does not

- deepseek already did the right thing on the old prompt (it called `ask`, was refused, and said so at the end); the change does not move it and does not hurt it. qwen did not, and with the reminder did
  once. One run per setting, so for qwen this is two "no" runs before and one "yes" after: a lead.
- **A side effect.** An unconditional reminder invites an assumption statement on tasks that were clear. On s1 (a failing test, no ambiguity) qwen closed with "Assumption: I assumed the pagination should round up",
  which restates what the test already says ([s1-qwen-reminder](s1-qwen-reminder/events.jsonl)); on s2 it did not ([s2-qwen-reminder](s2-qwen-reminder/events.jsonl)). Harmless, slightly noisy.
- The prompt line did nothing for qwen, so it is not in the change, and the system prompt, its hash and its byte budget are untouched. The pass headline is not in the harness config hash.
- The harness cannot tell that a task is ambiguous, and does not try. The reminder is the model's cue to say what it chose.

Not checked: the Codex driver, other models, how the reminder reads when verification passes several times in one session (it appears on every pass).
