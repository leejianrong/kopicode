package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/leejianrong/kopicode/cmd/kopicode/lineedit"
)

// Consent is a permission request as the surface needs it.
//
// The fields are permission.Request's, copied: Kind is its Kind.String()
// ("run_shell", "write_outside_root"), Reason is the sentence naming the rule
// that fired, Detail is the thing being consented to, and Resolved is the
// absolute symlink-followed path a write targets.
//
// It is a separate declaration rather than the engine's own type for the
// reason internal/permission's doc comment gives: the engine decides *that*
// consent is required and what a decision means, never how it is asked, and a
// surface that imported the decision type would be one edit away from
// deciding.
//
// Resolved is shown rather than the model's spelling, and that is the whole
// point of the field: consenting to "../../etc/hosts" and consenting to
// "/etc/hosts" are different acts, and only one of them is legible.
type Consent struct {
	Kind     string
	Tool     string
	Detail   string
	Reason   string
	Resolved string
}

// Answer is what the user said.
type Answer uint8

const (
	// AnswerDeny refuses. It is the zero value, so every path that fails to
	// reach a positive answer denies — including a dropped return value.
	AnswerDeny Answer = iota
	// AnswerAllow permits this action and only this one.
	AnswerAllow
	// AnswerAllowSession permits this action and later ones with the same
	// kind and the same detail. Exact match and nothing wider; the scope is
	// the gate's, and it is stated in the prompt so the user knows what they
	// are agreeing to.
	AnswerAllowSession
)

// String returns the journal wire value the engine records for this answer:
// "deny", "allow" or "allow_session".
func (a Answer) String() string {
	switch a {
	case AnswerDeny:
		return "deny"
	case AnswerAllow:
		return "allow"
	case AnswerAllowSession:
		return "allow_session"
	default:
		return fmt.Sprintf("answer(%d)", uint8(a))
	}
}

// ErrNoConsent is returned when the question could not be put to anybody: the
// input ended, the turn was cancelled, or the terminal broke. The [Answer] is
// [AnswerDeny] in every such case.
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
func (l *Loop) Ask(ctx context.Context, c Consent) (Answer, error) {
	if err := ctx.Err(); err != nil {
		return AnswerDeny, fmt.Errorf("%w: %w", ErrNoConsent, err)
	}

	l.Progress("")
	l.out.flushLine()
	l.renderRequest(c)

	answer, err := l.readAnswer(c)
	if err != nil {
		return AnswerDeny, err
	}
	if cerr := ctx.Err(); cerr != nil {
		// Answered, but the turn it belonged to is gone. Honouring it would
		// run the command the user just interrupted.
		return AnswerDeny, fmt.Errorf("%w: %w", ErrNoConsent, cerr)
	}
	return answer, nil
}

// renderRequest prints the question. One block, no box drawing, no cursor
// movement: it scrolls like everything else and can be selected and pasted
// into a bug report.
func (l *Loop) renderRequest(c Consent) {
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
func (l *Loop) readAnswer(c Consent) (Answer, error) {
	l.ed.SetPrompt(consentPrompt)
	defer l.ed.SetPrompt(l.prompt)

	reply, err := l.ed.ReadLine()
	l.out.afterPrompt(l.promptEndsLine)
	switch {
	case errors.Is(err, lineedit.ErrInterrupted):
		l.tag("perm", "denied: interrupted")
		return AnswerDeny, fmt.Errorf("%w: interrupted", ErrNoConsent)
	case errors.Is(err, io.EOF):
		l.tag("perm", "denied: no input to ask")
		return AnswerDeny, fmt.Errorf("%w: %w", ErrNoConsent, io.EOF)
	case err != nil:
		l.tag("perm", "denied: "+err.Error())
		return AnswerDeny, fmt.Errorf("%w: %w", ErrNoConsent, err)
	}

	answer := interpret(reply)
	l.tag("perm", fmt.Sprintf("%s: %s", orUnknown(c.Tool), answer))
	return answer, nil
}

// interpret maps a typed reply onto an answer. Anything that is not an
// affirmative is a refusal, including an empty line and including a typo.
func interpret(reply string) Answer {
	switch strings.ToLower(strings.TrimSpace(reply)) {
	case "y", "yes":
		return AnswerAllow
	case "a", "always":
		return AnswerAllowSession
	default:
		return AnswerDeny
	}
}
