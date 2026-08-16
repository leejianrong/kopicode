package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/leejianrong/kopicode/cmd/kopicode/lineedit"
	"github.com/leejianrong/kopicode/internal/engine"
)

// The request and the answer are the engine's types, not this package's.
//
// [engine.ConsentRequest] and [engine.ConsentAnswer] exist precisely so a
// surface can be asked without importing internal/permission, and declaring a
// third pair here would be a translation table between two vocabularies that
// already agree — the kind of duplication that stays correct until the day it
// does not. What this file owns is how the question *looks*, which is the whole
// of the surface's half (internal/permission's package doc).

// ErrNoConsent is returned when the question could not be put to anybody: the
// input ended, the turn was cancelled, or the terminal broke. The [Answer] is
// [engine.ConsentDeny] in every such case.
//
// An unanswerable question is not a yes. That is the one rule this function
// has, and it is why the error and the deny always travel together.
var ErrNoConsent = errors.New("repl: consent could not be obtained")

// Ask puts a consent request to the user and returns the answer.
//
// The question is printed as ordinary lines and read as an ordinary line, so
// it works identically with and without a terminal — which matters because a
// piped session that hangs invisibly on an unanswerable prompt is worse than
// one that refuses.
//
// Cancellation is checked either side of the read. A Ctrl-C that lands while
// the question is on screen reaches this in one of two ways: on a terminal it
// is a byte, and lineedit returns [lineedit.ErrInterrupted] immediately; piped,
// it is a signal that cancels the turn's context, and the check after the read
// catches it. In neither case can an interrupted turn be the turn that gets
// consent.
func (l *Loop) Ask(ctx context.Context, c engine.ConsentRequest) (engine.ConsentAnswer, error) {
	if err := ctx.Err(); err != nil {
		return engine.ConsentDeny, fmt.Errorf("%w: %w", ErrNoConsent, err)
	}

	l.Progress("")
	l.out.flushLine()
	l.renderRequest(c)

	answer, err := l.readAnswer(c)
	if err != nil {
		return engine.ConsentDeny, err
	}
	if cerr := ctx.Err(); cerr != nil {
		// Answered, but the turn it belonged to is gone. Honouring it would
		// run the command the user just interrupted.
		return engine.ConsentDeny, fmt.Errorf("%w: %w", ErrNoConsent, cerr)
	}
	return answer, nil
}

// renderRequest prints the question. One block, no box drawing, no cursor
// movement: it scrolls like everything else and can be selected and pasted
// into a bug report.
func (l *Loop) renderRequest(c engine.ConsentRequest) {
	l.out.line(l.out.bold("[perm] "+orUnknown(c.Tool)) + " needs your consent")
	if c.Detail != "" {
		l.out.line("       " + label(c.Kind) + ": " + c.Detail)
	}
	if c.Resolved != "" && c.Resolved != c.Detail {
		l.out.line("       resolves to: " + c.Resolved)
	}
	if c.Reason != "" {
		l.out.line("       reason: " + c.Reason)
	}
}

// label names what Detail is, so the line reads as a sentence rather than as a
// field dump.
func label(kind string) string {
	switch kind {
	case "run_shell":
		return "command"
	case "write_outside_root":
		return "path"
	default:
		return "detail"
	}
}

// consentPrompt states every option and which one Enter picks. The capital N
// is the convention for "this is what you get for free", and free must be no.
const consentPrompt = "allow? [y]es / [N]o / [a]lways for this exact request: "

// readAnswer asks once and interprets the reply.
//
// It asks exactly once. A loop that re-asked on an unrecognised answer would
// be a loop a piped session cannot leave, and re-prompting a human who typed
// something else is how a considered "no" becomes an accidental "yes" three
// keystrokes later.
func (l *Loop) readAnswer(c engine.ConsentRequest) (engine.ConsentAnswer, error) {
	l.ed.SetPrompt(consentPrompt)
	defer l.ed.SetPrompt(l.prompt)

	reply, err := l.ed.ReadLine()
	l.out.afterPrompt(l.promptEndsLine)
	switch {
	case errors.Is(err, lineedit.ErrInterrupted):
		l.tag("perm", "denied: interrupted")
		return engine.ConsentDeny, fmt.Errorf("%w: interrupted", ErrNoConsent)
	case errors.Is(err, io.EOF):
		l.tag("perm", "denied: no input to ask")
		return engine.ConsentDeny, fmt.Errorf("%w: %w", ErrNoConsent, io.EOF)
	case err != nil:
		l.tag("perm", "denied: "+err.Error())
		return engine.ConsentDeny, fmt.Errorf("%w: %w", ErrNoConsent, err)
	}

	answer := interpret(reply)
	l.tag("perm", fmt.Sprintf("%s: %s", orUnknown(c.Tool), answer))
	return answer, nil
}

// interpret maps a typed reply onto an answer. Anything that is not an
// affirmative is a refusal, including an empty line and including a typo.
func interpret(reply string) engine.ConsentAnswer {
	switch strings.ToLower(strings.TrimSpace(reply)) {
	case "y", "yes":
		return engine.ConsentAllow
	case "a", "always":
		return engine.ConsentAllowSession
	default:
		return engine.ConsentDeny
	}
}
