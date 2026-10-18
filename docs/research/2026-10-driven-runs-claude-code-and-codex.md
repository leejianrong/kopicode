# Driving kopicode from Claude Code and from Codex (KOP-212)

Checked 2026-10-18. Each client ran scenario `s1-off-by-one` ([`docs/agent-driver/scenarios/`](../agent-driver/scenarios/)) in a throwaway repo, through `kopicode mcp`. kopicode v0.5.0-14-g5b8f4af
(`qwen/qwen3-coder-next`, the default), Claude Code 2.1.302 on Haiku, codex-cli 0.155.1 on a ChatGPT login (`gpt-5.6-luna`, low effort). One run per client, plus the
reruns noted. Neither client's own configuration file was touched: both were pointed at kopicode on the command line, and the OpenRouter key reached kopicode
by reference (`${OPENROUTER_API_KEY}` for Claude Code, `env_vars` for Codex), never typed into a file or command.

**Result: both drive kopicode end to end.** In each run the client called `kopicode_start` once with the task text character for character and
`consent_mode: "auto"`, kopicode finished (`stop: completed`, exit 0), `scenarios/check.sh` returned OK on the tree, and the session was closed with `kopicode_close`.

| | Claude Code (subagent `kopicode-driver`) | Codex (`codex exec`) |
| --- | --- | --- |
| Wiring | `--mcp-config <file> --strict-mcp-config`, a one-server JSON with `env: {OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"}` | `-c mcp_servers.kopicode.command=… -c mcp_servers.kopicode.args=["mcp"] -c mcp_servers.kopicode.env_vars=["OPENROUTER_API_KEY"]` |
| Needed to run headless | `--allowedTools "Task" "mcp__kopicode" "Read" "Bash(git -C:*)" …`: a tool outside the list is refused, with no one to ask | `-c mcp_servers.kopicode.default_tools_approval_mode="approve"`. Without it the call fails: "MCP tool call requires approval, but approval policy is never" |
| kopicode's side | 6 turns, 29,249 tokens, $0.0028 | 5 turns, 24,679 tokens, $0.0022 |
| Driver's side | $0.0085 (the run before the fixes below) and $0.0119 (after), 6 and 9 turns | 82k input tokens (68k cached), no per-token cost on a subscription |
| Report | thin on the first run, complete on the second (see below) | exactly the requested block on the first run that got through |

## What was found

1. **Codex `exec` refuses MCP calls by default.** The first attempt ended with the agent saying "kopicode_start was blocked", no kopicode session opened and no cost. Setting the
   server's `default_tools_approval_mode` to `approve` fixed it. A search turned up a Codex issue where that setting alone did not stop a cancellation under a closed stdin;
   on this version it worked.
2. **Claude Code's first report was thin, for two reasons that were ours.** The subagent's `git -C <dir> status` was refused because only `git status` was
   allowed, and it tried to read the journal as `<record>/journal.jsonl`; `record` is a directory and the journal is `events.jsonl`. Both are fixed in the subagent file
   (`e74ce62`) and the rerun reported correctly.
3. **The subagent kept the task verbatim even when the parent paraphrased it.** The parent session's prompt to the subagent wrapped the task in its own instructions and
   asked for "the final summary and the test result"; the `kopicode_start` call carried only the original text. The pass-through rule held in the subagent. The parent then did
   what the rule forbids a driver: it tried to restore a blank line the edit had removed, and to run the tests. Both were refused in a headless run. This is the case the rule exists for, and
   the README now says to give the subagent only `dir` and `task`.
4. **kopicode's own session was the same under both drivers**: 5 to 6 turns, under 30k tokens, the fix landed in `shop/pagination.py` only, tests untouched.

## Not tested

`consent_mode: "remote_interactive"` (needs the client to declare MCP elicitation), `kopicode_submit` for a second turn, `kopicode_cancel`, `kopicode_handoff`, progress
notifications (neither client showed them to the model), a driven run on a scenario that is meant to fail (s4, s5), several runs at once, and Codex or Claude
Code on a model other than the two named. The Claude Code result is for the subagent route; plain `claude mcp add` was not tried.
