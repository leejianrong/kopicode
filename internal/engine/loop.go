package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
//   - The turn cap is reached (Selection.Config.MaxTurns).
//   - Reported token usage reaches the budget (Selection.Config.TokenBudget).
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

	// The phase is per exchange: a REPL session that cancels turn 1, runs turn 2
	// to completion and cancels turn 5 must not record turn 5 as interrupted
	// wherever turn 1 was.
	e.cancelPhase = ""

	// at is the turn events belong to, which is not e.turn: the loop increments
	// that only once a turn actually starts, and the user's message — and a
	// cancellation that lands before the first provider call — belong to the
	// turn about to run rather than to the one that already finished.
	at := e.turn + 1

	settle := func(stop Stop, cause error) (Result, error) {
		e.stop = stop
		e.detail = ""
		switch {
		case cause != nil:
			e.detail = cause.Error()
		case stop == StopVerificationFailed:
			// The one nil-error stop that has something to say. Without this,
			// SessionEnded would record "verification_failed" and nothing about
			// which command said so.
			e.detail = e.unverified
		}
		if stop == StopCancelled {
			if err := e.journalCancellation(ctx, at, cause); err != nil {
				// A record that could not say the turn was interrupted is a
				// broken record, and the loop reports its own breakage rather
				// than returning a tidy cancellation over a journal that lost
				// it. Every other journal failure in this file ends the exchange
				// the same way.
				stop, cause = StopHarnessError, err
				e.stop, e.detail = stop, cause.Error()
			}
		}
		return Result{Stop: stop, Turns: used, Tokens: e.spent}, cause
	}

	e.asm.AppendUser(prompt)
	if _, err := e.append(ctx, at, journal.UserMessage{Text: journal.InlineText(prompt)}); err != nil {
		return settle(StopHarnessError, err)
	}

	for {
		if err := ctx.Err(); err != nil {
			e.noteCancelled(phaseBetweenSteps)
			return settle(StopCancelled, err)
		}
		if used >= e.cfg.Selection.Config.MaxTurns {
			return settle(StopMaxTurns, nil)
		}
		if e.overBudget() {
			return settle(StopBudgetExhausted, nil)
		}

		used++
		e.turn++
		at = e.turn
		stop, err := e.runTurn(ctx, e.turn)
		if stop != StopUnspecified {
			return settle(stop, err)
		}
	}
}

// overBudget reports whether reported usage has reached the budget.
//
// **What "enforced" can mean for this bound, stated rather than implied.** The
// only authoritative token count is provider.Usage on a reply, so it arrives
// after the request that spent it. The budget is therefore a stop condition on
// *spend to date*, checked before every provider call: once the usage the
// provider has reported reaches it, no further request is sent and the exchange
// stops with reason "budget_exhausted". A session can exceed the budget by at
// most the usage of the request in flight when it was crossed.
//
// It is deliberately not admission control from [Size.EstimatedTokens]. That
// method documents itself as not a token count, and refusing a request on a byte
// estimate while journaling it as a budget decision is fabricated precision —
// a guess wearing a measurement's clothes.
func (e *Engine) overBudget() bool {
	budget := e.cfg.Selection.Config.TokenBudget
	return budget > 0 && e.spent.Total >= budget
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
	rep := parse.NewRepairer(e.cfg.Catalogue, e.cfg.Selection.Config.RepairBudget)
	site := callSiteID(turn)

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			e.noteCancelled(phaseBetweenSteps)
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
			// is owed a repair: this is the clean stop — unless the project's
			// own verification command has rejected the tree the model is
			// declaring finished, in which case it is not a clean anything
			// (docs/SLICE-1.md §5).
			if e.unverified != "" {
				return StopVerificationFailed, nil
			}
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
				if stop, err := e.verify(ctx, turn); stop != StopUnspecified {
					return stop, err
				}
			}
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

