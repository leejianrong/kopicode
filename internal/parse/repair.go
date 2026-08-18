package parse

import (
	"errors"
	"fmt"
)

// DefaultBudget is the repair budget docs/SLICE-1.md §3 specifies: two repair
// attempts per call, then the call fails and the turn continues with the
// failure as an observation.
const DefaultBudget = 2

// Event names the journal event an [Outcome] maps to.
//
// This package does not import internal/journal and must not — the engine
// journals, the parser parses, and that arrow points one way. What this package
// owes instead is that every outcome it can produce maps onto exactly one event
// without the engine having to guess, which is what makes parse-success rate
// and repair-recovery rate measurable from the journal rather than anecdotal.
type Event uint8

const (
	// EventUnspecified is the zero value. The loop never produces it; an
	// Outcome reporting it was not produced by [Repairer.Step].
	EventUnspecified Event = iota

	// EventNone means no tool-call event is owed: the model replied in prose.
	// The engine journals the reply as an AssistantMessage and moves on. This
	// is the benign outcome, and it is deliberately not a failure.
	EventNone

	// EventToolCallParsed means the calls are dispatchable. It maps to one
	// ToolCallParsed per call in [Outcome.Extraction]. When
	// [Outcome.Recovered] is true, this is the event that closes a repair
	// round trip, and the pair of it with the preceding ToolCallRepaired is
	// the repair-recovery rate.
	EventToolCallParsed

	// EventToolCallRepaired means a repair was requested. It maps to one
	// ToolCallRepaired carrying [Outcome.Attempt] and [Outcome.Feedback].
	EventToolCallRepaired

	// EventToolCallFailed means the budget is spent, or the failure was never
	// repairable. It maps to one ToolCallFailed. The turn does not abort.
	EventToolCallFailed
)

var eventText = map[Event]string{
	EventUnspecified:      "unspecified",
	EventNone:             "none",
	EventToolCallParsed:   "tool_call_parsed",
	EventToolCallRepaired: "tool_call_repaired",
	EventToolCallFailed:   "tool_call_failed",
}

// String returns the wire form of the event.
func (e Event) String() string {
	if s, ok := eventText[e]; ok {
		return s
	}
	return fmt.Sprintf("event(%d)", uint8(e))
}

// Outcome is what one assistant reply produced, and what the engine should do
// about it.
//
// Its event is unexported and has no setter, so the only Outcome carrying one
// is an Outcome [Repairer.Step] returned — the same reason [Extraction] hides
// its route. An outcome that does not say which event it maps to is not
// representable, because an outcome that could omit it would get omitted, and
// the rates this card exists to make measurable would quietly stop adding up.
type Outcome struct {
	// Attempt numbers the repair round trip.
	//
	// On [EventToolCallRepaired] it is the 1-based number of the repair being
	// requested, which is exactly what ToolCallRepaired.Attempt records. On
	// [EventToolCallParsed] and [EventToolCallFailed] it is the number of
	// repairs already spent — 0 on a first-try parse, the budget on a give-up.
	Attempt int

	// Extraction holds the dispatchable calls. It is populated only on
	// [EventToolCallParsed]; a call site that failed dispatches nothing, and a
	// half-dispatched batch would leave the transcript describing calls the
	// model never made.
	Extraction Extraction

	// Failure is the classification, on [EventToolCallRepaired] and
	// [EventToolCallFailed], and nil otherwise. Failure.Kind is the string the
	// journal records as the classification; Failure.Tool is what
	// ToolCallFailed.Tool carries, empty when the call never named one.
	Failure *Error

	// Feedback is the message the model is given: the repair request on
	// [EventToolCallRepaired], and the observation the turn continues with on
	// [EventToolCallFailed]. Empty on the other events.
	Feedback string

	event Event
}

// Event returns the journal event this outcome maps to.
func (o Outcome) Event() Event { return o.event }

// Recovered reports whether this outcome is a parse that a repair rescued —
// the numerator of the repair-recovery rate.
func (o Outcome) Recovered() bool { return o.event == EventToolCallParsed && o.Attempt > 0 }

// Terminal reports whether the call site is settled. Only
// [EventToolCallRepaired] is not: it is the one outcome that asks for another
// reply.
func (o Outcome) Terminal() bool {
	return o.event != EventToolCallRepaired && o.event != EventUnspecified
}

// Repairer runs the bounded repair loop for one call site.
//
// The engine drives it: feed a reply to [Repairer.Step], and if the outcome is
// [EventToolCallRepaired], send Feedback back to the model and feed the next
// reply to the same Repairer. The Repairer holds the attempt count, so one
// value per call site is the contract — sharing one across sites would share
// the budget, which is the bound quietly stopping being a bound.
//
// The unit of repair is the reply, not the individual call: a reply that failed
// to parse contains no call to attach a budget to, and a reply whose second
// call is invalid is corrected as a whole rather than half-dispatched.
type Repairer struct {
	tools   Tools
	budget  int
	order   []Route
	repairs int
	done    bool
}

