package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/parse"
	"github.com/leejianrong/kopicode/internal/provider"
)

// Run is one bounded exchange: the user's message in, then prompt → provider →
// parse → dispatch → observe → repeat, until the loop stops.
//
// It is SLICE-1 affordance E1, and the whole of it. A single turn tree: no
// subagents, no planner, no second loop. What ends it:
//
//   - The model replies in prose and asks for no tool. That is the clean stop.
//   - The turn cap is reached ([Config.MaxTurns]).
//   - Reported token usage reaches the budget ([Config.TokenBudget]).
//   - The context is cancelled.
//   - The provider or the harness fails.
//
// A malformed tool call does **not** end it. Repair is bounded per reply, and a
// call that exhausts its budget continues the turn with the failure as an
// observation rather than aborting the session (docs/SLICE-1.md §3).
//
// The error is non-nil for [StopCancelled], [StopProviderError] and
// [StopHarnessError] — the outcomes a caller has to do something about — and
// nil for the three ordinary terminations, which the Result names. The
// session stays open either way; [Engine.Close] writes SessionEnded, so a REPL
// can cancel a turn and prompt again with the record intact.
func (e *Engine) Run(ctx context.Context, prompt string) (Result, error) {
	switch {
	case !e.started:
		return Result{}, ErrNotStarted
	case e.ended:
		return Result{}, ErrEnded
	}

	used := 0
	settle := func(stop Stop, cause error) (Result, error) {
		e.stop = stop
		e.detail = ""
		if cause != nil {
			e.detail = cause.Error()
		}
		return Result{Stop: stop, Turns: used, Tokens: e.spent}, cause
	}

	e.asm.AppendUser(prompt)
	if _, err := e.append(ctx, e.turn+1, journal.UserMessage{Text: journal.InlineText(prompt)}); err != nil {
		return settle(StopHarnessError, err)
	}

	for {
		if err := ctx.Err(); err != nil {
			return settle(StopCancelled, err)
		}
		if used >= e.cfg.MaxTurns {
			return settle(StopMaxTurns, nil)
		}
		if e.overBudget() {
			return settle(StopBudgetExhausted, nil)
		}

		used++
		e.turn++
		stop, err := e.runTurn(ctx, e.turn)
		if stop != StopUnspecified {
			return settle(stop, err)
		}
	}
}

// overBudget reports whether reported usage has reached the budget.
//
// Reported, and nothing else. The comparison is against
// provider.Usage summed across the session's replies — see the note on
// [Config.TokenBudget] for why an estimate may not stand in for it, and why
// the bound is therefore crossed rather than never reached.
func (e *Engine) overBudget() bool {
	return e.cfg.TokenBudget > 0 && e.spent.Total >= e.cfg.TokenBudget
}

// runTurn runs one turn, including any repair round trips inside it.
//
// It returns [StopUnspecified] to mean "the loop continues", which is the
// ordinary outcome: the model called tools, or its call failed and the failure
// became an observation. Anything else ends the exchange.
func (e *Engine) runTurn(ctx context.Context, turn int) (Stop, error) {
	// One repairer per turn, because the unit of repair is the reply: a reply
	// carrying one good call and one malformed one cannot have the good half
	// dispatched, so nothing is dispatched until all of it parses
	// (docs/SLICE-1.md §3). Sharing one across turns would share the budget,
	// which is the bound quietly ceasing to be one.
	rep := parse.NewRepairer(e.cfg.Catalogue, e.repairBudget())
	site := callSiteID(turn)

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return StopCancelled, err
		}
		if e.overBudget() {
			return StopBudgetExhausted, nil
		}

		reply, stop, err := e.call(ctx, turn, attempt)
		if stop != StopUnspecified {
			return stop, err
		}

		out := rep.Step(reply.Message())
		switch out.Event() {
		case parse.EventNone:
			// The model answered in prose. Nothing was malformed and nothing
			// is owed a repair: this is the clean stop.
			return StopCompleted, nil

		case parse.EventToolCallRepaired:
			if err := e.journalUnparsed(ctx, turn, site, reply, out); err != nil {
				return StopHarnessError, err
			}
			if err := e.observe(out.Feedback); err != nil {
				return StopHarnessError, err
			}
			continue

		case parse.EventToolCallFailed:
			if err := e.journalUnparsed(ctx, turn, site, reply, out); err != nil {
				return StopHarnessError, err
			}
			// The turn continues with the failure as an observation rather
			// than aborting the session.
			if err := e.observe(out.Feedback); err != nil {
				return StopHarnessError, err
			}
			return StopUnspecified, nil

		case parse.EventToolCallParsed:
			mutated, err := e.dispatch(ctx, turn, out.Extraction)
			if err != nil {
				return StopHarnessError, err
			}
			if mutated {
				if err := e.snapshot(ctx, turn); err != nil {
					return StopHarnessError, err
				}
			}
			// Forced verification (KAN-787) goes here: after a turn that
			// modified files, run the project's own command, journal
			// VerificationRun, and let a non-zero exit block a success report.
			// Nothing in this card runs it, and nothing here pretends to.
			return StopUnspecified, nil

		case parse.EventUnspecified:
			fallthrough
		default:
			return StopHarnessError, fmt.Errorf(
				"engine: turn %d attempt %d: the repair loop returned %s, which maps to no journal event",
				turn, attempt, out.Event())
		}
	}
}

