package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/leejianrong/kopicode/internal/journal"
)

// This file is docs/adr/0009-ask-tool-contract.md's mechanism: a sibling to
// consent.go's Consenter/ConsentRequest/ConsentAnswer/ConsentMode, declared
// independently rather than reused. The ADR argues why at length (decision 2,
// Alternatives rejected): ask answers a question with free text, not a
// closed three-valued verdict, and folding the two together would mean
// either abusing a Verdict to carry prose or widening it for a concept that
// has nothing to do with authorizing an action. internal/permission is not
// extended for this — no OperationAsk, no KindAsk — because there is no
// classification rule to write: every ask call unconditionally needs an
// answer, so there is nothing for a Policy to decide beyond "did one arrive".

// AskRequest is a question the model asked, as a surface needs it.
type AskRequest struct {
	// Question is the text to show the human.
	Question string
	// Context is the model's optional supporting text — what it tried, why
	// it is stuck. Empty means the model gave none.
	Context string
}

// AskAnswer is what a surface returned.
type AskAnswer struct {
	// Text is the human's reply, verbatim. Empty is a legitimate answer — a
	// human who read the question and pressed Enter answered "nothing to
	// add," which is not the same event as the question going unanswered.
	Text string
}

// Answerer answers a question the model asked.
//
// Shaped like [Consenter] and crosses the boundary the same way, for the same
// reason: ctx first because an interactive implementation blocks on a human,
// and a turn the user just interrupted must abandon the question rather than
// answer it stale. It returns free text rather than a bounded enum because
// retrieving information, not authorizing an action, is the whole point of
// ask. An implementation that cannot get an answer returns an error — see
// [Engine.runAsk] for what that means downstream, which is deliberately
// *not* "the exchange ends" (ADR-0009 decision 2, Alternatives rejected).
type Answerer func(ctx context.Context, req AskRequest) (AskAnswer, error)

// AskMode says who [Options.Ask]'s replies are attributed to, mirroring
// [ConsentMode] for the identical reason: attribution is not the surface's
// fact to assert about itself, and a self-reported "I am a human" is not a
// control.
type AskMode uint8

const (
	// AskInteractive is the zero value: a human, relayed by a surface. This is
	// what the REPL uses and what a caller gets by not setting the field.
	AskInteractive AskMode = iota
	// AskUnattended says nobody is present; Answerer answers on its own — the
	// headless `run --print` surface's denyHeadlessAsk, for instance.
	AskUnattended
)

// The journal wire values for journal.AskAnswered.Source, kept here rather
// than imported from internal/permission: ask is never routed through that
// package (ADR-0009 decision 2), and permission.Source happening to stringify
// to the identical two words is a coincidence of vocabulary shared with
// journal.PermissionDecided, not a shared type.
const (
	askSourceUser   = "user"
	askSourcePolicy = "policy"
)

// noAnswerer is [Config.Ask]'s default when nothing was wired — a caller that
// builds a Config directly (internal/bench does; see [New]'s own comment) and
// never sets the field, or an [Options.Ask] left nil at [Open]. Every question
// fails closed with an error saying so, the same direction [asker.Ask] takes
// for a nil Consenter: an unanswerable question must never read as a "yes",
// and here, must never read as silence either.
func noAnswerer(context.Context, AskRequest) (AskAnswer, error) {
	return AskAnswer{}, errors.New("engine: no Answerer was configured, so this question cannot be answered by anyone")
}

// errAskRefused wraps whatever error an Answerer returned, so [faultOf]
// classifies a refused ask the same way it already classifies a denied
// run_shell: as tools.FaultTask ("the harness worked exactly as designed and
// the answer was no"), never tools.FaultInternal, which is what
// tools.FaultOf would otherwise return for an error it does not recognise.
var errAskRefused = errors.New("engine: the question went unanswered")

// askResultText is what the model sees for a successful ask call. It is not
// [modelText]: that function's "the tool produced no output" fallback is
// about a tool that failed to produce anything, and an empty human reply is
// not that — ADR-0009's Consequences section is explicit that free text has
// no safe default the way a refused permission does, so a human who pressed
// Enter on purpose gets a rendering that says so rather than one that reads
// like nothing happened.
func askResultText(a AskAnswer) string {
	if a.Text == "" {
		return "(the human answered with no additional text)"
	}
	return a.Text
}

// runAsk is the ask tool's own dispatch closure body (see dispatch.go's
// toolEntries): journal the question, call the configured Answerer directly
// — never through internal/permission, which ask does not use at all — and
// journal whatever came back.
//
// AskRequested is written before Ask runs, and unconditionally: a turn
// cancelled while the question is outstanding leaves it with no matching
// AskAnswered, which ADR-0009 decision 3 argues is the honest shape of what
// happened rather than a gap, the same way a PermissionRequested can stand
// alone with no PermissionDecided.
func (e *Engine) runAsk(ctx context.Context, turn int, callID string, a *askArgs) (toolOutcome, error) {
	if _, err := e.append(ctx, turn, journal.AskRequested{
		CallID:   callID,
		Question: journal.InlineText(a.Question),
		Context:  journal.InlineText(a.Context),
	}); err != nil {
		return toolOutcome{}, err
	}

	answer, aerr := e.cfg.Ask(ctx, AskRequest{Question: a.Question, Context: a.Context})
	if aerr != nil {
		detail := aerr.Error()
		if _, jerr := e.append(ctx, turn, journal.AskAnswered{
			CallID:  callID,
			Answer:  journal.InlineText(detail),
			Source:  e.cfg.AskSource,
			Refused: true,
		}); jerr != nil {
			return toolOutcome{}, jerr
		}
		// A refusal is non-fatal (ADR-0009 decision 2): the error travels in
		// toolOutcome.err purely so faultOf can classify it, and errAskRefused
		// is what steers that classification to tools.FaultTask instead of
		// tools.FaultOf's default of tools.FaultInternal. detail, not aerr, is
		// the model-visible text.
		return toolOutcome{output: detail, err: fmt.Errorf("%w: %w", errAskRefused, aerr)}, nil
	}

	if _, err := e.append(ctx, turn, journal.AskAnswered{
		CallID: callID,
		Answer: journal.InlineText(answer.Text),
		Source: e.cfg.AskSource,
	}); err != nil {
		return toolOutcome{}, err
	}
	return toolOutcome{output: askResultText(answer)}, nil
}
