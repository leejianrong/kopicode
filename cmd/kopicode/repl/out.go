package repl

import (
	"io"
	"strings"
)

// The whole escape vocabulary this package is allowed to write, and it is
// short on purpose.
//
// ADR-0004 decision 3 says a progress indicator repaints at most the current
// line and never scrollback. The way to keep that true is to have no
// constant that could break it: there is no cursor-up here, no scroll region,
// no clear-screen and no alt-screen switch, so a future edit that wanted one
// would have to add it and explain itself.
// escapeVocabulary in escape_test.go holds this list to the bytes the
// package actually emits.
const (
	// csiClearToEnd erases from the cursor to the end of the current line.
	csiClearToEnd = "\x1b[K"
	// sgrReset, sgrBold and sgrDim are the only colours used. Colour is
	// emphasis, never information: every line reads correctly with the
	// escapes removed, which is what makes the piped form of the same
	// session equivalent rather than degraded.
	sgrReset = "\x1b[0m"
	sgrBold  = "\x1b[1m"
	sgrDim   = "\x1b[2m"
)

// out is the append-only writer everything the user reads goes through.
//
// Append-only is the invariant, and "the current line" is the single
// exception: a progress status may be repainted in place because it has not
// been committed to scrollback yet, and it is erased before anything else is
// written. Nothing here can reach a line the user has already read.
//
// A write error is sticky and silent. A REPL whose stdout has gone away —
// `kopicode | head` — has nothing useful to say about it and nowhere to say
// it, and turning a closed pipe into a session-ending error would make the
// most ordinary shell idiom look like a crash. The error is kept so
// [out.err] can be consulted where it matters.
type out struct {
	w           io.Writer
	interactive bool

	// pending is true when the last byte written was not a newline, so the
	// next thing that needs a line of its own knows to break first.
	pending bool
	// progress is true when a progress status is painted on the current
	// line and has not been erased.
	progress bool

	err error
}

func newOut(w io.Writer, interactive bool) *out {
	return &out{w: w, interactive: interactive}
}

// write is the only place bytes leave this package.
func (o *out) write(s string) {
	if s == "" || o.err != nil {
		return
	}
	if _, err := io.WriteString(o.w, s); err != nil {
		o.err = err
		return
	}
	o.pending = !strings.HasSuffix(s, "\n")
}

// text writes model output verbatim, exactly as it arrived.
//
// No wrapping, no re-indentation, no trimming. The terminal wraps, the
// terminal selects, and a surface that reflowed the model's prose would make
// a piped transcript differ from what the journal recorded.
func (o *out) text(s string) {
	o.clearProgress()
	o.write(s)
}

// line writes s and a newline, breaking out of a partial line first.
func (o *out) line(s string) {
	o.clearProgress()
	o.flushLine()
	o.write(s + "\n")
}

// flushLine ends a partial line, so the next thing written starts at column
// zero. It writes nothing when there is nothing to end.
func (o *out) flushLine() {
	if o.pending {
		o.write("\n")
	}
}

// afterPrompt records where the line editor left the cursor.
//
// The editor writes the prompt to the same stream as this writer but not
// through it, so without this the writer's idea of the current column is a
// guess. endedLine is true when the editor finished the line itself, which is
// exactly the raw-mode path: every exit from it writes CR LF. The plain path
// writes the prompt, reads a line the terminal never echoed, and leaves the
// cursor mid-line — which is why a piped session would otherwise run its first
// output line into the prompt, and why the last prompt before EOF would leave
// a file that does not end in a newline.
func (o *out) afterPrompt(endedLine bool) { o.pending = !endedLine }

// setProgress paints status on the current line, replacing whatever was there.
//
// It is a total no-op without a terminal — not a plain-text fallback, no-op.
// A progress line's whole value is that it is transient, and a transient
// thing written to a pipe is neither transient nor valuable: it would put a
// dozen "thinking…" lines in the middle of a transcript that is supposed to be
// the session.
func (o *out) setProgress(status string) {
	if !o.interactive {
		return
	}
	if status == "" {
		o.clearProgress()
		return
	}
	o.flushLine()
	o.write("\r" + csiClearToEnd + status)
	o.progress = true
}

// clearProgress erases a painted status, leaving the cursor at column zero.
func (o *out) clearProgress() {
	if !o.progress {
		return
	}
	o.write("\r" + csiClearToEnd)
	o.progress = false
	o.pending = false
}

// style wraps s in an SGR sequence, or returns it unchanged with no terminal.
//
// This is the function that makes "no escapes when piped" structural rather
// than a stripping pass: there is one place a colour can be introduced, and it
// has the check in it.
func (o *out) style(code, s string) string {
	if !o.interactive || s == "" {
		return s
	}
	return code + s + sgrReset
}

func (o *out) dim(s string) string  { return o.style(sgrDim, s) }
func (o *out) bold(s string) string { return o.style(sgrBold, s) }