// NewRepairer returns a repair loop over tools, allowing at most budget repair
// attempts before the call fails, and trying routes in order when it extracts
// each reply.
//
// The budget is a parameter rather than a constant in the loop because varying
// it is a bench arm: measuring what repair buys means being able to run with
// none. Pass [DefaultBudget] unless that is what you are doing. A budget of
// zero disables repair — the first malformed call fails immediately — and a
// negative budget is read as zero.
//
// order is the same discipline applied to extraction route order (KAN-855): a
// harness configuration's ParseRoutes names the routes an arm is willing to
// accept, and this is the one place that name has to reach in order to mean
// anything, because [Extract] itself always uses its own package default and
// does not take an order — see its doc comment. nil means
// [DefaultRouteOrder]; pass nil unless varying the order is the point, the same
// convention [Extract] follows via [DefaultRouteOrder]. An explicit non-nil
// empty slice is a real, if deliberately degenerate, arm: no route is ever
// tried, so every reply this loop sees is read as prose, never as a call.
//
// tools may be nil, in which case the semantic classifications are unreachable
// and only extraction failures are repaired.
func NewRepairer(tools Tools, budget int, order []Route) *Repairer {
	if budget < 0 {
		budget = 0
	}
	if order == nil {
		order = DefaultRouteOrder()
	}
	return &Repairer{tools: tools, budget: budget, order: order}
}

// Budget returns the repair budget this loop was built with.
func (r *Repairer) Budget() int { return r.budget }

// Order returns the extraction route order this loop was built with. It is a
// copy: an order a caller mutated in place after handing it to [NewRepairer]
// would be rewriting the arm's own bound out from under it.
func (r *Repairer) Order() []Route { return append([]Route(nil), r.order...) }

// Repairs returns how many repair attempts have been spent so far.
func (r *Repairer) Repairs() int { return r.repairs }

// Step feeds one assistant reply to the loop and reports what the engine should
// do next.
//
// It never panics and never returns an error: a model reply is untrusted input
// by definition, and every way it can be wrong is already a classification. The
// failure path is an outcome, not an exception, because the turn continues
// either way.
func (r *Repairer) Step(msg Message) Outcome {
	if r.done {
		// Reusing a settled loop is a harness bug, not a model failure, so it
		// is not repaired and not attributed to the model.
		return r.fail(&Error{
			Kind:   KindUnspecified,
			Detail: "internal: the repair loop for this call site was already settled",
		})
	}

	ext, err := extractOrder(msg, r.order)
	if err != nil {
		return r.classify(err)
	}

	for _, call := range ext.Calls() {
		if perr := validate(r.tools, ext.Route(), call); perr != nil {
			return r.repairOrFail(perr)
		}
	}

	r.done = true
	return Outcome{
		Attempt:    r.repairs,
		Extraction: ext,
		event:      EventToolCallParsed,
	}
}

// classify turns an extraction error into an outcome.
func (r *Repairer) classify(err error) Outcome {
	var perr *Error
	if !errors.As(err, &perr) {
		// Extract only ever returns *Error, so this is unreachable today. It
		// is handled rather than asserted because an unclassified failure that
		// reached the model as a repair request would be a harness bug asking
		// the model to fix it.
		return r.fail(&Error{
			Kind:   KindUnspecified,
			Detail: "internal: extraction failed without a classification",
			err:    err,
		})
	}

	// The one benign outcome: the model answered in prose. Nothing was
	// malformed, so nothing is owed a repair.
	if perr.Kind == KindNoCall {
		r.done = true
		return Outcome{Attempt: r.repairs, event: EventNone}
	}

	return r.repairOrFail(perr)
}

// repairOrFail is where the bound lives, and it is the only place it lives.
func (r *Repairer) repairOrFail(perr *Error) Outcome {
	if !repairable(perr.Kind) {
		return r.fail(perr)
	}
	if r.repairs >= r.budget {
		return r.fail(perr)
	}

	r.repairs++
	return Outcome{
		Attempt:  r.repairs,
		Failure:  perr,
		Feedback: repairMessage(r.tools, perr, false),
		event:    EventToolCallRepaired,
	}
}

// fail settles the call site. The turn is not over: Feedback is the observation
// it continues with, which is the behaviour docs/SLICE-1.md §3 asks for in
// place of aborting the session.
func (r *Repairer) fail(perr *Error) Outcome {
	r.done = true
	return Outcome{
		Attempt:  r.repairs,
		Failure:  perr,
		Feedback: repairMessage(r.tools, perr, true),
		event:    EventToolCallFailed,
	}
}
