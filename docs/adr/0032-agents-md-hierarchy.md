# ADR-0032: An AGENTS.md hierarchy, nearest last, with a byte cap and a visible listing

*Proposed.* Amends [ADR-0024](0024-user-level-config.md) decision 5 and the repository loader from KAN-1024 if accepted. Whether to accept is the owner's call. The survey cited below is the evidence for the shape and also the reason to keep it small.

## Context

**What the code does today.** `harness.LoadAgentsFile` walks up from the working directory and returns the **first** `AGENTS.md` it finds, stopping at the first directory holding a `.git` entry (`internal/harness/file.go`). `loadProjectInstructions` (`internal/engine/instructions.go`) journals it as one `ProjectInstructionsLoaded` event and appends one user message. The user-level file goes in first (ADR-0024 decision 5). There is no byte limit and nothing tells the person which file loaded, other than the path inside the event.

**Consequence.** Start a session in `services/api/` of a monorepo that has a root `AGENTS.md` and a `services/api/AGENTS.md`: only the nested one loads, so the repository-wide rules (here, for example, the subprocess and consent rules) are silently absent. A very large file loads whole.

**What others settled** (`docs/research/2026-10-harness-survey.md` §1, the table and the patterns paragraph):

- Codex CLI builds a chain once per session: global, then repository root to the working directory, one file per directory, nearer last, capped at 32 KiB.
- opencode walks up from the working directory and also reads a global file.
- Claude Code reads a `CLAUDE.md` hierarchy; Cursor reads nested `AGENTS.md`; Gemini CLI reads global, ancestors and subdirectories.
- The survey notes one study (REAP) found extra context files can be neutral or harmful on a stronger harness. This is a single study on another harness, so it argues for a cap and for measuring, not for or against the hierarchy.

## Decision

1. **Collect every `AGENTS.md` from the repository root down to the working directory**, one per directory. The repository root is the first directory holding a `.git` entry going upward, the same boundary ADR-0007's loader uses. Nothing above the root is read, so a stray file in `$HOME` or `/` cannot reach a session.
2. **Order: user file, then root, then each directory down to the working directory. The nearest is last.** The model weights the most recent text most, and the nearest file is the most specific. This extends the rule ADR-0024 already applies between the user file and the repository.
3. **One journal event per file**, `ProjectInstructionsLoaded` with its `Path` and the existing `Scope` field (`user` or omitted for the repository). The unknown-type and additive-field rules (AGENTS.md, "Events are a compatibility surface") hold: an old journal, with one event, reads as before and resume replays from the events, not from the disk.
4. **A byte cap on the total, default 32 KiB**, matching Codex's published figure. When the total exceeds it, **the furthest file is dropped first, whole**: never a file cut in the middle, because half a rule is worse than none. The nearest file always loads; if it alone exceeds the cap it loads whole and the listing says so, since the instruction is the person's own and a hidden clip would be the failure the "never truncate" rule exists to prevent. The cap value is a constant in `internal/harness`, not a config key, until someone needs to change it.
5. **A visible listing.** The REPL prints, at session start, one line per file in load order with its size and whether it was dropped for the cap. `serve` and `mcp` report the same list in the `session.start` result as `instructions: [{path, bytes, loaded}]`, with a feature name added to `features` in `cmd/kopicode/capabilities.go` and a row in `docs/kopicode-serve-protocol.md`, per the wire rule in AGENTS.md. The listing is derived from the journal events plus a drop record, not a second source.
6. **A dropped file is journalled.** The event for a file the cap excluded carries `Dropped: true` and no content, so replay, the listing and a later audit agree.
7. **In the harness hash only when it changes what loads.** The default behaviour with a single `AGENTS.md` is byte-identical to today's, so no recorded fixture or benchmark hash moves. A benchmark arm that sets nothing keeps the one-file shape: bench task repositories have a single file at most, so the hierarchy is a no-op there. If a task fixture ever has nested files, the arm records the list.
8. **No `@`-imports, no `CLAUDE.md` fallback, no globs in this ADR.** Each is a separate decision. The fallback in particular changes which file a repository means, and this repository's own `CLAUDE.md` already points at `AGENTS.md`.

## Measurement

No gain is claimed. The risk is cost and harm, not benefit, so the check is a regression one:

- Mock-provider tests: ordering (user, root, nested, nearest last), the root boundary, the cap and drop order, a lone oversized nearest file, replay from the journal equal to the live session, and a single-file repository byte-identical to today.
- `make bench-smoke` stays green with no hash change.
- A paired live run on a fixture with a nested `AGENTS.md`, hierarchy against nearest-only, belongs with the other context experiments and needs OpenRouter credit, which is exhausted as of this writing. Nothing live is run here.

## Rejected

- **Nearest only (today).** Drops repository-wide rules in monorepos, which is the case the owner's own fleet hits.
- **Concatenating everything with no cap.** One large file, or a deep tree, silently eats the context window on a weak model.
- **Truncating the file that goes over the cap.** Clips a rule mid-sentence; the "never truncate" principle applies by analogy.
- **Reading files above the repository root.** An ancestor directory outside the project is not the project's to speak for.
- **A config key for the cap now.** No evidence anyone needs another value.

## Consequences

- The work splits into one implementation card after acceptance: `harness.LoadAgentsChain` beside `LoadAgentsFile`, `loadProjectInstructions` emitting one event per file, the `Dropped` field on the event, the REPL listing, the `session.start` field and its feature name, and the tests above.
- `docs/kopicode-serve-protocol.md`, the agent brief (`AGENTS.md`) and the README's description of project instructions change in that card, not here.
- Fork and resume already copy `ProjectInstructionsLoaded` events (`internal/engine/fork.go`); the implementation must keep that holding for several events.
