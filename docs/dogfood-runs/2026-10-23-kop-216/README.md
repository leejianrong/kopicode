# KOP-216: telling the model where it is

In 3 of 6 first driven sessions the model's first shell command was `cd /testbed && python -m pytest ...`, a path from other benchmarks that does not exist here
(s1 seq 7, s4 seq 45, s5 seq 33 in [`2026-10-18-*`](../2026-10-18-driven-runs.md)). It fails at once and costs a turn. Same setup as those runs: Claude Code driver
(Haiku, `kopicode-driver`), `qwen/qwen3-coder-next`, 2026-10-23, one run per setting; journals beside this file.

## What the model was told

The prompt said `dir` is "relative to the repository root" and nothing about where the shell starts or what the root is called. The root's path was never in the conversation.

## What did not work, and what did

| Change | s1 | s4 | s5 |
| --- | --- | --- | --- |
| none (the 2026-10-18 runs) | `cd /testbed` seq 7 | seq 45 | seq 33 |
| **prompt**: "starting in the repository root: never `cd` to a path you were not shown (there is no /testbed)" | `cd /testbed` seq 21 | seq 43 | seq 33 |
| prompt **and** the `command` argument's description in the tool schema | `cd /testbed` seq 22 | not run | not run |
| **the real root, told once as a message** (this PR) | `cd <the real root>` seq 8, succeeded | seq 44, same | seq 30, same |

Telling the model there is no `/testbed` did not stop it, in either place it was said (`wording-only/`); that habit is stronger than a sentence, at temperature 0 on this model. Telling it the true
path did: no shell command in any of the three sessions names `/testbed` (`jq -r 'select(.type=="ToolCallParsed" and .payload.tool=="run_shell")|.payload.args.command'`), and no result says
"No such file or directory". The model still starts commands with `cd <root> &&`, which is redundant and harmless.

## The change

A new session journals one `ProjectInstructionsLoaded` event with scope `environment` right after `SessionStarted`, and the model is shown it as a first user message:
"Environment: the repository root is `<path>`. run_shell starts there and every relative path is relative to it, so there is no need to cd." It follows the skills catalogue's route (ADR-0025
decision 6): no new event type, replayed from the journal on resume and fork (both open in the same root), and **not** in `Config.SystemPrompt`, so the harness config hash does not move, the
prompt budget is untouched and the benchmark arms are as they were. `kopibench` builds its engine directly and does not send it. Prefix stability holds
(`TestEveryRequestExtendsThePreviousOneByteForByte` passes): the note is fixed for the session.

## Result

| | before (2026-10-18) | after |
| --- | --- | --- |
| first shell command names `/testbed` | s1, s4, s5 | none |
| s1 turns / tokens | 6 / 29,249 | 6 / 30,753 |
| s4 turns / tokens | 10 / 46,593 | 10 / 48,214 |
| s5 turns / tokens | 11 / 54,342 | 11 / 66,699 |
| `check.sh` | OK, OK, FAIL | OK, OK, FAIL (`tests/` changed, as before: KOP-217) |

The fix removes the failed command but not the turn count: the three sessions took the same number of turns. The model reaches the same test run one call later or earlier depending on the run, so a
turn saved is not shown here. The after runs include the KOP-215 anchor-mapping change (merged in between), which the control for this card did not have.

## Not checked

One run per setting, one model, one driver. Whether a model other than qwen needs this is not known; the note is cheap (about 40 tokens, once). Windows paths were not tried (`Root.Path()` is the
platform's own form). The Codex driver and `deepseek/deepseek-v4.1-flash` were not run for this card.