// wireTools is the tool catalogue to advertise on the wire this request, or
// nil to advertise none.
//
// Whether native tool-calling is advertised at all is
// Selection.Config.AdvertiseNativeTools — a harness value, not a constant
// here, because SLICE-1 §3 has three extraction routes precisely because not
// every model reliably uses the native one: whether an arm *offers* the native
// route is exactly the kind of thing a second arm might vary, and a bool this
// package decided for itself would be a bound outside the hash the same way a
// turn cap held in the loop instead of the config would be (see [Config]'s own
// doc comment on that point). false does not narrow what the loop will
// dispatch: Selection.Config.ToolSet still decides that on its own, same as
// overriding Config.Catalogue does not widen it.
//
// The order is ToolSet's, not Catalogue.Names()'s: [Catalogue] sorts its name
// list for a stable unknown-tool message, but ToolSet is presentation order —
// the same order the system prompt's sections follow (KAN-843) — and a wire
// catalogue is presentation too. A name ToolSet lists that the catalogue has
// no schema for is skipped rather than erred on: [Config.Catalogue] can be
// overridden to a narrower stand-in in a test, and a wire-rendering path is
// not the place to enforce the completeness [New] already checked once.
func (e *Engine) wireTools() []parse.Schema {
	if !e.cfg.Selection.Config.AdvertiseNativeTools {
		return nil
	}
	var out []parse.Schema
	for _, name := range e.cfg.Selection.Config.ToolSet {
		if schema, ok := e.cfg.Catalogue.Schema(name); ok {
			out = append(out, schema)
		}
	}
	return out
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

	sampling := e.cfg.Selection.Config.RequestSampling()
	req := provider.Request{
		ModelID:  e.cfg.Selection.ModelID,
		Pin:      e.cfg.Selection.Pin,
		Sampling: sampling,
		Messages: e.asm.Messages(),
		Tools:    e.wireTools(),
		Turn:     turn,
		Attempt:  attempt,
	}

	if _, err := e.append(ctx, turn, journal.ProviderRequest{
		ModelID:  e.cfg.Selection.ModelID,
		Provider: journalPin(e.cfg.Selection.Pin),
		Sampling: journal.Sampling{
			Temperature: sampling.Temperature,
			TopP:        sampling.TopP,
			MaxTokens:   sampling.MaxTokens,
			Seed:        sampling.Seed,
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
		return provider.Reply{}, e.noteProviderStop(err), fmt.Errorf(
			"engine: provider call turn %d attempt %d: %w", turn, attempt, err)
	}
	defer func() { _ = stream.Close() }()

	for stream.Next() {
		if e.cfg.Stream != nil {
			e.cfg.Stream(turn, stream.Delta())
		}
	}
	reply, err := stream.Reply()
	if err != nil {
		return provider.Reply{}, e.noteProviderStop(err), fmt.Errorf(
			"engine: provider reply turn %d attempt %d: %w", turn, attempt, err)
	}

	if _, err := e.append(ctx, turn, journal.ProviderResponse{
		Body: responseBody(stream, reply),
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

// responseBody is what journal.ProviderResponse.Body records, and the two
// sources are not interchangeable.
//
//   - **Replay** has an assembled body beside the frames, and [provider.Reply].Raw
//     passes it through untouched. The record then holds the bytes the recording
//     holds.
//   - **A live stream has no assembled body at all.** OpenRouter never sends one,
//     so Raw is nil there — deliberately, because building one out of the chunks
//     would put a re-encoding in the one field whose whole point is that it is
//     not one. What the provider did send is the frames, which
//     [provider.Stream.Transcript] returns verbatim: keep-alive comments, blank
//     separators and the [DONE] sentinel included.
//
// Journaling Raw unconditionally would record nothing at all against the real
// client while looking perfectly correct against the mock, which is the exact
// failure shape this repo keeps finding. Both are handled, and which one applied
// is visible in the record: a transcript starts `data: `, an assembled body
// starts `{`.
//
// Call it only after the stream is drained. Transcript's "so far" is literal —
// the scanner reads ahead — and mid-stream it can hold more than the deltas
// delivered.
func responseBody(stream *provider.Stream, reply provider.Reply) journal.Text {
	if len(reply.Raw) > 0 {
		return journal.InlineText(string(reply.Raw))
	}
	return journal.InlineText(string(stream.Transcript()))
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

// verify runs forced verification after a turn that could have changed the tree
// and journals what it produced.
//
// It returns [StopUnspecified] to mean "the loop continues", which is the
// outcome for a pass, for a failure, and for a verification that could not run.
// A failure does not end the exchange: docs/SLICE-1.md §5 says the failure
// becomes the next turn's observation, so the model is shown the suite's output
// and gets to fix it. What a failure does is set [Engine.unverified], which is
// what the prose stop consults before it is allowed to call itself completed.
//
// **Verification is skipped entirely when the arm does not force it.**
// Selection.Config.Verification.Forced is in the harness config hash, so a run
// that verified under a configuration saying it does not would be an arm doing
// something the value identifying it denies.
//
// The full output is journaled whether it passed or failed. A passing run is the
// evidence that the tree was verified at all, and a record that held only the
// failures could not tell "it passed" from "nobody looked".
func (e *Engine) verify(ctx context.Context, turn int) (Stop, error) {
	if !e.cfg.Selection.Config.Verification.Forced {
		return StopUnspecified, nil
	}

	res, verr := e.cfg.Verify.Run(ctx)
	if _, err := e.append(ctx, turn, journal.VerificationRun{
		Command: res.Command,
		Source:  string(res.Source),
		// Never 0 for a run that did not happen: internal/verify guarantees -1,
		// and this copies rather than recomputes so the two cannot disagree.
		ExitCode: res.ExitCode,
		// Skip carries verify.Result.Skip's reason across, "" when the run
		// concluded — KAN-876, so a tool-missing, broken-command, timed-out or
		// cancelled non-verdict is distinguishable on the record rather than
		// collapsing into "ExitCode < 0".
		Skip:   string(res.Skip),
		Output: journal.InlineText(res.Output),
	}); err != nil {
		return StopHarnessError, err
	}
	if verr != nil {
		// The only error internal/verify returns is a cancellation, and the
		// result beside it has already been recorded above.
		e.noteCancelled(phaseVerification)
		return StopCancelled, fmt.Errorf("engine: verification on turn %d: %w", turn, verr)
	}

	switch {
	case res.Blocks():
		// A sentence, not the whole suite output: this ends up on
		// SessionEnded.Detail, and the output itself is already on the
		// VerificationRun event above. One record, said once.
		e.unverified = fmt.Sprintf("`%s` exited %d", strings.Join(res.Command, " "), res.ExitCode)
		// The failure travels back as the next turn's observation. Every tool
		// call is already answered at this point, so this lands as a user turn.
		if err := e.observe(res.Output); err != nil {
			return StopHarnessError, err
		}
	case res.Ran():
		// A passing run answers an outstanding rejection; nothing else does.
		e.unverified = ""
	}
	return StopUnspecified, nil
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
