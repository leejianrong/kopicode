# ADR-0041: Record, from the journal, a final message that says it changed a file it never edited

*Proposed* 2026-10-27, for the owner to accept or reject. Nothing is built. KOP-230. Builds on [ADR-0039](0039-leftover-name-note.md) (the engine derives what it checks from its own journal, not from the task) and on the one-session-record rule: everything a surface prints is derived from journal events. It touches no boundary in `AGENTS.md`. Part 2 changes how a session ends, so it is a decision.

## Context

On s3 (rename `fetch_user` across five files), a qwen session sometimes makes one edit, to `README.md`, sees the tests pass, and ends with a message that lists all five files as updated. ADR-0039's note does not reach it by design: its threshold is two edits. Verification cannot catch it either, because the tests do not mention the leftover.

Counted over the 89 distinct sessions with a final message in the journals on disk (qwen and deepseek, `run --print` and driven, 746 applied edits):

- 82 final messages name at least one file.
- 6 name a file with no `EditApplied`, `write_file` or `delete_file` on it. Three of the six are the one-edit s3 sessions above (`drv/s3`, `runs/base-1`, `runs/trial-new-0`): four files each named as updated and never touched. The other three name the failing *test* file as the thing that was fixed, which is a subject, not a claim of change.
- A claim is read as: the file's name, then within 60 characters on the same line one of *updated, renamed, modified, changed, edited, replaced, rewrote, fixed, added, removed, deleted*. With that rule the count is **3 of 89 sessions, all three real, and 0 false positives** over the 6 above.

The sample is the two scenario families this repo runs and is not a sample of real use. A file changed through `run_shell` (`sed -i`, a redirect) has no `EditApplied`, so a session that does that would be flagged wrongly; in the 89 the shell only runs tests, searches and `rm`/`rmdir` of scratch files, none rewrites a named source file, and the check must say "cannot tell" for a session that ran a mutating shell command rather than guess.

## Decision

1. **When a session ends, the engine compares its last `AssistantMessage` with the files the journal shows touched** (`EditApplied.path`, `write_file`, `delete_file`) and records a new event `ClaimUnbacked {files, seq}` for each file the message names with a change verb after it and the journal never touched. A session that ran a shell command that could write (anything the consent gate classes as mutating) records `ClaimUnchecked {reason}` instead and nothing else.
2. **Journal only.** The session ends exactly as it would have. No turn is added, no text is sent to the model, and the exit code and stop reason are unchanged. The event is new; older readers keep unknown types.
3. **`kopicode sessions -report` prints a line** under R8 (`claims: 4 files named as changed, never edited: accounts/greeting.py, ...`) so a driven-run note cites it by `seq` instead of by reading.
4. **A second part, not proposed to build now:** turning the event into one more turn (the engine appends *"your message says X was changed; the journal shows no edit to it"* and lets the model answer, once per prompt). That changes a stop, so it needs its own measurement on s3 before it is a decision; this ADR only records the event that measurement would need.

## Consequences

- No change to what a model sees or to any session's outcome. Existing replays are unaffected.
- The detector is a regex over prose. It will miss a claim without a file name ("I renamed it everywhere") and could flag a file named in a quoted list. The measurement says 0 of 89; it does not say it is safe on text it has not seen. The event carries the matched span so a false positive is visible.
- It does not tell anyone the rename is incomplete, only that the message and the journal disagree. ADR-0039's note and a grep are what find the leftover.

## Alternative

Close KOP-230: three of 89 is the same one-edit stop already counted in ADR-0039, which a driver caught by reading the tree. The cost of closing is that an outer agent has to do that every time; the event makes it one line in a report.
