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

func TestFormatSessionOrdinary(t *testing.T) {
	s := engine.SessionSummary{
		ID:           "sess-1",
		StartedAt:    time.Date(2026, 8, 19, 10, 30, 0, 0, time.UTC),
		ModelID:      "qwen/qwen3-coder-next",
		Turns:        3,
		Ended:        true,
		EndReason:    "completed",
		FirstMessage: "fix the failing test",
	}
	got := formatSession(s)
	for _, want := range []string{
		"sess-1", "2026-08-19T10:30:00Z", "qwen/qwen3-coder-next",
		"turns:3", "ended:completed", `"fix the failing test"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatSession(%+v) = %q, want it to contain %q", s, got, want)
		}
	}
}

// TestFormatSessionStillRunning holds the "running" placeholder to account:
// a session with no SessionEnded must not read as though it ended cleanly.
func TestFormatSessionStillRunning(t *testing.T) {
	s := engine.SessionSummary{ID: "sess-2", Ended: false}
	got := formatSession(s)
	if !strings.Contains(got, "running") {
		t.Errorf("formatSession(%+v) = %q, want it to say the session is still running", s, got)
	}
	if strings.Contains(got, "ended:") {
		t.Errorf("formatSession(%+v) = %q, claims an end reason with Ended false", s, got)
	}
}

// TestFormatSessionForked holds the forked-from annotation to account.
func TestFormatSessionForked(t *testing.T) {
	s := engine.SessionSummary{
		ID: "sess-3", Forked: true, ForkedFrom: "sess-1", ForkedTurn: 2,
	}
	got := formatSession(s)
	if !strings.Contains(got, "forked from sess-1@2") {
		t.Errorf("formatSession(%+v) = %q, want it to name the fork source and turn", s, got)
	}
}

// TestFormatSessionNoModelOrMessage holds orDash's placeholder to account:
// an empty field reads as one column, not as a run of blank space that
// looks like a misalignment.
func TestFormatSessionNoModelOrMessage(t *testing.T) {
	s := engine.SessionSummary{ID: "sess-4"}
	got := formatSession(s)
	if !strings.Contains(got, "-") {
		t.Errorf("formatSession(%+v) = %q, want a placeholder for the missing model id", s, got)
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
