package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

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
	asJSON := fs.Bool("json", false, "emit one JSON object per session (or the one report), for a script")
	// --report <id> is KOP-214: the rubric's mechanical fields for one session,
	// derived from its journal and nothing else, so a run note does not copy
	// them out by hand. It judges nothing; every seq in it is an event's own.
	report := fs.String("report", "", "print a digest of one session's record: how it ended, turns, tokens, cost, "+
		"verification, edits and rejections, tool-call routes, syntax gate, permissions and the final message")
	if err := fs.Parse(args); err != nil {
		// flag has already printed the error and the usage. A help request is not
		// an error: -h/--help exits 0, everything else is a usage error.
		if errors.Is(err, flag.ErrHelp) {
			return exitSuccess
		}
		return exitUsage
	}

	dir, err := os.Getwd()
	if err != nil {
		say(stderr, "kopicode: %v\n", err)
		return exitHarness
	}

	if *report != "" {
		return reportCmd(stdout, stderr, dir, *report, *asJSON)
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

// sessionColumns names the fields [sessionFields] returns, in order. It is the
// header row and the single source of truth for how many columns a row has, so
// a column added to one without the other is a compile-time-obvious mismatch
// rather than a silently ragged table.
var sessionColumns = []string{"ID", "STARTED (UTC)", "MODEL", "TURNS", "STATUS", "FIRST MESSAGE"}

// writeSessionsTable renders the human-facing form: a labelled, column-aligned
// table, newest last — so the session a person most likely wants to resume or
// fork from is the one their eye lands on at the bottom of the terminal, the
// same reason a shell history or a log tails that way. Alignment is
// text/tabwriter's job (stdlib, no dependency); every row carries the same
// fixed set of columns [sessionColumns] names, which is why the optional
// "forked from" annotation folds into STATUS rather than becoming a ragged
// extra field.
func writeSessionsTable(stdout io.Writer, dir string, sessions []engine.SessionSummary) {
	if len(sessions) == 0 {
		say(stdout, "no sessions recorded under %s\n", dir)
		return
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	// tabwriter buffers every write and only touches stdout on Flush, so a write
	// error here is impossible and Flush is the one place a lost-stdout error
	// could surface — the same case say() already swallows for this command, so
	// there is nothing new to report.
	_, _ = fmt.Fprintln(tw, strings.Join(sessionColumns, "\t"))
	for _, s := range sessions {
		_, _ = fmt.Fprintln(tw, strings.Join(sessionFields(s), "\t"))
	}
	_ = tw.Flush()
}

// sessionFields is one session's columns in [sessionColumns]'s order: id, when,
// model, how far it got, its status, and (truncated) what the human first asked
// for — enough to tell two sessions apart without opening either journal. It is
// the one place a row's contents are decided, shared by the table writer and its
// tests so the two cannot drift.
func sessionFields(s engine.SessionSummary) []string {
	status := "running"
	if s.Ended {
		status = "ended:" + s.EndReason
	}
	// The fork provenance rides in STATUS so every row keeps the same column
	// count; it is still the whole of "forked from <id>@<turn>" a reader needs
	// to find the source session.
	if s.Forked {
		status += fmt.Sprintf(" (forked from %s@%d)", s.ForkedFrom, s.ForkedTurn)
	}
	message := "-"
	if s.FirstMessage != "" {
		message = strconv.Quote(truncate(s.FirstMessage, 60))
	}
	return []string{
		s.ID,
		s.StartedAt.UTC().Format("2006-01-02T15:04:05Z"),
		orDash(s.ModelID),
		strconv.Itoa(s.Turns),
		status,
		message,
	}
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

// reportCmd is `kopicode sessions -report <id>`: [engine.ReportSession] as text,
// or as one JSON object with -json.
func reportCmd(stdout, stderr io.Writer, dir, id string, asJSON bool) int {
	r, err := engine.ReportSession(context.Background(), dir, id)
	if err != nil {
		say(stderr, "kopicode: %v\n", err)
		return exitHarness
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(r); err != nil {
			say(stderr, "kopicode: writing the report: %v\n", err)
			return exitHarness
		}
		return exitSuccess
	}
	writeReport(stdout, r)
	return exitSuccess
}

// writeReport renders a report for a person writing a run note: one fact per
// line, each with the seq it came from.
func writeReport(w io.Writer, r engine.SessionReport) {
	say(w, "session   %s   model %s   kopicode %s\n", r.ID, orDash(r.ModelID), orDash(r.Version))
	if r.Ended != nil {
		say(w, "ended     %s, exit %d (seq %d)\n", r.Ended.Reason, r.Ended.ExitCode, r.Ended.Seq)
	} else {
		say(w, "ended     no SessionEnded in the record (still running, or the process was killed)\n")
	}
	say(w, "usage     %d turns, %d requests (%d retried), %d tokens, $%.4f\n", r.Turns, r.Requests, r.Retries, r.Tokens, r.CostUSD)

	v := r.Verification
	switch {
	case v.Runs == 0:
		say(w, "verify    no VerificationRun in the record\n")
	case v.Last.Skip != "":
		say(w, "verify    %d runs, %d skipped; the last (seq %d) was skipped: %s, so nothing was checked\n", v.Runs, v.Skipped, v.Last.Seq, v.Last.Skip)
	default:
		say(w, "verify    %d runs, %d skipped, %d failed; last exit %d (seq %d) %s\n", v.Runs, v.Skipped, v.Failed, v.Last.ExitCode, v.Last.Seq, strings.Join(v.Last.Command, " "))
	}

	say(w, "edits     %d applied, %d rejected\n", len(r.Edits.Applied), len(r.Edits.Rejected))
	for _, e := range r.Edits.Rejected {
		say(w, "  rejected  seq %d  %s  %s\n", e.Seq, e.Path, e.Reason)
	}
	if len(r.Edits.Unrecovered) > 0 {
		say(w, "  never followed by an applied edit to the same path: seq %s\n", joinSeqs(r.Edits.Unrecovered))
	}
	say(w, "calls     %s\n", countList(r.Tools))
	say(w, "routes    %s\n", countList(r.Routes))
	say(w, "syntax    %d runs, %d failed\n", r.SyntaxGate.Runs, len(r.SyntaxGate.Failed))
	for _, g := range r.SyntaxGate.Failed {
		say(w, "  failed  seq %d  %s  exit %d\n", g.Seq, g.Path, g.ExitCode)
	}
	say(w, "consent   %d requested; %s\n", r.Permissions.Requested, countList(r.Permissions.Decisions))
	for _, d := range r.Permissions.Denied {
		say(w, "  denied  seq %d  %s  %s\n", d.Seq, d.Source, d.Reason)
	}
	switch {
	case r.Final == nil:
		say(w, "final     no assistant message in the record\n")
	case r.Final.Spilled:
		say(w, "final     seq %d (over the inline limit; it is in a blob)\n", r.Final.Seq)
	default:
		say(w, "final     seq %d\n%s\n", r.Final.Seq, r.Final.Text)
	}
}

func countList(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(m))
	for _, k := range engine.SortedCounts(m) {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func joinSeqs(seqs []uint64) string {
	parts := make([]string, len(seqs))
	for i, s := range seqs {
		parts[i] = strconv.FormatUint(s, 10)
	}
	return strings.Join(parts, ", ")
}
