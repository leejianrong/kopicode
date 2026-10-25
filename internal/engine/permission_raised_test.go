package engine_test

import (
	"errors"
	"testing"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/tools"
)

func shellScript() []scriptedReply {
	return []scriptedReply{
		{calls: []wireCall{nativeCall("call-shell", tools.ToolRunShell, `{"command":"echo hello"}`)},
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		{text: "Done.", usage: wireUsage{Prompt: 20, Completion: 4, Total: 24}},
	}
}

// KOP-228: while the policy is waiting on a person, the journal already holds the
// request and not the answer, so a reader can tell a dialog is open.
func TestAPermissionRequestIsOnTheJournalWhileItIsBeingAsked(t *testing.T) {
	h := scriptHarness(t, shellScript(), oneAttemptPerTurn(2), nil, withMaxTurns(2))
	var openRequests, answers int
	h.policy.during = func(permission.Request) {
		evs := h.events()
		openRequests = len(payloadsOf[journal.PermissionRequested](t, evs))
		answers = len(payloadsOf[journal.PermissionDecided](t, evs))
	}
	if _, err := h.eng.Run(t.Context(), "run it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if openRequests != 1 || answers != 0 {
		t.Errorf("while asking the journal held %d requests and %d answers, want 1 and 0", openRequests, answers)
	}
	evs := h.events()
	req := sole[journal.PermissionRequested](t, evs)
	dec := sole[journal.PermissionDecided](t, evs)
	if req.RequestID != dec.RequestID {
		t.Errorf("request %q and decision %q do not pair", req.RequestID, dec.RequestID)
	}
	// Exactly one of each, with the request first.
	var order []journal.Type
	for _, ty := range h.types() {
		if ty == journal.TypePermissionRequested || ty == journal.TypePermissionDecided {
			order = append(order, ty)
		}
	}
	if len(order) != 2 || order[0] != journal.TypePermissionRequested {
		t.Errorf("order = %v, want the request then the decision", order)
	}
}

// A policy that cannot answer must not leave the request open on the record.
func TestARaisedRequestThatGetsNoAnswerIsClosedAsADenial(t *testing.T) {
	h := scriptHarness(t, shellScript(), oneAttemptPerTurn(2), nil, withMaxTurns(2))
	h.policy.err = errors.New("the dialog went away")
	if _, err := h.eng.Run(t.Context(), "run it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs := h.events()
	req := sole[journal.PermissionRequested](t, evs)
	dec := sole[journal.PermissionDecided](t, evs)
	if dec.RequestID != req.RequestID || dec.Decision != permission.VerdictDeny.String() {
		t.Errorf("decision = %+v, want a denial of request %q", dec, req.RequestID)
	}
	if dec.Reason == "" {
		t.Error("the closing denial gives no reason")
	}
}
