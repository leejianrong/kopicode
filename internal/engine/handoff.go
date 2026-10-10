package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/leejianrong/kopicode/internal/handoff"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider"
)

// HandoffResult is a drafted handoff (ADR-0026).
type HandoffResult struct {
	// Document is the whole text: the model's sections, then the facts block
	// kopicode wrote from the journal.
	Document string
	// Path is the projection file under .kopicode/handoff, absolute. A person
	// may edit it before a new session starts from it, and [ReadHandoff] reads
	// it back.
	Path string
}

// ErrNothingToHand is returned by Handoff when the session has said nothing yet,
// so there is nothing a successor could need.
var ErrNothingToHand = errors.New("engine: nothing to hand off: this session has not run a turn")

// handoffDir is where handoff documents are projected, beside the sessions.
func handoffDir(root string) string { return filepath.Join(root, ".kopicode", "handoff") }

// HandoffPath is where session id's handoff document is written under root.
func HandoffPath(root, id string) string { return filepath.Join(handoffDir(root), id+".md") }

// ReadHandoff reads a handoff document back, which is how an edit a person made
// to the projection file reaches the new session.
func ReadHandoff(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("engine: reading handoff: %w", err)
	}
	return string(data), nil
}

// Handoff drafts the handoff document for this session (ADR-0026): one model
// call with no tools, from the same history the session has, asking for the
// narrative sections; then the facts block, built from the journal, appended.
//
// It does not end the session and it does not touch the conversation: the
// request and the reply are not added to the history, so a session that is kept
// after a handoff carries on exactly as it was. What it leaves behind is the
// HandoffWritten event, the projection file, and the spend, which counts toward
// the budget like any other call.
//
// goal, when given, is the user's statement of what the next session is for.
func (e *Engine) Handoff(ctx context.Context, goal string) (HandoffResult, error) {
	if !e.started || e.ended {
		return HandoffResult{}, fmt.Errorf("%w: Handoff needs a started, open session", ErrConfig)
	}
	if e.turn == 0 {
		return HandoffResult{}, ErrNothingToHand
	}
	if left := e.asm.Unanswered(); len(left) > 0 {
		return HandoffResult{}, fmt.Errorf("engine: handoff: %d tool call(s) are unanswered: %v", len(left), left)
	}
	goal = strings.TrimSpace(goal)

	msgs := append(e.asm.Messages(), provider.Message{Role: provider.RoleUser, Content: handoff.Prompt(goal)})
	stream, err := e.cfg.Provider.Complete(ctx, provider.Request{
		ModelID:   e.cfg.Selection.ModelID,
		Pin:       e.cfg.Selection.Pin,
		Sampling:  e.cfg.Selection.Config.RequestSampling(),
		Messages:  msgs,
		Turn:      e.turn,
		SessionID: e.cfg.SessionID,
	})
	if err != nil {
		return HandoffResult{}, fmt.Errorf("engine: handoff: provider call: %w", err)
	}
	defer func() { _ = stream.Close() }()
	for stream.Next() {
	}
	reply, err := stream.Reply()
	if err != nil {
		return HandoffResult{}, fmt.Errorf("engine: handoff: provider reply: %w", err)
	}
	e.recordSpend(e.turn, reply)

	var events []journal.Event
	for ev, rerr := range journal.ReadSessionBlobs(ctx, e.cfg.CWD, e.cfg.SessionID) {
		if rerr != nil {
			return HandoffResult{}, fmt.Errorf("engine: handoff: reading the journal for its facts: %w", rerr)
		}
		events = append(events, ev)
	}
	doc := handoff.Render(handoff.ParseNarrative(reply.Content), handoff.FactsFrom(events))

	path := HandoffPath(e.cfg.CWD, e.cfg.SessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return HandoffResult{}, fmt.Errorf("engine: handoff: %w", err)
	}
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		return HandoffResult{}, fmt.Errorf("engine: handoff: writing %s: %w", path, err)
	}
	if _, err := e.append(ctx, e.turn, journal.HandoffWritten{
		Goal:     goal,
		Document: journal.InlineText(doc),
		Path:     path,
	}); err != nil {
		return HandoffResult{}, err
	}
	return HandoffResult{Document: doc, Path: path}, nil
}

// Handoff drafts this session's handoff. See [Engine.Handoff].
func (s *Session) Handoff(ctx context.Context, goal string) (HandoffResult, error) {
	return s.engine.Handoff(ctx, goal)
}

// parentSession is the session a new one's handoff came from, or "" when the
// session is not starting from one.
func parentSession(opts Options) string {
	if opts.Handoff == "" || opts.Resume || opts.Fork != nil {
		return ""
	}
	return opts.ParentSession
}

// loadHandoff journals and feeds a handoff into a new session as its one
// starting message (ADR-0026 decision 7): a user turn under a <handoff> tag.
// Like the skills catalogue it reuses ProjectInstructionsLoaded, here with scope
// "handoff", so resume and fork replay it from the journal and no new event type
// is needed. Nothing is recorded when there is no handoff.
func (e *Engine) loadHandoff(ctx context.Context, doc string) error {
	doc = strings.TrimSpace(doc)
	if doc == "" {
		return nil
	}
	msg := "You are continuing work another session started. Its handoff follows; it is all you have of that session. " +
		"Nothing it says has been verified in this session.\n\n<handoff>\n" + doc + "\n</handoff>"
	if _, err := e.append(ctx, 0, journal.ProjectInstructionsLoaded{
		Content: journal.InlineText(msg),
		Scope:   journal.InstructionsScopeHandoff,
	}); err != nil {
		return fmt.Errorf("engine: recording the handoff: %w", err)
	}
	e.asm.AppendUser(msg)
	return nil
}
