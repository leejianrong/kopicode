# KOP-222: a multi-region `edit_regions` tool, measured on qwen3-coder-next s3 (2026-10-24)

The second remedy on the card, after the schema sentence ([`2026-10-24-kop-222-schema`](../2026-10-24-kop-222-schema/README.md)) did nothing: one call that carries several regions of one file,
every anchor from one read, applied together or not at all. It does not accept a stale anchor, so ADR-0006 is untouched.

## What was built (not merged)

`edit_regions {path, edits: [{anchor_start, anchor_end, new_text}]}`. Every region is resolved against the same read; any stale, malformed or ambiguous anchor, or two regions sharing a line, refuses the **whole**
call with the file byte-identical; regions may touch. It journals one `EditApplied` per region under the call's id and runs the syntax gate once. Offered only by a new opt-in config (`default-regions`), because
this repository's guards require the system prompt (at its 9,216-byte budget) to document every tool a config offers; the variant prompt trimmed three paragraphs to fit.

It was tested before it was measured: a seeded property test (240 file shapes: LF, CRLF, with and without a trailing newline, deletions, touching regions, shuffled request order) holds its bytes equal to applying each region
alone against a fresh read. That test found a real bug on its first run (deleting the last lines under another region shifted offsets), which is why the tool writes in one pass over the original bytes. The code is kept on a local
branch, not merged: the measurement below says it does not do its job.

## Result

s3 on `qwen/qwen3-coder-next`, the Claude Code driver, `harness = "default-regions"` in the scratch repo's config, six runs, against the six baseline runs of the schema note (same day, same model, main).

| | runs | used `edit_regions` | `check.sh` FAIL | rejections per run |
| --- | --- | --- | --- | --- |
| baseline (main) | 6 | n/a | 1 | 3, 3, 1, 3, 3, 0 |
| `default-regions` | 6 | **0** | **4** | 3, 3 in the two that finished; 0 in the four that stopped early |

- **Qwen never called it.** Not once in six runs; the journals have no `edit_regions` call, parsed or failed.
- **Four of six runs got worse.** Each sent one `edit_file` (the README), ran verification (which passed: the verify command does not look for the leftover name), and ended with "I've successfully renamed ... throughout the repository", with
  `fetch_user` still in four files. The other two did what the baseline does, 11 applied and 3 rejected. In the baseline one run in six left an occurrence.
- Not isolated: this arm differs from main in the tool catalogue **and** in the trimmed prompt, so the early stop is not attributed to either. It does not matter for the decision: the point of the tool is that the model uses it, and it did not.
- Some runs show `retried` requests; those are HTTP 429s from six sessions on one provider (`ProviderRetried` cause `http 429`), not stream retries.

## Verdict

Not shipped. KOP-222's remedies are exhausted on this model: a prompt line, a schema sentence and a new tool shape each failed to move s3's three rejections, and the tool shape made qwen stop early. The common outcome on qwen is still
`anchor_drift` x3 on s3 with a recoverable, correctly finished tree. What is left is a rule that accepts a stale anchor next to an applied edit in the same reply (the old-to-new map KOP-215 already prints, applied to a later call): that changes
ADR-0006 and needs an ADR and the owner's approval first, so it is a proposal, not something built here. The other option is to leave it: deepseek has 0 rejections on s3.

## Not checked

One model, one scenario, one driver, six runs. Deepseek was not run on this arm. Whether a different model (or qwen with a prompt that says to use the tool) would adopt `edit_regions` was not tried; the prompt already says to use it instead of several `edit_file` calls.
