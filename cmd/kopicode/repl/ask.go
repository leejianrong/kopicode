package repl

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/leejianrong/kopicode/cmd/kopicode/lineedit"
	"github.com/leejianrong/kopicode/internal/engine"
)

// engine.AskRequest and engine.AskAnswer are the engine's types, for the
// reason consent.go's own note gives about ConsentRequest/ConsentAnswer: a
// third pair here would be a translation table between two vocabularies that
// already agree.

// ErrNoAnswer is returned when a question could not be put to anybody: the
// input ended, the turn was cancelled, or the terminal broke. It mirrors
// [ErrNoConsent] rather than reusing it, so a caller matching on one never
// accidentally also matches the other's failure.
var ErrNoAnswer = errors.New("repl: the question could not be answered")

// AnswerAsk puts a question the model asked to the user and returns their
// reply.
//
// Unlike [Loop.Ask], there is no y/n/a interpretation: whatever the human
// types is the answer, verbatim, including an empty line.
// docs/adr/0009-ask-tool-contract.md's Consequences section is explicit that
// free text has no safe default the way a refused permission does, so an
// empty reply a human actually pressed Enter on is a real answer ("nothing
// to add"), not a refusal — a deliberate departure from consent's "empty
// means no" convention. Only Ctrl-C, EOF and a cancelled turn are
// unanswerable.
func (l *Loop) AnswerAsk(ctx context.Context, req engine.AskRequest) (engine.AskAnswer, error) {
	if err := ctx.Err(); err != nil {
		return engine.AskAnswer{}, fmt.Errorf("%w: %w", ErrNoAnswer, err)
	}

	l.Progress("")
	l.out.flushLine()
	l.renderAsk(req)

	reply, err := l.readAskReply()
	if err != nil {
		return engine.AskAnswer{}, err
	}
	if cerr := ctx.Err(); cerr != nil {
		// Answered, but the turn it belonged to is gone. The turn that reads
		// this answer must be the one that asked, not a stale leftover from
		// before the interruption.
		return engine.AskAnswer{}, fmt.Errorf("%w: %w", ErrNoAnswer, cerr)
	}
	return engine.AskAnswer{Text: reply}, nil
}

// renderAsk prints the question and, when the model gave one, its context.
// Same convention as consent.go's renderRequest: one block, no box drawing,
// no cursor movement — it scrolls like everything else and can be selected
// and pasted into a bug report.
func (l *Loop) renderAsk(req engine.AskRequest) {
	l.out.line(l.out.bold("[ask] ") + "the model has a question")
	l.out.line("      " + req.Question)
	if req.Context != "" {
		l.indent(req.Context)
	}
}

// askPrompt replaces the ordinary prompt while a question is outstanding.
const askPrompt = "your answer: "

// readAskReply asks once and returns the line verbatim.
//
// One read only, for the same reason readAnswer is: a loop that re-prompted
// on some input it did not like would be a loop a piped session cannot
// leave, and there is nothing here to dislike anyway — every line is a valid
// answer, including the empty one.
func (l *Loop) readAskReply() (string, error) {
	l.ed.SetPrompt(askPrompt)
	defer l.ed.SetPrompt(l.prompt)

	reply, err := l.ed.ReadLine()
	l.out.afterPrompt(l.promptEndsLine)
	switch {
	case errors.Is(err, lineedit.ErrInterrupted):
		l.tag("ask", "unanswered: interrupted")
		return "", fmt.Errorf("%w: interrupted", ErrNoAnswer)
	case errors.Is(err, io.EOF):
		l.tag("ask", "unanswered: no input to answer with")
		return "", fmt.Errorf("%w: %w", ErrNoAnswer, io.EOF)
	case err != nil:
		l.tag("ask", "unanswered: "+err.Error())
		return "", fmt.Errorf("%w: %w", ErrNoAnswer, err)
	}

	l.tag("ask", "answered")
	return reply, nil
}