// observe puts one piece of feedback into the conversation, by the route the
// reply it answers left open.
//
// This is not a stylistic choice, it is a wire requirement, and getting it
// wrong is invisible until the *next* request. A reply that carried native tool
// calls has an id waiting for a result whatever the harness thought of the
// call: a repair message appended as a user turn would leave those ids
// unanswered, and an OpenAI-compatible provider refuses that request with a 400
// several layers from the mistake. So a failure or a repair for a reply whose
// calls the provider issued travels back as the result of those calls; one for
// a reply that wrote its call as text — where there is no id to echo — travels
// back as a user turn.
//
// Every unanswered call gets the same text because the unit of repair is the
// reply and not the call: a reply carrying one good call and one malformed one
// is corrected as a whole rather than half-dispatched (docs/SLICE-1.md §3).
func (e *Engine) observe(text string) error {
	unanswered := e.asm.Unanswered()
	if len(unanswered) == 0 {
		e.asm.AppendUser(text)
		return nil
	}
	for _, id := range unanswered {
		if err := e.asm.AppendToolResult(id, text); err != nil {
			return fmt.Errorf("engine: answering call %q with the parse failure: %w", id, err)
		}
	}
	return nil
}

// call sends one request and returns the reply, journaling both.
func (e *Engine) call(ctx context.Context, turn, attempt int) (provider.Reply, Stop, error) {
	// An OpenAI-compatible provider refuses a request whose assistant message
	// made tool calls that nothing answers, and the 400 lands several layers
	// from the mistake. The assembler can see the pending calls and cannot know
	// a request is about to go out; this is the one place both are true.
	if left := e.asm.Unanswered(); len(left) > 0 {
		return provider.Reply{}, StopHarnessError, fmt.Errorf(
			"engine: turn %d attempt %d: %d tool call(s) would be sent unanswered: %v",
			turn, attempt, len(left), left)
	}

	req := provider.Request{
		ModelID:  e.cfg.ModelID,
		Pin:      e.cfg.Pin,
		Sampling: e.cfg.Sampling,
		Messages: e.asm.Messages(),
		Turn:     turn,
		Attempt:  attempt,
	}

	if _, err := e.append(ctx, turn, journal.ProviderRequest{
		ModelID: e.cfg.ModelID,
		Provider: journal.ProviderPin{
			Order:          e.cfg.Pin.Order,
			AllowFallbacks: e.cfg.Pin.AllowFallbacks,
			Quantizations:  e.cfg.Pin.Quantizations,
		},
		Sampling: journal.Sampling{
			Temperature: e.cfg.Sampling.Temperature,
			TopP:        e.cfg.Sampling.TopP,
			MaxTokens:   e.cfg.Sampling.MaxTokens,
			Seed:        e.cfg.Sampling.Seed,
		},
		// Tokens is left zero. There is no tokenizer here and the provider
		// reports prompt usage only in the reply, so the accounting lands on
		// ProviderResponse; see the field's own comment.
		Attempt: attempt,
	}); err != nil {
		return provider.Reply{}, StopHarnessError, err
	}

	stream, err := e.cfg.Provider.Complete(ctx, req)
	if err != nil {
		return provider.Reply{}, providerStop(err), fmt.Errorf(
			"engine: provider call turn %d attempt %d: %w", turn, attempt, err)
	}
	defer func() { _ = stream.Close() }()

	for stream.Next() {
		if e.cfg.Stream != nil {
			e.cfg.Stream(stream.Delta())
		}
	}
	reply, err := stream.Reply()
	if err != nil {
		return provider.Reply{}, providerStop(err), fmt.Errorf(
			"engine: provider reply turn %d attempt %d: %w", turn, attempt, err)
	}

	if _, err := e.append(ctx, turn, journal.ProviderResponse{
		Body: journal.InlineText(string(reply.Raw)),
		Tokens: journal.TokenCounts{
			Prompt:     reply.Usage.Prompt,
			Completion: reply.Usage.Completion,
			Total:      reply.Usage.Total,
		},
		FinishReason: reply.FinishReason,
		ServedBy:     reply.ServedBy,
	}); err != nil {
		return provider.Reply{}, StopHarnessError, err
	}

	// The budget's only input, added the moment the provider states it.
	e.spent.Prompt += reply.Usage.Prompt
	e.spent.Completion += reply.Usage.Completion
	e.spent.Total += reply.Usage.Total

	// History before events, so a journal write that fails cannot leave the
	// conversation holding a reply the record does not.
	e.asm.AppendAssistant(reply)

	if reply.Reasoning != "" {
		if _, err := e.append(ctx, turn, journal.ThinkingBlock{Text: journal.InlineText(reply.Reasoning)}); err != nil {
			return provider.Reply{}, StopHarnessError, err
		}
	}
	if reply.Content != "" {
		if _, err := e.append(ctx, turn, journal.AssistantMessage{Text: journal.InlineText(reply.Content)}); err != nil {
			return provider.Reply{}, StopHarnessError, err
		}
	}
	return reply, StopUnspecified, nil
}

