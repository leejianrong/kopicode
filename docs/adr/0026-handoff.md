# ADR-0026: Handoff: clear the context, carry a document

*Proposed.* Settles the direction [ADR-0012](0012-context-compaction-strategy.md) left open (KAN-991). It does not amend ADR-0012: compaction stays rejected.

## Context

ADR-0012 rejected redacting or summarising history in place. The reasoning behind that was never "memory is out of scope". It was that a long session is better ended than shrunk: clear the whole context and start again from a document that says where things stand. The index line for ADR-0012 and the non-goals list ("a memory system") made it read as if kopicode wanted neither, which is wrong.

What the survey of other harnesses (2026-10) found:

- Amp dropped compaction for a handoff: the user states a goal, the agent drafts a prompt and a file list, the user edits it, and a new thread starts. Cline's `/newtask` carries the plan, work done, relevant files and next steps into a clean window without the tool-call noise. Anthropic's long-running-agent guidance does the same with a progress file, git history and a verify script.
- Long contexts degrade reliability (Chroma's context-rot study). Hiding old tool outputs matched or slightly beat LLM summarisation at about half the cost (JetBrains, arXiv 2508.21433, Qwen3-Coder on SWE-bench Verified), so the summary itself is not what carries the benefit.
- **There is no controlled study of reset-with-a-document against compaction, and none on weak open-weight models.** This ADR is a bet that the benchmark rig can test, not a finding.
- cuttlefish-crew already built its own handover for a round that runs out of room (V5): a third-person report of the previous handover and the repository's state, never empty, naming the files and tool calls of the rounds it covers. What it learned is in the decisions below.

## Decision

1. **A handoff ends a session and seeds a new one. Nothing is rewritten in place.** The old session's history, its journal and `context.go`'s "nothing here truncates" guarantee are untouched. The new session has an empty context plus one message: the handoff.
2. **The document has fixed sections**: goal; done; remaining; decisions and why; open problems (a failing test, a denied call, an error not yet understood); files (paths only, never contents); the next step; the verify command. Sections are fixed so a weak model fills blanks instead of choosing a shape.
3. **The model writes the narrative; the harness writes the facts.** The sections above are one tool-less model call from a fixed prompt. A second block is appended mechanically from the journal: files written or deleted this session, the last verification outcome and command, turns and tokens used, and calls that were denied. A model that forgets a file does not lose it, which is the lesson of cuttlefish's "never empty, name the files".
4. **The journal is the record, the file is a projection.** The handoff text is journaled as an event (a blob if over 64 KiB) with the parent session id, and `.kopicode/handoff/<session>.md` is written from it. The new session's journal records which handoff it started from. No parallel transcript.
5. **Verification does not carry over.** The new session starts with verification `NotRun`, whatever the old one's last result was. A handoff that says "tests pass" is a claim, not a result; forced verification runs again before success can be reported.
6. **Triggers.**
   - REPL: `/handoff [goal]` drafts the document, prints it, and opens it for editing before anything starts; confirming starts a new session from it. The human edit is the cheap quality gate Amp relies on. `/context` may warn when the window is mostly full. The engine never clears a context on its own.
   - `serve` and `mcp`: a `session.handoff` method returns the document for a session, and `session.start` accepts a `handoff` field. When to hand off stays the caller's policy (cuttlefish does it at 75% of the window). Feature name: `session.handoff`.
   - `run --print` and `kopibench` do not hand off.
7. **No change to the system prompt or the harness hash**, as with ADR-0025. The handoff arrives as a user-turn message under a `<handoff>` tag, and kopibench arms never see one.
8. **How it gets judged.** A paired run on tasks that exceed the turn cap: continue in place against hand off at the cap, same pinned arm. Until that exists the claim is "unmeasured", and the docs say so.

## Rejected

- **In-place compaction or summarisation** (ADR-0012 decision 2): the shape is wrong, whatever the exception's scope.
- **Observation masking in the model's view** (stubs pointing at blobs): a separate idea with the best published numbers, but it rewrites the prefix and costs a provider cache write each time. It is not rejected for good; it needs its own measurement and ADR, and it competes with a handoff for the same job.
- **The engine clearing the context automatically**: a decision to throw away working memory belongs to a person or a caller with a policy.
- **Auto-written or self-editing memory** (Letta-style blocks, Claude Code's auto memory): no evidence yet on how well weak models write it. A handoff is written once, on request, and reviewed.
- **The model writing its own handoff through `write_file` mid-session**: a weak model forgets to, and the harness-written block could not be guaranteed.

## Consequences

- A new `internal/handoff` leaf package builds the facts block from journal events and renders the document; the engine gains one tool-less call and `engine.Open` gains a handoff option, so front ends keep their import allowlist.
- The journal schema gains a `handoff` event and a `parent_session` link. Events are a compatibility surface, so both are versioned and old readers preserve them as unknown types.
- `session.handoff` is a new wire addition: a string in `features` in `cmd/kopicode/capabilities.go` and a row in `docs/kopicode-serve-protocol.md`.
- cuttlefish can replace its own summariser call with the harness-written facts block, keeping its trigger.
- Instruction files (an AGENTS.md hierarchy walked from the working directory, nearest last, with a byte cap) are a separate, cheaper piece of work and get their own ADR. They are not part of this one.

## Status of the build

- Step 1 (#205): the `internal/handoff` document and facts block, and the `HandoffWritten` event.
- Step 2 (KAN-1966): `Engine.Handoff` (one tool-less call, history untouched), `Options.Handoff` / `ParentSession` (the document arrives as a `ProjectInstructionsLoaded` with scope `handoff`; `SessionStarted.parent_session` links back), and `/handoff [goal]` in the REPL. The REPL closes the old session before opening the new one, because a working tree has one.
- Step 3 (KAN-1967): `session.handoff` on `serve` (queued behind accepted turns, cancellable), `kopicode_handoff` on `mcp`, and `handoff` / `handoff_from` on both start calls; feature `session.handoff`.
- Step 4 (KAN-1968): the paired measurement. Not built; the claim stays "unmeasured".
