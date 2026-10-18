# Driving kopicode from another agent

How a Claude Code or Codex session runs a task through kopicode and reports what happened, so a harness
change gets a real test. The wire is [`kopicode mcp`](../kopicode-mcp.md); this page is the practice around it.
Scoring a run is a separate step (the rubric card, KOP-211) and is not described here.

## The rule: pass the task through

The driver chooses the task, may cancel, and reports. It does not rewrite the task, hint, retry with a
better prompt, edit the tree, or answer for kopicode. A strong driver that helps makes kopicode look good, and then
the run measures the driver. Anything the driver would have done to help is a finding instead: write it down
and give it to the person.

`ask` is part of this. Over MCP a model's `ask` call gets the fixed "no human is present" refusal unless the
process was started with `--ask-policy-file`; the driver cannot answer it, and that is the harness behaving as it
does for any unattended caller. Only `serve` can put `ask` to a client live (`ask_mode: "remote"`).

## Safety: a scratch tree, always

kopicode has no sandbox ([ADR-0008](../adr/0008-shell-isolation-accepted-risk.md)) and the driver starts it in
`consent_mode: "auto"`, so shell commands run with your privileges inside `dir`. Point `dir` at a scratch
git repository or a worktree leased with `treehouse get --lease`, never at a checkout you care about. A worktree
shares `.git/config` and refs with its parent
([agent-ground-rules](../../.claude/skills/agent-ground-rules/SKILL.md)); a throwaway `git init` directory does not, so prefer that
for scenarios.

## Wiring

The credential is read from the environment kopicode is launched with. Do not type the key into a command or a
config file you will paste anywhere; pass it by reference.

Claude Code:

```bash
claude mcp add kopicode -- kopicode mcp          # key from the environment Claude Code starts in
```

then use the subagent in [`.claude/agents/kopicode-driver.md`](../../.claude/agents/kopicode-driver.md) (a project
subagent, picked up in this repository; copy it to `~/.claude/agents/` to use it elsewhere), for example:

> Use the kopicode-driver subagent. dir: /tmp/scratch-repo. task: "fix the failing test in ./parser".

Codex CLI (a custom prompt is not needed; give it the text under "Codex prompt" below as the task wrapper):

```bash
codex mcp add kopicode -- kopicode mcp
```

Both are the same `command: kopicode, args: [mcp]`. Whether each client really drives kopicode end to end is checked
by KOP-212; until that card is done, treat both lines as untested here.

## Codex prompt

```
You are driving kopicode through its MCP server. Do not write code yourself.
Call kopicode_start once with dir=<DIR>, consent_mode="auto" and prompt set to the TASK below, character for character.
When it settles, read stop, exit_code, turns, usage and record from the result, run `git status` and
`git log --oneline` in <DIR>, call kopicode_close, and report those and nothing else.
You may call kopicode_cancel if the session runs without bound. You must not rewrite the task, add hints, retry with a
different prompt, edit files in <DIR>, run its tests, or answer on kopicode's behalf. If something outside kopicode blocks the
run, say so and stop.

TASK:
<TASK TEXT>
```
