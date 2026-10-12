# ADR-0034: Mid-conversation reminders, a flat todo list and turn-budget warnings

*Proposed.* Builds on [ADR-0028](0028-stall-detector.md) and [ADR-0033](0033-tool-output-cap.md), which also change only the wire, and on [ADR-0022](0022-turn-and-budget-limits.md) (the turn cap and token budget this reads). Amends [ADR-0007](0007-model-selection-and-harness-config-shape.md) decision 6 the same way they did: new values enter the hash only when set. Whether to accept, and in what order to build, is the owner's call.

## Context

A model that runs many turns loses the thread: the task is a long way up the context, the verification rule is in a system prompt it no longer attends to, and the turn cap arrives as a stop, not a warning. Three mechanisms are common in other harnesses (`docs/research/2026-10-harness-survey.md` §3):

- LangChain's Deep Agents lists a pre-completion checklist, loop detection and **time-budget warnings** among changes that moved Terminal Bench 2.0 from 52.8 to 66.5 with the model fixed. Only the reasoning-effort schedule was isolated, so none of the three has a number of its own. Vendor data, one benchmark.
- Claude Code injects `<system-reminder>` blocks and has a todo tool. **No authoritative source and no ablation was found** for either; the survey says to capture raw API traffic if the exact text matters.
- Claude Code's guidance and the survey's list of traps agree that full plan mode with an approval UI suits strong models and not weak ones, which plan verbosely and then deviate.

So the evidence for these mechanisms is weak, and none of it is on kopicode's corpus or on an open-weight model. This ADR proposes mechanisms cheap enough to try and a plan for finding out whether they help. It claims no gain.

**What exists today.** The loop counts `used` turns against `Config.MaxTurns` (`internal/engine/loop.go`) and stops with `StopMaxTurns`; nothing tells the model. Forced verification runs after a turn that modified files (`Config.Verification`). The stall detector and the output cap already append wire-only text after a tool result, derived from the call sequence, with the journal untouched.

## Decision

1. **Three pieces, built and measured in this order, each its own card and its own harness value.** A turn-budget warning, then a reminder, then a todo list. Each ships off. A later one is built only if the earlier one's measurement did not rule the idea out.
2. **Turn-budget warning.** When the turns left in this prompt (`MaxTurns - used`) reach `Config.TurnWarning` (0 = off), a note is appended to the next tool result on the wire: *"kopicode: N turns left for this prompt. If the change is made, run the verification now and finish; if not, say what is blocking you."* It fires at that count and again at one turn left, no more. A token-budget warning is the same note keyed on `TokenBudget - spent`, but spend is reported after the request that incurred it (ADR-0022), so it is approximate and is left out until someone needs it.
3. **Reminder.** Every `Config.ReminderEvery` turns of one prompt (0 = off), and once right after a failing verification, a note is appended to the next tool result: the user's task for this prompt, verbatim and clipped to 500 bytes on a rune boundary, and one line restating the verify rule when forced verification is on. It never repeats a note the model has just been given: a turn that already carries a stall note or a turn warning skips it.
4. **Todo list, last.** One flat `todo` tool whose argument is the whole list, `[{text, status}]` with `status` one of `pending`, `in_progress`, `done` and at most one `in_progress`. It replaces the list, so there is no add/remove bookkeeping to get wrong. The call is an ordinary `ToolCall`/`ToolResult` in the journal and the list is derived from the latest one, so there is no parallel record. The REPL renders it. When on, the reminder (decision 3) includes the list. It adds a tool definition, which the survey lists as a cost for weak models, so it is the one piece that must justify itself by measurement before it is built.
5. **Only the wire changes, and a replay builds the same text.** Each note is a pure function of the turn count, the config and the journal's own events. The journal holds the tool results unmodified, as ADR-0012 decision 1, ADR-0028 and ADR-0033 already do. A resumed session rebuilds its history from the journal and carries no past notes; counts restart.
6. **Cache-neutral.** A note is appended to the newest tool result, which is new text at the tail of the next request, so the stable prefix ADR-0031's batching and KOP-172's caching protect is untouched. Nothing is inserted earlier in the history.
7. **Config.** `Config.TurnWarning`, `Config.ReminderEvery` and, later, `Config.Todo` (bool), each 0 or false = off and in the hash only when set, so every registered arm and every recorded hash stays as it was. Flags `--turn-warning N` and `--reminder-every N` on the REPL and `run --print`; `serve` and `mcp` stay off and opt in through a declared harness config, as ADR-0028 left them. **No default on, in the REPL or anywhere, until a measurement exists:** unlike the stall detector, nothing here has even a single-study justification for a weak model.
8. **No plan mode with an approval step.** A model proposes a plan, a human approves, the loop continues: this is rejected for the reason in the survey, and because it makes the loop wait on a person, which `serve` cannot do for cuttlefish.

## The measurement that decides

- **Arms**, same model and pinned provider route per ADR-0005 and ADR-0007: none; turn warning; turn warning plus reminder; and, if built, plus todo.
- **Metrics:** solve rate; the share of runs that end on `StopMaxTurns`; turns to completion on tasks that finish; the share of finished runs that ran verification before stopping; cost. The hypothesis the warning tests is narrow: fewer runs reach the cap without having verified.
- **The corpus cannot show it.** Its 13 tasks run 2 to about 8 turns against a cap of 20, so a warning at 3 turns left would almost never fire. A set of tasks that reach the cap is needed first. KOP-171 (handoff against continuing, past the turn cap) needs the same set, and the two should share it rather than each build one.
- **Mock-provider tests** cover the mechanics at zero cost: the note appears at the right turn, only once, never alongside a stall note, journal unchanged, hash unchanged when off, replay equal.
- **Live runs need OpenRouter credit**, which is exhausted as of this writing. Nothing live is run before it is topped up, and no claim is made without one.

## Rejected

- **A system-prompt change alone.** The point is that the system prompt is far away by turn 30.
- **A new user message injected as a turn.** It would be a journal event and a prefix change for text that is derivable; appending to the tool result needs neither.
- **Defaulting any of this on in the REPL by analogy with ADR-0028.** That one had a recorded failure to point at (13 of 22 round trips wasted); this has not.
- **A todo list that is a markdown file the model edits.** It would be a second place the plan lives and the journal rule forbids a parallel record.
- **Time-based warnings.** Wall-clock depends on the provider and breaks replay; turns are deterministic.

## Consequences

- When built, each piece touches `internal/engine/loop.go` or `dispatch.go` (where the notes are appended), `internal/harness` (the values, hash rule and flags), the README beside the stall detector's paragraph, and for the todo tool `internal/tools`, the tool catalogue and the REPL renderer.
- The turn warning needs `used` and `MaxTurns` where `dispatch` appends the note; today `used` is a local of the loop, so it becomes an engine field the way `turnSeen` is.
- ADR-0031 (masking) and ADR-0026 (handoff) compete for the same long-session problem. These notes are the cheapest of the three and should be measured in the same long-task set, as separate arms.
