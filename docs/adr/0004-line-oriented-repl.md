# ADR-0004: Line-oriented REPL, not a full-screen TUI

- **Status:** Accepted
- **Date:** 2026-08-11
- **Deciders:** Jian (leejianrong2@gmail.com)

Feeds [ADR-0001](0001-go-implementation-language.md) by removing the TUI framework
from the language comparison.

## Context

"Terminal UI" covers two designs with very different costs.

**Full-screen (alt-screen) applications** take over the terminal, manage panes,
handle mouse input and repaint on a frame loop. Bubble Tea in Go and Ink in
TypeScript exist for this. The costs are structural, not cosmetic: you inherit
layout and resize handling, and you break the terminal's own scrollback and
copy-paste, because your application owns the screen rather than appending to it.

**Line-oriented streaming REPLs** write to stdout. The terminal keeps its scrollback,
copy-paste works, `Ctrl-C` behaves like `Ctrl-C`, and output can be piped to a file
or read by another program. Far less code, far fewer failure modes. Claude Code
renders inline rather than taking the alt screen, which is the same call.

The stated requirement for kopicode's surface is *robust and performant*, not fancy.
Those properties argue against the alt screen, since almost every robustness problem
in a terminal agent — a mangled resize, lost scrollback, a repaint racing streamed
output, a broken copy-paste of a stack trace someone needs — is a consequence of
owning the screen.

## Decision

**1. The surface is a line-oriented streaming REPL.** stdout, append-only, terminal
keeps scrollback and selection.

**2. No TUI framework.** ANSI escapes for in-place status and streaming, plus a small
line-editor dependency for input history and editing. No Bubble Tea, no Ink, no
alt-screen mode.

**3. Constraints that follow, and are requirements not side effects:**

- Streamed model output must remain readable when piped to a file with no TTY —
  detect a non-TTY and drop escape sequences.
- Progress indicators repaint at most the current line, never scrollback.
- Interrupt is honoured mid-stream: `Ctrl-C` cancels the in-flight turn and returns
  to the prompt without killing the session.
- Diffs, tool output and errors are printed as text, not rendered in panes.

**4. `--print`/non-interactive mode is not an afterthought.** The headless runner in
[ADR-0003](0003-single-repo-internal-engine.md) is a first-class front end, so
machine-readable output (JSON events derived from the journal) is a supported mode
from slice 1.

**5. Reversible.** If a full-screen mode is later wanted for something specific — a
session browser, a fork-and-compare view — it can be added as a separate subcommand
over the same journal. This ADR governs the main interactive loop only.

## Consequences

- Less surface polish than a pane-based agent. Accepted deliberately.
- No mouse support, no simultaneous multi-pane views, no in-place editable regions.
- The rendering layer is small enough to be hand-written and tested by asserting on
  captured stdout, which is a real testing advantage over a frame-loop framework.
- Bubble Tea stops being an argument for Go. ADR-0001 rests on distribution,
  compile-time safety and process control instead.
