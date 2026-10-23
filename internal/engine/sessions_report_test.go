package engine_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

// TestReportSessionDigestsTheRubricsFieldsFromTheJournalAlone is KOP-214: each
// number the driven-run notes copied out of a journal by hand comes out of the
// record, and a seq in the report is the event's own. The journal is written
// event by event, so every expectation below is something a reader can find in
// the file.
func TestReportSessionDigestsTheRubricsFieldsFromTheJournalAlone(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	jrn, err := journal.Open(dir, "rep", journal.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.0025
	type step struct {
		turn int
		p    journal.Payload
	}
	steps := []step{
		{0, journal.SessionStarted{ModelID: "some/model", Build: journal.BuildInfo{Version: "v9.9.9"}}},
		{1, journal.UserMessage{Text: journal.InlineText("do it")}},
		{1, journal.ProviderRequest{ModelID: "some/model"}},
		{1, journal.ProviderRetried{}},
		{1, journal.ProviderResponse{Tokens: journal.TokenCounts{Total: 100}, CostUSD: &cost}},
		{1, journal.ToolCallParsed{Tool: "edit_file", Route: "native", ArgEncoding: "json_string"}},
		{1, journal.EditRejected{Path: "a.py", Mode: "anchored", Reason: "anchor_drift"}},
		{1, journal.ToolCallParsed{Tool: "edit_file", Route: "native", ArgEncoding: "json_string"}},
		{1, journal.EditApplied{Path: "b.py", Mode: "anchored"}},
		{1, journal.EditRejected{Path: "c.py", Mode: "anchored", Reason: "ambiguous"}},
		{1, journal.EditApplied{Path: "c.py", Mode: "anchored"}},
		{1, journal.EditRejected{Path: "a.py", Mode: "anchored", Reason: "anchor_drift"}},
		{1, journal.SyntaxGateRun{Path: "b.py", Ran: true, ExitCode: 1}},
		{1, journal.SyntaxGateRun{Path: "c.py", Ran: true, ExitCode: 0}},
		{1, journal.PermissionRequested{Kind: "run_shell", Tool: "run_shell"}},
		{1, journal.PermissionDecided{Decision: "deny", Source: "auto", Reason: "sudo"}},
		{1, journal.PermissionRequested{Kind: "run_shell", Tool: "run_shell"}},
		{1, journal.PermissionDecided{Decision: "allow", Source: "auto"}},
		{1, journal.VerificationRun{ExitCode: -1, Skip: "no_command"}},
		{2, journal.ProviderRequest{ModelID: "some/model"}},
		{2, journal.ProviderResponse{Tokens: journal.TokenCounts{Total: 50}}},
		{2, journal.VerificationRun{Command: []string{"make", "test"}, ExitCode: 2}},
		{2, journal.VerificationRun{Command: []string{"make", "test"}, ExitCode: 0}},
		{2, journal.AssistantMessage{Text: journal.InlineText("all done")}},
		{2, journal.SessionEnded{Reason: "completed", ExitCode: 0}},
	}
	seqs := make([]uint64, len(steps))
	for i, s := range steps {
		ev, err := jrn.Append(ctx, s.turn, s.p)
		if err != nil {
			t.Fatalf("step %d (%T): %v", i, s.p, err)
		}
		seqs[i] = ev.Seq
	}
	if err := jrn.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := engine.ReportSession(ctx, dir, "rep")
	if err != nil {
		t.Fatal(err)
	}

	if got.ModelID != "some/model" || got.Version != "v9.9.9" {
		t.Errorf("identity = %q %q", got.ModelID, got.Version)
	}
	if got.Ended == nil || got.Ended.Reason != "completed" || got.Ended.Seq != seqs[len(seqs)-1] {
		t.Errorf("Ended = %+v, want completed at seq %d", got.Ended, seqs[len(seqs)-1])
	}
	if got.Turns != 2 || got.Requests != 2 || got.Retries != 1 || got.Tokens != 150 || got.CostUSD != cost {
		t.Errorf("turns/requests/retries/tokens/cost = %d/%d/%d/%d/%v, want 2/2/1/150/%v",
			got.Turns, got.Requests, got.Retries, got.Tokens, got.CostUSD, cost)
	}
	v := got.Verification
	if v.Runs != 3 || v.Skipped != 1 || v.Failed != 1 || v.Last == nil || v.Last.Seq != seqs[22] || v.Last.ExitCode != 0 {
		t.Errorf("verification = %+v last %+v, want 3 runs, 1 skipped, 1 failed, last exit 0 at seq %d", v, v.Last, seqs[22])
	}
	if len(got.Edits.Applied) != 2 || len(got.Edits.Rejected) != 3 {
		t.Errorf("edits = %+v, want 2 applied and 3 rejected", got.Edits)
	}
	// a.py was rejected twice and never edited; c.py was rejected and then applied.
	if want := []uint64{seqs[6], seqs[11]}; !reflect.DeepEqual(got.Edits.Unrecovered, want) {
		t.Errorf("unrecovered = %v, want %v (the two rejections on a.py)", got.Edits.Unrecovered, want)
	}
	if got.Tools["edit_file"] != 2 || got.Routes["native/json_string"] != 2 {
		t.Errorf("tools %v routes %v", got.Tools, got.Routes)
	}
	if got.SyntaxGate.Runs != 2 || len(got.SyntaxGate.Failed) != 1 || got.SyntaxGate.Failed[0].Path != "b.py" {
		t.Errorf("syntax gate = %+v", got.SyntaxGate)
	}
	p := got.Permissions
	if p.Requested != 2 || p.Decisions["deny/auto"] != 1 || p.Decisions["allow/auto"] != 1 || len(p.Denied) != 1 || p.Denied[0].Reason != "sudo" {
		t.Errorf("permissions = %+v", p)
	}
	if got.Final == nil || got.Final.Text != "all done" || got.Final.Seq != seqs[23] {
		t.Errorf("final = %+v", got.Final)
	}

	// The whole report is JSON a script can read back.
	raw, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(raw), `"unrecovered"`) {
		t.Errorf("report does not marshal as expected: %v %s", err, raw)
	}
}

func TestReportSessionOfAnUnknownIdIsAnError(t *testing.T) {
	if _, err := engine.ReportSession(context.Background(), t.TempDir(), "nope"); err == nil {
		t.Error("an id with no record reported as if it existed")
	}
}

// A record with no SessionEnded is reported as one, not as a failure to read.
func TestReportSessionOfASessionThatNeverEnded(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	jrn, err := journal.Open(dir, "open", journal.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jrn.Append(ctx, 0, journal.SessionStarted{ModelID: "m"}); err != nil {
		t.Fatal(err)
	}
	_ = jrn.Close()
	got, err := engine.ReportSession(ctx, dir, "open")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ended != nil || got.Turns != 0 {
		t.Errorf("report = %+v, want no end and no turns", got)
	}
}
