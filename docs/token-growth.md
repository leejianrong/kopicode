# Per-turn token growth on real dogfood tasks

- **Observed:** 2026-08-23, live OpenRouter traffic, `qwen/qwen3-coder-next`, default
  harness config
- **Card:** KAN-935 (the runs), KAN-936 (the journal review), KAN-947 (this writeup)
- **Rule it satisfies:** SLICE-1's naive-full-history decision (`internal/engine`'s own
  doc comment: "a compaction strategy chosen without session data would be a guess") —
  this is that data, for the first time from something other than the frozen corpus.

**Read the numbers below for what they are.** Two sessions, one model, one harness
config. This is not enough to fit a growth model or set a compaction threshold — it is
enough to answer the one question KAN-948 is blocked on: does context grow smoothly
enough that "trim the last N turns" would work, or is growth bursty and driven by
specific tool outputs that a compaction strategy needs to target instead of history
length in general. It's the latter, clearly, from n=2.

## The two tasks

Both are real bugs in real, unmodified upstream repositories (not corpus tasks), found
by reading source and reproducing before writing the prompt — not filed GitHub issues,
except where noted. Full task setup, review, and a third finding (a harness gap, not a
token-growth one) are on KAN-936.

| | Repo | Bug | Prompt gave the model | Outcome |
|---|---|---|---|---|
| Go | [dustin/go-humanize](https://github.com/dustin/go-humanize) `ordinals.go` | `Ordinal(-1)` returns `"-1th"`, should be `"-1st"` — Go's `%` keeps the dividend's sign and the function never normalizes it | The observed-vs-expected behavior only, no hint at the cause | Fixed correctly, turn 9/9, all tests pass |
| JS | [visionmedia/bytes.js](https://github.com/visionmedia/bytes.js) `index.js` | `bytes(1075, {decimalPlaces: 3})` → `'1.050KB'` but `bytes(1536, {decimalPlaces: 3})` → `'1.5KB'` — trailing-zero regex only strips when the first fractional digit is non-zero ([upstream issue #69](https://github.com/visionmedia/bytes.js/issues/69), unfixed at time of writing) | The observed-vs-expected behavior only | Fixed correctly by round-trip 9/22, all tests pass — but the session itself did not end cleanly (see KAN-936: it never let go of a `run_shell rm` on its own scratch file, and hit `max_turns`) |

Raw session output (the `run --print` schema-1 stream, one line per journal event):
[`dogfood-runs/2026-08-23-go-humanize-and-bytesjs/go-humanize-ordinal.jsonl`](dogfood-runs/2026-08-23-go-humanize-and-bytesjs/go-humanize-ordinal.jsonl)
and [`.../bytesjs-decimalplaces.jsonl`](dogfood-runs/2026-08-23-go-humanize-and-bytesjs/bytesjs-decimalplaces.jsonl).

## Token growth, round-trip by round-trip

"Round-trip" here, not "turn": a `tool_call_repaired` event can trigger a second
provider call within the same engine turn (it happened twice in the bytes.js run), so
turn numbers repeat but every row below is a distinct request actually sent to
OpenRouter, in order.

### go-humanize (9 engine turns, 10 round-trips, clean finish)

| # | turn | prompt | completion | total |
|---|---|---|---|---|
| 1 | 1 | 3,766 | 42 | 3,808 |
| 2 | 2 | 4,171 | 38 | 4,209 |
| 3 | 3 | 4,670 | 37 | 4,707 |
| 4 | 4 | 5,395 | 165 | 5,560 |
| 5 | 5 | 5,611 | 38 | 5,649 |
| 6 | 6 | 5,700 | 37 | 5,737 |
| 7 | 7 | 5,788 | 549 | 6,337 |
| 8 | 8 | 7,284 | 49 | 7,333 |
| 9 | 9 | 8,149 | 206 | 8,355 |
| 10 | 10 | 8,435 | 227 | 8,662 |

Total cost: **$0.0057**.

### bytes.js (20 engine turns, 22 round-trips, hit `max_turns`)

| # | turn | prompt | completion | total |
|---|---|---|---|---|
| 1 | 1 | 3,797 | 48 | 3,845 |
| 2 | 2 | 3,996 | 22 | 4,018 |
| 3 | 3 | 7,196 | 35 | 7,231 |
| 4 | 4 | 7,293 | 25 | 7,318 |
| 5 | 5 | 10,360 | 5,509 | 15,869 |
| 6 | 6 | 15,920 | 34 | 15,954 |
| 7 | 7 | 16,786 | 105 | 16,891 |
| 8 | 8 | 16,942 | 131 | 17,073 |
| 9 | 9 | 17,119 | 109 | 17,228 |
| 10 | 10 | 17,378 | 61 | 17,439 |
| 11 | 10 | 17,550 | 44 | 17,594 |
| 12 | 11 | 17,980 | 82 | 18,062 |
| 13 | 12 | 18,215 | 142 | 18,357 |
| 14 | 13 | 18,434 | 37 | 18,471 |
| 15 | 14 | 18,522 | 32 | 18,554 |
| 16 | 15 | 18,605 | 61 | 18,666 |
| 17 | 15 | 18,777 | 45 | 18,822 |
| 18 | 16 | 19,769 | 59 | 19,828 |
| 19 | 17 | 20,080 | 219 | 20,299 |
| 20 | 18 | 20,350 | 26 | 20,376 |
| 21 | 19 | 20,427 | 26 | 20,453 |
| 22 | 20 | 20,504 | 26 | 20,530 |

Total cost: **$0.0305** — over 5x the clean run, on a task that was, in the code sense,
finished nine round-trips earlier (see below).

## What actually drives growth

Not turn count on its own. Cross-referencing prompt-token deltas against the
`tool_result` sizes in the journal:

- **go-humanize**, round-trip 3→4: prompt jumps 4,670 → 5,395 (+725) right after a
  1,064-byte `read_file` on `ordinals_test.go`. Every other jump in that run is under
  300 tokens and follows a small tool result.
- **bytes.js**, round-trip 2→3: prompt jumps 3,996 → 7,196 (+3,200) right after a
  5,998-byte `read_file` on `index.js`. Round-trip 4→5 jumps 7,293 → 10,360 (+3,067)
  right after a 7,197-byte `read_file` on the test file. Those two reads alone account
  for **over a third of the entire session's final context size** (17,073 of ~20,530
  tokens at the point the fix landed).

Full-file reads are the dominant cost, not conversation length. A short exchange after
a big read stays expensive for the rest of the session under naive full-history
assembly (`internal/engine`'s documented choice) — the read never gets smaller, and
nothing currently proposes that it should.

The one genuine outlier is bytes.js round-trip 5: **5,509 completion tokens** in one
response — the model produced a long internal walkthrough before making the actual
`edit_file` call at round-trip 9. That is a generation-length problem, not a
history-growth one, and a compaction strategy wouldn't touch it.

## The second finding: growth that isn't about the task at all

The fix in bytes.js was correct and verified passing by round-trip 9 of 22 (`edit_file`
+ `verification: passed`, turn 9). Everything from round-trip 10 onward is the model
trying to delete a scratch file it had written for its own testing
(`write_file test_bug.js` at turn 8) via `run_shell rm test_bug.js` — denied five times
in a row, identically, by `UnattendedDenyPolicy`, each with the same explicit
`"this will not be approved on retry"` reason. The session still grew from 17,228 to
20,530 prompt tokens (+3,302, ~16%) purely from repeating a call the harness had
already told it, in plain language, would never succeed — and then hit `max_turns`
with the real work done and unreported.

This is not a compaction problem; a growth-curve fix wouldn't have prevented it, and
trimming history wouldn't have made the model stop retrying. It's a distinct harness
gap — no tool exists to remove a file the model created, and the explicit
don't-retry guidance in the deny reason didn't change the model's behavior here. Raised
separately as its own card rather than folded into KAN-948, since the fix (if any) is
either a new tool or a stronger repeat-request cutoff in the engine loop, not a context
strategy. See KAN-936 for the full journal trace.

## What this means for KAN-948

Two real sessions is not a curve, but it rules out the simplest compaction shapes:

1. **A flat per-turn cap (e.g. "keep the last N tool results") is the wrong lever.**
   Growth is bursty and concentrated in a small number of large `read_file` results,
   not evenly distributed across turns. A strategy that trims oldest-first will happily
   keep two 6-7 KB file dumps from turn 2 and throw away small, currently-relevant
   results instead.
2. **The candidate worth designing is result-size-aware, not turn-age-aware:** once a
   `read_file` result has been superseded by a successful `edit_file`/`write_file`
   against the same path (confirmed by a later `verification: passed`), the original
   full-file dump is very unlikely to be needed again in that session and is the
   highest-value thing to compact — not the oldest message.
3. **A stuck loop is a separate failure mode from a naturally long session** and
   inflates the same metric (prompt tokens) for a completely different reason. Any
   instrumentation for compaction should keep these apart — e.g. by watching for
   identical consecutive tool calls — rather than one curve conflating "legitimately
   deep task" with "spinning on a denied action."

None of this is a design yet — KAN-948 is explicit that a strategy chosen without
session data is a guess, and n=2 is barely data. It's the shape the next real dogfood
runs (a third and fourth language, per KAN-935's original scope) should go looking to
confirm or break.
