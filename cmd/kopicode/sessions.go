package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/leejianrong/kopicode/internal/engine"
)

// `kopicode sessions` (KAN-941) lists the sessions recorded under the current
// directory's .kopicode/sessions, so a human — or a script picking up an
// interrupted headless run, see print.go's own note on `run --print
// --resume` — has somewhere to find a session id short of reading the
// directory by hand. main.go's own --resume flag doc comment named this gap
// before this file existed to close it.
//
// It is presentation over [engine.ListSessions] and nothing else: every
// field printed below is one that function already computed, so there is no
// second place this command's idea of "what a session is" could drift from
// the engine's. It needs no arm resolved and no session opened — listing is
// read-only and does not care which model a caller is about to run, which is
// why this command's flag set is the smallest of the three.

// sessionsCmd is `kopicode sessions`.
func sessionsCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kopicode sessions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// --json is the machine-readable form: one object per line, matching
	// `run --print`'s own NDJSON convention (print.go) rather than a second
	// shape a script would have to learn. It exists for the same reason
	// print.go's schema does — a script picking up an interrupted `run
	// --print` invocation needs a session id it can parse, not a column a
	// human eyeballs.
	asJSON := fs.Bool("json", false, "emit one JSON object per session, for a script")
	if err := fs.Parse(args); err != nil {
		// flag has already printed the error and the usage.
		return exitUsage
	}

	dir, err := os.Getwd()
	if err != nil {
		say(stderr, "kopicode: %v\n", err)
		return exitHarness
	}

	sessions, err := engine.ListSessions(context.Background(), dir)
	if err != nil {
		say(stderr, "kopicode: %v\n", err)
		return exitHarness
	}

	if *asJSON {
		return writeSessionsJSON(stdout, stderr, sessions)
	}
	writeSessionsTable(stdout, dir, sessions)
	return exitSuccess
}

// writeSessionsJSON emits one line of JSON per session, escaping matching
// print.go's newEmitter: HTML escaping off, because a first message
// containing "<" or "&" should reach a script's parser as the bytes the
// journal holds, not re-encoded into \uXXXX.
func writeSessionsJSON(stdout, stderr io.Writer, sessions []engine.SessionSummary) int {
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	for _, s := range sessions {
		if err := enc.Encode(s); err != nil {
			say(stderr, "kopicode: writing the session list: %v\n", err)
			return exitHarness
		}
	}
	return exitSuccess
}

// writeSessionsTable renders the human-facing form: newest last, so the
// session a person most likely wants to resume or fork from is the one
// their eye lands on at the bottom of the terminal, the same reason a shell
// history or a log tails that way.
func writeSessionsTable(stdout io.Writer, dir string, sessions []engine.SessionSummary) {
	if len(sessions) == 0 {
		say(stdout, "no sessions recorded under %s\n", dir)
		return
	}
	for _, s := range sessions {
		say(stdout, "%s\n", formatSession(s))
	}
}

// formatSession renders one session as one line: id, when, model, how far
// it got, and (space permitting) what the human first asked for — enough to
// tell two sessions apart without opening either journal.
func formatSession(s engine.SessionSummary) string {
	status := "running"
	if s.Ended {
		status = "ended:" + s.EndReason
	}
	fields := []string{
		s.ID,
		s.StartedAt.UTC().Format("2006-01-02T15:04:05Z"),
		orDash(s.ModelID),
		"turns:" + strconv.Itoa(s.Turns),
		status,
	}
	if s.Forked {
		fields = append(fields, fmt.Sprintf("forked from %s@%d", s.ForkedFrom, s.ForkedTurn))
	}
	line := strings.Join(fields, "  ")
	if s.FirstMessage != "" {
		line += "  " + strconv.Quote(truncate(s.FirstMessage, 60))
	}
	return line
}

// orDash renders an empty string as a placeholder a column-scanning eye
// still parses as "one field", rather than a run of two spaces that reads
// as a misalignment.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// truncate shortens s to at most n runes, marking the cut with an ellipsis
// so a truncated first message is never mistaken for a short one.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
