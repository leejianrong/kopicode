package repl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/leejianrong/kopicode/internal/engine"
)

// HandoffDraft is a drafted handoff document (ADR-0026) and the file it was
// written to.
type HandoffDraft struct {
	Document string
	Path     string
}

// HandoffControl is what /handoff needs from the session (ADR-0026). The loop
// never holds the session: it asks for a draft, lets the person edit the file,
// and hands the text back to start a new session from.
type HandoffControl struct {
	// Draft asks the session for a handoff, with the person's goal for the next
	// session when they gave one. It does not end the session.
	Draft func(ctx context.Context, goal string) (HandoffDraft, error)
	// Edit opens the file in the person's editor and returns when they are done.
	// Optional: nil means no editor is configured, and the loop says to edit the
	// file by hand.
	Edit func(path string) error
	// Read returns the file's text, edits included.
	Read func(path string) (string, error)
	// Start ends the current session and starts a new one from the document,
	// returning the new session's id. The old one is closed first because a
	// working tree has one session; an error here means no session is running,
	// and the loop ends.
	Start func(ctx context.Context, document string) (string, error)
}

const handoffPrompt = "start a new session from it? [y/N] "

// handoff handles /handoff [goal]. It returns an error only when the session
// has been closed and no new one could be opened, which ends the loop; a draft
// that failed, an editor that failed or a person who said no leaves the session
// exactly as it was.
func (l *Loop) handoff(ctx context.Context, goal string) error {
	h := l.handoffCtl
	if h.Draft == nil || h.Read == nil || h.Start == nil {
		l.Notice("/handoff is not available in this session")
		return nil
	}
	draft, ok := l.draftHandoff(ctx, goal)
	if !ok {
		return nil
	}
	doc, ok := l.confirmHandoff(draft)
	if !ok {
		return nil
	}
	id, err := h.Start(ctx, doc)
	if err != nil {
		l.Fail(err.Error() + "; the handoff is at " + draft.Path)
		return fmt.Errorf("repl: starting a session from the handoff: %w", err)
	}
	l.Notice("new session " + id + " started from the handoff; its verification starts as not run")
	return nil
}

// draftHandoff asks the session for a document and prints it. ok is false when
// there is none to go on with, having said why.
func (l *Loop) draftHandoff(ctx context.Context, goal string) (draft HandoffDraft, ok bool) {
	l.Notice("drafting a handoff; this is one model call, and the session is unchanged")
	var err error
	if l.interruptible(ctx, func(c context.Context) { draft, err = l.handoffCtl.Draft(c, goal) }) {
		l.Notice("handoff cancelled; the session is unchanged")
		return HandoffDraft{}, false
	}
	switch {
	case errors.Is(err, engine.ErrNothingToHand):
		l.Notice("nothing to hand off yet: this session has not run a turn")
		return HandoffDraft{}, false
	case err != nil:
		l.Fail(err.Error())
		return HandoffDraft{}, false
	}

	l.out.line("")
	for _, ln := range strings.Split(strings.TrimRight(draft.Document, "\n"), "\n") {
		l.out.line(ln)
	}
	l.out.line("")
	l.Notice("handoff written to " + draft.Path)
	return draft, true
}

// confirmHandoff lets the person edit the file, asks whether to start from it,
// and returns the file's text as it now stands. ok is false when nothing should
// start, having said why.
func (l *Loop) confirmHandoff(draft HandoffDraft) (doc string, ok bool) {
	h := l.handoffCtl
	if h.Edit != nil {
		l.Notice("opening it in your editor; save and quit to go on")
		if err := h.Edit(draft.Path); err != nil {
			l.Fail("the editor did not finish cleanly (" + err.Error() + "); nothing started, the handoff is at " + draft.Path)
			return "", false
		}
	} else {
		l.Notice("edit that file now if you want to change it (set $VISUAL or $EDITOR to have it opened for you)")
	}

	if !l.askYes(handoffPrompt) {
		l.Notice("staying in this session; the handoff stays at " + draft.Path)
		return "", false
	}

	doc, err := h.Read(draft.Path)
	if err != nil {
		l.Fail(err.Error())
		return "", false
	}
	if strings.TrimSpace(doc) == "" {
		l.Fail("the handoff file is empty, so nothing was started")
		return "", false
	}
	return doc, true
}

// askYes puts a yes/no question and reports whether the answer was an exact y
// or yes. Anything else, an empty line, EOF and an interrupt included, is no.
func (l *Loop) askYes(prompt string) bool {
	l.ed.SetPrompt(prompt)
	defer l.ed.SetPrompt(l.prompt)

	reply, err := l.ed.ReadLine()
	l.out.afterPrompt(l.promptEndsLine)
	// An interrupt, EOF or any read failure is not a yes.
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(reply)) {
	case "y", "yes":
		return true
	}
	return false
}

// goalOf is what follows the command word on a line, with its case intact:
// command lowercases for matching and a goal is the person's own words.
func goalOf(line string) string {
	line = strings.TrimSpace(line)
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return strings.TrimSpace(line[i+1:])
	}
	return ""
}
