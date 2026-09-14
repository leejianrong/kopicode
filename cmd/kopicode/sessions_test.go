package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
)

// This file drives the pure rendering functions behind `kopicode sessions`
// directly, with hand-built [engine.SessionSummary] values, rather than
// through a real directory and os.Getwd — sessionsCmd's own use of
// engine.ListSessions is already held to account end to end in
// internal/engine/sessions_test.go, and going through a real process here
// would only add process-spawning cost for the same coverage
// sessions_integration_test.go already gets more cheaply for the argv-level
// cases (an empty directory, --json's shape on the real binary).

// The column positions sessionFields returns, in sessionColumns's order.
const (
	colID = iota
	colStarted
	colModel
	colTurns
	colStatus
	colMessage
	colCount
)

func TestSessionFieldsOrdinary(t *testing.T) {
	s := engine.SessionSummary{
		ID:           "sess-1",
		StartedAt:    time.Date(2026, 8, 19, 10, 30, 0, 0, time.UTC),
		ModelID:      "qwen/qwen3-coder-next",
		Turns:        3,
		Ended:        true,
		EndReason:    "completed",
		FirstMessage: "fix the failing test",
	}
	got := sessionFields(s)
	want := []string{
		"sess-1", "2026-08-19T10:30:00Z", "qwen/qwen3-coder-next",
		"3", "ended:completed", `"fix the failing test"`,
	}
	if len(got) != colCount {
		t.Fatalf("sessionFields returned %d fields, want %d (one per sessionColumns header)", len(got), colCount)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("field %d = %q, want %q", i, got[i], w)
		}
	}
}

// TestSessionFieldsColumnCountMatchesHeader keeps every row alignable: a data
// row with a different column count than the header is exactly the ragged table
// this change replaced.
func TestSessionFieldsColumnCountMatchesHeader(t *testing.T) {
	if len(sessionColumns) != colCount {
		t.Fatalf("sessionColumns has %d headers, but sessionFields returns %d columns", len(sessionColumns), colCount)
	}
}

// TestSessionFieldsStillRunning holds the "running" placeholder to account:
// a session with no SessionEnded must not read as though it ended cleanly.
func TestSessionFieldsStillRunning(t *testing.T) {
	s := engine.SessionSummary{ID: "sess-2", Ended: false}
	got := sessionFields(s)
	if got[colStatus] != "running" {
		t.Errorf("status = %q, want %q for a session that never ended", got[colStatus], "running")
	}
	if strings.Contains(got[colStatus], "ended:") {
		t.Errorf("status = %q claims an end reason with Ended false", got[colStatus])
	}
}

// TestSessionFieldsForked holds the forked-from annotation to account: it rides
// in the STATUS column so the row keeps a fixed column count.
func TestSessionFieldsForked(t *testing.T) {
	s := engine.SessionSummary{
		ID: "sess-3", Forked: true, ForkedFrom: "sess-1", ForkedTurn: 2,
	}
	got := sessionFields(s)
	if len(got) != colCount {
		t.Fatalf("a forked session returned %d fields, want %d — the fork note must not add a column",
			len(got), colCount)
	}
	if !strings.Contains(got[colStatus], "forked from sess-1@2") {
		t.Errorf("status = %q, want it to name the fork source and turn", got[colStatus])
	}
}

// TestSessionFieldsNoModelOrMessage holds orDash's placeholder to account:
// an empty field reads as one column, not as a run of blank space that
// looks like a misalignment.
func TestSessionFieldsNoModelOrMessage(t *testing.T) {
	s := engine.SessionSummary{ID: "sess-4"}
	got := sessionFields(s)
	if got[colModel] != "-" {
		t.Errorf("model = %q, want the %q placeholder for a missing model id", got[colModel], "-")
	}
	if got[colMessage] != "-" {
		t.Errorf("message = %q, want the %q placeholder for a missing first message", got[colMessage], "-")
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{in: "short", n: 10, want: "short"},
		{in: "exactly ten", n: 11, want: "exactly ten"},
		{in: "this is a long message", n: 7, want: "this is…"},
		{in: "", n: 5, want: ""},
	}
	for _, tc := range tests {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestWriteSessionsTableEmpty(t *testing.T) {
	var out strings.Builder
	writeSessionsTable(&out, "/some/dir", nil)
	if !strings.Contains(out.String(), "no sessions recorded under /some/dir") {
		t.Errorf("writeSessionsTable with no sessions = %q, want it to say so and name the directory",
			out.String())
	}
	// No header when there is nothing to head: an empty listing is a sentence,
	// not a table with a lone header row above no rows.
	if strings.Contains(out.String(), sessionColumns[0]) {
		t.Errorf("writeSessionsTable with no sessions printed a header row: %q", out.String())
	}
}

// TestWriteSessionsTableHasHeader is KAN-1365's fix: the plain output labels its
// columns. The first non-empty line is the header, and each session renders
// below it.
func TestWriteSessionsTableHasHeader(t *testing.T) {
	sessions := []engine.SessionSummary{
		{ID: "sess-1", StartedAt: time.Date(2026, 8, 19, 10, 30, 0, 0, time.UTC), ModelID: "m", Turns: 1},
	}
	var out strings.Builder
	writeSessionsTable(&out, "/some/dir", sessions)

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != len(sessions)+1 {
		t.Fatalf("got %d lines, want a header plus %d session row(s):\n%s", len(lines), len(sessions), out.String())
	}
	for _, col := range sessionColumns {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header row %q is missing the %q column", lines[0], col)
		}
	}
	if !strings.Contains(lines[1], "sess-1") {
		t.Errorf("session row %q does not name the session", lines[1])
	}
}

// TestWriteSessionsJSONRoundTrips holds the --json path to the same
// promise print.go's own emitter carries: one decodable JSON object per
// line, in the order given.
func TestWriteSessionsJSONRoundTrips(t *testing.T) {
	sessions := []engine.SessionSummary{
		{ID: "a", Turns: 1},
		{ID: "b", Turns: 2, Forked: true, ForkedFrom: "a", ForkedTurn: 1},
	}
	var out, errOut strings.Builder
	code := writeSessionsJSON(&out, &errOut, sessions)
	if code != exitSuccess {
		t.Fatalf("writeSessionsJSON = %d, want %d. stderr: %s", code, exitSuccess, errOut.String())
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(sessions) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(sessions), out.String())
	}
	for i, line := range lines {
		var got engine.SessionSummary
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d did not decode as a SessionSummary: %v\n%s", i, err, line)
		}
		if got.ID != sessions[i].ID || got.Turns != sessions[i].Turns || got.Forked != sessions[i].Forked {
			t.Errorf("line %d decoded to %+v, want it to match %+v", i, got, sessions[i])
		}
	}
}
