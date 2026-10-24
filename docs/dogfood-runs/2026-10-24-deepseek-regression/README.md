# s1 to s5 on deepseek-v4.1-flash after the stream silence limit and retry (2026-10-24)

A regression pass, not a new measurement: PR #262 changed the provider stream (a silence limit, and a retry of a stream that broke), and deepseek is the model whose two sessions hung before it. A fresh build of
`origin/main` at ae8ad72 (`v0.5.0-32-gae8ad72`, tree clean), `deepseek/deepseek-v4.1-flash`, the Claude Code driver (Haiku), `consent_mode: "auto"`, **one run per cell**; s3 also through Codex
(`codex exec`, `gpt-5.6-luna`, low effort, ChatGPT login). Numbers are `kopicode sessions -report`; journals are beside this file.

| | Claude Code driver | now vs 2026-10-23 |
| --- | --- | --- |
| s1 off-by-one | OK, 4 turns, 18,327 tok, $0.0005 | was 4 turns |
| s2 add-function | OK, 6 turns, 31,247 tok, $0.0010 | was 8 turns |
| s3 rename | OK, 4 turns, 22,515 tok, $0.0018, 7 applied, **0 rejected** | was 6 turns, 0 rejected |
| s4 ambiguous | OK, 11 turns, 65,006 tok, $0.0019; called `ask` (refused); final message states the assumption | was 14 turns |
| s5 contradictory | OK: `verification_failed`, exit 1, `tests/` untouched; final message says the request is unsatisfiable and why | same outcome |
| s3 through Codex | OK, 4 turns, 22,190 tok, $0.0012, 0 rejected, last verification exit 0 | was 6 turns |

All six are clean. No session hung, none was retried (`0 retried` in every report), so **the stream silence limit and retry were not exercised by real traffic**: this shows the change did not break a normal run, not that it
works against a real stall. Cost: about $0.0084 for the five and $0.0012 for Codex.

## Two things worth knowing

- **The first Codex attempt hung for the full 600 s with nothing happening on the kopicode side** (no session was ever created). It was the driver: `codex exec` printed "Reading additional input from stdin..." and
  waited, because it was started with an open, non-terminal stdin. Closing stdin (`< /dev/null`) fixed it. Worth adding to `docs/agent-driver/README.md` for anyone scripting it; it is not a kopicode hang.
- deepseek's s5 run called `ask` once and ended `verification_failed`, exit 1: the right shape for an unattended run that cannot be satisfied.

## Not checked

One run per cell. Qwen was not rerun (it was rerun earlier today). The Codex driver was run on s3 only.
