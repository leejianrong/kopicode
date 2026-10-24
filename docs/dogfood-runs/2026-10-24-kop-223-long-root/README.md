# KOP-223: a model mistyping the repository root in `cd <root> &&` (2026-10-24)

On 2026-10-23 s1 on `qwen/qwen3-coder-next` mistyped a 115-character scratch path twice in a redundant `cd <root> &&` and took 12 turns against 6
([final round](../2026-10-23-final-round/README.md), calls at seq 55 and 69). This round asks whether it happens again with a long root, before changing anything.

## Method

s1 (`off-by-one`) on `qwen/qwen3-coder-next`, the Claude Code driver (Haiku, `kopicode-driver`), `consent_mode: "auto"`, a fresh scratch repo per run, on a build of `origin/main` (c9d594a, no code change under test).
Twelve runs, roots of 125 to 141 characters: nine at one path shape (`.../scratchpad/runs/s1-base-<n>/tree`, 125 characters) and three with other labels (129, 140 and 141 characters; the 129 one is `final-s1-qwen`, the label of the failing run).
The earlier run's exact path was not replayed (it sits in another session's scratch directory), so the root's characters differ from it.

## Result

| | runs | `check.sh` | turns | `cd <root> &&` as the first words of a shell call | a failed `cd` |
| --- | --- | --- | --- | --- | --- |
| roots of 125 to 141 characters | 12 | 12 OK | 6 in every run (30.8k tokens, about $0.0028) | 1 per run, 12 in all | **0** |

No session started a command with `cd /testbed`. Every run made one `run_shell` call, `cd <root> && python -m pytest tests/test_pagination.py -v`, copied correctly; it exited 127 in each (`/bin/sh: 1: python: not found`,
this machine has `python3` only), and the verification command in `.kopicode/config.toml` then did the checking. That is a wasted call, but it is not the path and not this card.

## Verdict

Not reproduced, so no change: the `done when` of the card (three runs with a 100+ character root, no failed `cd`) holds on the build as it stands, 12 of 12. The redundant `cd` itself is still there in every run; it is harmless when the
path is copied right. The earlier two mistypes are one run, at one path, and the runs here are at temperature 0 and seed 1, so they repeat each other closely (all twelve took six turns and between 30,751 and 30,921 tokens): that is
closer to one sample per path shape than to twelve independent draws. What it does not rule out is a path whose characters tempt the model to drop the leading `/tmp/claude-1000/` the way the old one did.

If it comes back, the cheaper fix to try first is the one the card names: have the `run_shell` result say the command started in the root, so the model has no reason to `cd` (the environment note already says there is no need to).

## Not checked

One model, one scenario, one driver. Other models were not run. Nothing was tried on a short root.
