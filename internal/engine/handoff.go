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

// ErrEmptyHandoff is returned by Handoff when the model's reply held none of the
// sections the draft asked for, so there is no narrative to hand over. The
// message says what the model returned instead.
var ErrEmptyHandoff = errors.New("engine: the model's reply held no handoff sections")

// clip shortens s to at most n runes for an error message, saying so.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

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

// handoffRoot is the directory whose .kopicode holds this session's record,
// which is where its handoff file goes beside it. A file journal says where it
// is; any other falls back to the working directory.
func (e *Engine) handoffRoot() string {
	if p, ok := e.cfg.Journal.(interface{ Path() string }); ok {
		// <root>/.kopicode/sessions/<id>/events.jsonl
		return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(p.Path()))))
	}
	return e.cfg.CWD
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

	// Models answer this request in two different wrong ways. With no tools on
	// the request, and a history full of tool calls, some return an empty reply;
	// with the tools offered, some return a tool call instead of text. The two
	// failures are complementary, so a draft tries the loop's own tools first
	// (which keeps the cached prefix), then none, then the tools again, and
	// reports the failure only when all three came back with no sections.
	var (
		reply     provider.Reply
		narrative handoff.Narrative
		failures  []string
	)
	for i, withTools := range []bool{true, false, true} {
		req := provider.Request{
			ModelID:   e.cfg.Selection.ModelID,
			Pin:       e.cfg.Selection.Pin,
			Sampling:  e.cfg.Selection.Config.RequestSampling(),
			Messages:  msgs,
			Turn:      e.turn,
			Attempt:   i,
			SessionID: e.cfg.SessionID,
		}
		if withTools {
			req.Tools = e.wireTools()
		}
		var err error
		if reply, err = e.draftOnce(ctx, req); err != nil {
			return HandoffResult{}, err
		}
		if narrative = handoff.ParseNarrative(reply.Content); narrative != (handoff.Narrative{}) {
			break
		}
		failures = append(failures, fmt.Sprintf("attempt %d (tools offered: %t): finish reason %q, %d tool call(s), reply %q",
			i+1, withTools, reply.FinishReason, len(reply.ToolCalls), clip(reply.Content, 120)))
	}
	// A reply with none of the sections would render a document that says
	// nothing but the facts, under headings that all read "did not say". That is
	// a failed draft, and it is reported as one: handing a successor a blank
	// handoff as if it were a real one is worse than handing it none.
	if narrative == (handoff.Narrative{}) {
		return HandoffResult{}, fmt.Errorf("%w: %s", ErrEmptyHandoff, strings.Join(failures, "; "))
	}

	// The engine's own journal, which knows where its record and its blobs are:
	// a runner may keep journals somewhere other than under the working
	// directory.
	var events []journal.Event
	for ev, rerr := range e.cfg.Journal.Read(ctx) {
		if rerr != nil {
			return HandoffResult{}, fmt.Errorf("engine: handoff: reading the journal for its facts: %w", rerr)
		}
		events = append(events, ev)
	}
	doc := handoff.Render(narrative, handoff.FactsFrom(events))

	path := HandoffPath(e.handoffRoot(), e.cfg.SessionID)
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
		Reply:    journal.InlineText(reply.Content),
	}); err != nil {
		return HandoffResult{}, err
	}
	return HandoffResult{Document: doc, Path: path}, nil
}

// StartFromHandoff tells a started engine's conversation the handoff doc as its
// first message, the way [Options.Handoff] does for a session [Open] builds. It
// is for a caller that builds its engine with [New], the benchmark runner being
// the one today. Call it after Start and before the first Run; the engine's
// Config.ParentSession names where the document came from.
func (e *Engine) StartFromHandoff(ctx context.Context, doc string) error {
	if !e.started || e.ended || e.turn != 0 {
		return fmt.Errorf("%w: StartFromHandoff needs a started engine that has run no turn", ErrConfig)
	}
	return e.loadHandoff(ctx, doc)
}

// draftOnce makes one drafting request and counts its spend. It adds nothing to
// the conversation.
func (e *Engine) draftOnce(ctx context.Context, req provider.Request) (provider.Reply, error) {
	stream, err := e.cfg.Provider.Complete(ctx, req)
	if err != nil {
		return provider.Reply{}, fmt.Errorf("engine: handoff: provider call: %w", err)
	}
	defer func() { _ = stream.Close() }()
	for stream.Next() {
	}
	reply, err := stream.Reply()
	if err != nil {
		return provider.Reply{}, fmt.Errorf("engine: handoff: provider reply: %w", err)
	}
	e.recordSpend(e.turn, reply)
	return reply, nil
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
