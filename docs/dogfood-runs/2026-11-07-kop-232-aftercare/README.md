# `--edit-aftercare` on s1, 10 runs a side (2026-11-07, KOP-232)

The measurement [ADR-0040](../../adr/0040-edit-left-code-behind-note.md) said to make first: does telling the model that an edit left code that can never run make it fix it? kopicode v0.5.0-52-g08a9b03 (main at #284), `qwen/qwen3-coder-next` (the default), `kopicode run --print` over a fresh s1 scratch repo per run (`scenarios/setup.sh`), the unchanged `task.txt`, with and without `--edit-aftercare`. Ten pairs, interleaved (off, on, off, on, ...). Raw: `off-N/` and `on-N/` hold `events.jsonl`, `check.txt` (`check.sh`) and `dead.txt` (`dead.py` on the final `shop/pagination.py`: `dead` when a `return` or `raise` has a statement after it in the same block). Cost: about $0.003 a run.

## Result

| | off | on |
| --- | --- | --- |
| `check.sh` passes | 10 of 10 | 10 of 10 |
| runs with an applied edit that left dead code (`EditApplied.unreachable`) | 3 (off-2, off-3, off-9) | 3 (on-2, on-4, on-6) |
| final `shop/pagination.py` has dead code | 3 | 3 |
| mean turns | 8.6 | 8.7 |

In the three `on` runs where it applied, the note reached the model (it is in the `ToolResult` of the edit: `can never run` appears once in each journal; it appears in none of the `off` journals). In all three the next event is a passing forced verification (seq 48), then the model's closing message (seq 51) saying the test passes; **no edit followed**, so the dead code stayed. The closing messages describe the `//` to ceiling-division fix and do not mention the leftover.

## What this shows and does not show

- **Not that the note works.** With 3 firings and 0 repairs the data fits "qwen ignores it when verification passes" and does not exclude a small effect. It does not support turning the note on by default, which ADR-0040 said only a measurement could do. It stays off.
- **That the damage is common on this task, with or without the note.** 6 of 20 final trees (30 percent) have dead code and every one passes `check.sh`. Earlier replays over committed journals found it in 20 of 175 sessions (11 percent), so s1 on the current build is at the high end of that.
- **Not checked:** other models, a note worded as an instruction rather than a statement, or showing it again at the closing turn. The note arrives on the edit's result, on the turn before a passing verification tells the model the work is done; where it sits is a guess about why it is ignored, not something these runs test.
- The `dead.py` check counts any statement after a `return` or `raise` in a block, which is broader than the detector (the same indentation and an edit-added line); the two name the same six runs of the twenty.