// providerStop tells a cancelled call from a failed one.
//
// A Ctrl-C that lands while a reply is streaming arrives as a stream error
// wrapping context.Canceled, and reporting it as a provider error would put
// every interrupted turn in ADR-0006's harness bucket against an acceptance
// criterion of zero.
func providerStop(err error) Stop {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return StopCancelled
	}
	return StopProviderError
}

// journalUnparsed records a reply the extractor could not turn into calls: the
// text as the model emitted it, then the repair request or the give-up.
//
// ToolCallRequested carries the reply's own text here rather than one call's,
// because there is no call — that is the point. Malformed calls are the corpus
// the repair path is measured against (KAN-777), and a journal that recorded
// only the classification would leave nothing to re-examine the classification
// against.
func (e *Engine) journalUnparsed(ctx context.Context, turn int, site string, reply provider.Reply, out parse.Outcome) error {
	if _, err := e.append(ctx, turn, journal.ToolCallRequested{
		CallID: site,
		Raw:    journal.InlineText(reply.Content),
	}); err != nil {
		return err
	}

	classification, tool := "", ""
	if out.Failure != nil {
		classification, tool = out.Failure.Kind.String(), out.Failure.Tool
	}

	switch out.Event() {
	case parse.EventToolCallRepaired:
		_, err := e.append(ctx, turn, journal.ToolCallRepaired{
			CallID:         site,
			Attempt:        out.Attempt,
			Classification: classification,
			Error:          journal.InlineText(out.Feedback),
		})
		return err
	default:
		_, err := e.append(ctx, turn, journal.ToolCallFailed{
			CallID: site,
			Tool:   tool,
			Reason: classification,
			Detail: journal.InlineText(out.Feedback),
		})
		return err
	}
}

// snapshot records the working tree after a turn that ran a tool able to change
// it, and journals the shadow ref.
func (e *Engine) snapshot(ctx context.Context, turn int) error {
	if e.cfg.Snapshots == nil {
		return nil
	}
	snap, err := e.cfg.Snapshots.Snapshot(ctx, turn)
	if err != nil {
		return fmt.Errorf("engine: snapshotting turn %d: %w", turn, err)
	}
	_, err = e.append(ctx, turn, journal.TurnSnapshot{
		Ref:    snap.Ref,
		Commit: snap.Commit,
		Tree:   snap.Tree,
		Parent: snap.Parent,
	})
	return err
}

// callSiteID is the identifier for a turn's call site — the thing a repair
// round trip is about when there is no parsed call to name.
//
// Derived from the turn and nothing else: no clock, no counter, no randomness,
// so a replayed session mints the same ids and the journal compares byte for
// byte.
func callSiteID(turn int) string { return fmt.Sprintf("kc-%d", turn) }

// callID is the identifier for one call the model made on a text route, where
// the provider issued none. A native call keeps the provider's own id.
func callID(turn, index int) string { return fmt.Sprintf("kc-%d-%d", turn, index) }
