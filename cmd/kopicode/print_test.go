package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// This file is `kopicode run --print`, driven end to end over real HTTP against
// a server the test stands up — the same seam session_test.go uses and for the
// same reason: the thing under test is the wiring, and a fake substituted at the
// provider interface would leave the client, the SSE reader and the request
// shape uncovered exactly where this surface depends on them.
//
// **Every exit code below is caused, not asserted directly.** The card's "done
// when" is that each of the five is produced by a test that creates the matching
// condition, so each case here sets up the condition — a model id that does not
// exist, a verification command that fails, a provider that refuses, a reply
// that never stops calling tools — and reads the code off the run. exit_test.go
// is where the *table* is held to engine.Stop.ExitCode; nothing here calls it.

// --- provider fixtures ------------------------------------------------------

// sseToolCall is one streamed reply that calls a tool natively.
//
// The delta shape is OpenAI's, which is what internal/provider/fixture's own
// note says OpenRouter documents nowhere and follows for the same reason. Wire
// fidelity is held there and in internal/provider's tests; what this file needs
// is a well-formed stream the loop will act on.
func sseToolCall(callID, tool, args string) string {
	var b strings.Builder
	frame := func(format string, a ...any) {
		fmt.Fprintf(&b, "data: "+format+"\n\n", a...)
	}
	frame(`{"id":"gen-1","object":"chat.completion.chunk","model":"qwen/qwen3-coder-next",`+
		`"provider":"Parasail","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":`+
		`[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},`+
		`"finish_reason":null}]}`, callID, tool, args)
	frame(`{"id":"gen-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":9,` +
		`"total_tokens":21}}`)
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// scriptedProvider serves one reply per request, repeating the last once the
// script runs out.
//
// Repeating rather than failing is what the turn-cap case needs: a model that
// keeps calling tools is the condition, and writing the same reply out twenty
// times would say nothing the repeat does not.
func scriptedProvider(t *testing.T, bodies ...string) *httptest.Server {
	t.Helper()

	var mu sync.Mutex
	var n int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		body := bodies[min(n, len(bodies)-1)]
		n++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// refusingProvider answers every request with status and an error body.
func refusingProvider(t *testing.T, status int, message string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":%q}}`, status, message)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- driving the surface ----------------------------------------------------

// headlessIn runs one headless exchange in dir and returns stdout, stderr and
// the exit code.
func headlessIn(t *testing.T, dir, prompt string, opts engine.Options) (string, string, int) {
	t.Helper()

	selection, err := engine.ResolveSelection(dir, engine.SelectionOverrides{})
	if err != nil {
		t.Fatalf("resolving the arm in %s: %v", dir, err)
	}
	opts.Dir = dir
	opts.Selection = selection

	var out, errOut strings.Builder
	code := headless(prompt, &out, &errOut, opts)
	return out.String(), errOut.String(), code
}

// runHeadless is headlessIn in a fresh directory.
func runHeadless(t *testing.T, prompt string, opts engine.Options) (string, string, int, string) {
	t.Helper()

	dir := t.TempDir()
	out, errOut, code := headlessIn(t, dir, prompt, opts)
	return out, errOut, code, dir
}

// --- reading the stream -----------------------------------------------------

// line is one decoded record. It is a map rather than the record struct on
// purpose: what is under test is the JSON a consumer reads, and decoding into
// the type that produced it would agree with itself about a field that was
// renamed.
type line map[string]any

func stream(t *testing.T, stdout string) []line {
	t.Helper()

	if strings.TrimSpace(stdout) == "" {
		return nil
	}
	var out []line
	for i, raw := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("line %d is not JSON: %v\n%s", i+1, err, raw)
		}
		out = append(out, l)
	}
	return out
}

// kinds is every line's kind, in order.
func kinds(ls []line) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		kind, _ := l["kind"].(string)
		out = append(out, kind)
	}
	return out
}

// last returns the final line, which for any session that opened is
// session_ended.
func last(t *testing.T, ls []line) line {
	t.Helper()

	if len(ls) == 0 {
		t.Fatal("the stream is empty")
	}
	return ls[len(ls)-1]
}

func number(t *testing.T, l line, field string) int {
	t.Helper()

	v, ok := l[field]
	if !ok {
		t.Fatalf("%v has no %s", l, field)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s is %T, want a number: %v", field, v, l)
	}
	return int(f)
}

// --- exit 0: success --------------------------------------------------------

// TestACompletedTaskExitsZeroAndTheStreamSaysSo.
//
// The condition: the model answers in prose and asks for no tool, which is the
// loop's clean stop.
func TestACompletedTaskExitsZeroAndTheStreamSaysSo(t *testing.T) {
	srv := scriptedProvider(t, sseBody("the fix is in"))
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")

	stdout, stderr, code, dir := runHeadless(t, "why does it fail?", engine.Options{
		ProviderBaseURL: srv.URL,
	})

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d. stderr:\n%s\nstdout:\n%s", code, exitSuccess, stderr, stdout)
	}

	ls := stream(t, stdout)
	head := ls[0]
	if head["kind"] != kindStream {
		t.Errorf("line 1 is %v, want the stream header", head)
	}
	if got := number(t, head, "schema"); got != schemaVersion {
		t.Errorf("schema = %d, want %d", got, schemaVersion)
	}
	// The header names where the record is, and the record is there.
	recordPath, _ := head["record"].(string)
	if recordPath == "" {
		t.Fatalf("the header names no record: %v", head)
	}
	if _, err := os.Stat(filepath.Join(recordPath, "events.jsonl")); err != nil {
		t.Errorf("the header points at %s, which holds no journal: %v", recordPath, err)
	}

	got := kinds(ls)
	for _, want := range []string{"session_started", "user_message", "assistant_message", "session_ended"} {
		if !slices.Contains(got, want) {
			t.Errorf("the stream has no %s line: %v", want, got)
		}
	}

	end := last(t, ls)
	if end["kind"] != "session_ended" {
		t.Errorf("the last line is %v, want session_ended", end)
	}
	if end["reason"] != "completed" {
		t.Errorf("session_ended reason = %v, want completed", end["reason"])
	}
	if got := number(t, end, "exit_code"); got != code {
		t.Errorf("the record says exit %d and the process returned %d; they come from one "+
			"engine.Stop and must not disagree", got, code)
	}

	if !strings.Contains(stdout, "the fix is in") {
		t.Errorf("the reply the record holds is not on the stream:\n%s", stdout)
	}
	if events := findJournal(t, dir); !strings.Contains(events, "the fix is in") {
		t.Error("the journal does not hold the message the stream printed")
	}
}

// TestEverythingOnTheStreamIsInTheRecord.
//
// ADR-0002 decision 2 made checkable from outside: every line but the header
// carries a journal sequence number, the numbers are the record's own and appear
// in order, and there are exactly as many of them as the journal holds events.
// A surface accumulating its own account would fail all three.
//
// It also covers the deltas. A streamed fragment reaches the engine's observer
// with Seq zero — that is how engine.Event says "this is not in the record" —
// and this reply arrives as two of them, so a `seq` of 0 anywhere is a delta
// that leaked onto a journal-derived stream.
func TestEverythingOnTheStreamIsInTheRecord(t *testing.T) {
	srv := scriptedProvider(t, sseBody("two words"))
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")

	stdout, stderr, code, dir := runHeadless(t, "hello", engine.Options{ProviderBaseURL: srv.URL})
	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d. stderr:\n%s", code, exitSuccess, stderr)
	}

	ls := stream(t, stdout)
	if len(ls) < 2 {
		t.Fatalf("the stream has %d lines, so this asserted nothing:\n%s", len(ls), stdout)
	}

	prev := 0
	for i, l := range ls[1:] {
		seq, ok := l["seq"]
		if !ok {
			t.Fatalf("event line %d has no seq, so it is not derived from the record: %v", i+1, l)
		}
		n := int(seq.(float64))
		if n <= prev {
			t.Errorf("event line %d has seq %d after %d; the record's order is the stream's order",
				i+1, n, prev)
		}
		prev = n
	}

	// And the count matches: nothing the journal holds was dropped, and nothing
	// the stream carries was invented.
	events := strings.Count(strings.TrimSuffix(findJournal(t, dir), "\n"), "\n") + 1
	if got := len(ls) - 1; got != events {
		t.Errorf("the stream carries %d events and the journal holds %d", got, events)
	}
}

// --- exit 1: the task was not completed -------------------------------------

// TestAFailedVerificationIsTaskNotCompleted.
//
// The condition: the model edits a file and then declares itself finished over a
// tree the project's own verification command rejects. That is exactly the
// answer forced verification exists to withhold (docs/SLICE-1.md §5), and the
// engine settles it as StopVerificationFailed — "the task was not completed",
// not "the harness broke", because the harness ran the suite, showed the model
// the failure, and the model stopped anyway.
func TestAFailedVerificationIsTaskNotCompleted(t *testing.T) {
	dir := t.TempDir()
	// The repository names its own verification command, so this does not depend
	// on what discovery would infer from an empty directory.
	writeVerifyConfig(t, dir, `["sh", "-c", "echo the suite is red; exit 1"]`)

	srv := scriptedProvider(t,
		sseToolCall("call-1", "write_file", `{"path":"fix.txt","content":"attempted\n"}`),
		sseBody("all done"),
	)
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")

	stdout, stderr, code := headlessIn(t, dir, "make it pass", engine.Options{ProviderBaseURL: srv.URL})

	if code != exitNotCompleted {
		t.Fatalf("exit code = %d, want %d. stderr:\n%s\nstdout:\n%s",
			code, exitNotCompleted, stderr, stdout)
	}

	ls := stream(t, stdout)
	if !slices.Contains(kinds(ls), "verification") {
		t.Errorf("the stream carries no verification line: %v", kinds(ls))
	}

	end := last(t, ls)
	if end["reason"] != "verification_failed" {
		t.Errorf("session_ended reason = %v, want verification_failed", end["reason"])
	}
	if got := number(t, end, "exit_code"); got != code {
		t.Errorf("the record says exit %d and the process returned %d", got, code)
	}
	// The suite's own output is on the stream, because it is in the record: the
	// evidence for the refusal, not just the verdict.
	if !strings.Contains(stdout, "the suite is red") {
		t.Errorf("the verification output is not on the stream:\n%s", stdout)
	}
}

// --- exit 2: usage ----------------------------------------------------------

// TestUsageErrorsAreRefusedBeforeAnythingRuns.
//
// Four conditions, one code. Each is a fact about the command line alone, so
// each is settled before the working directory is read, before the arm is
// resolved and before a journal exists — ADR-0007 decision 4's ordering, which
// is what makes exit 2 mean "nothing was opened, locked or written".
//
// These go through run() rather than through headless(), because the refusal
// happens in the flag parsing and a test that skipped it would be asserting
// about a path users cannot reach.
func TestUsageErrorsAreRefusedBeforeAnythingRuns(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		mention string
	}{
		{"an unknown model", []string{"run", "--print", "--model", "qwen/not-a-model", "fix it"},
			"qwen/not-a-model"},
		{"an unknown flag", []string{"run", "--print", "--summarise", "fix it"}, "summarise"},
		{"no --print", []string{"run", "fix it"}, "--print"},
		{"no task", []string{"run", "--print"}, "kopicode run --print"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder

			if code := run(tc.args, &stdout, &stderr); code != exitUsage {
				t.Errorf("exit code = %d, want %d. stderr:\n%s", code, exitUsage, stderr.String())
			}
			if stdout.String() != "" {
				t.Errorf("a refused invocation wrote to stdout, which --print owns:\n%s", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.mention) {
				t.Errorf("the refusal does not mention %q:\n%s", tc.mention, stderr.String())
			}
		})
	}
}

// --- exit 3: provider error -------------------------------------------------

// TestAProviderRefusalExitsThree.
//
// The condition: the provider answers with a 4xx that is not 429 — here 402,
// which is what OpenRouter returns when an account is out of credit. The client
// does not retry that and must not: it describes the request rather than the
// moment, so it is reported at once. What reaches the loop is a call that
// produced no usable reply, which is StopProviderError.
//
// The other route to the same code — a 5xx that survives every retry — is
// TestRetriesExhaustedExitThree in print_retry_integration_test.go, which is
// tagged out of the fast suite because it waits out the real backoff.
func TestAProviderRefusalExitsThree(t *testing.T) {
	srv := refusingProvider(t, http.StatusPaymentRequired, "insufficient credits")
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")

	stdout, stderr, code, _ := runHeadless(t, "fix it", engine.Options{ProviderBaseURL: srv.URL})

	if code != exitProvider {
		t.Fatalf("exit code = %d, want %d. stderr:\n%s\nstdout:\n%s",
			code, exitProvider, stderr, stdout)
	}

	// The session opened, so the record exists and holds its own ending.
	end := last(t, stream(t, stdout))
	if end["kind"] != "session_ended" || end["reason"] != "error" {
		t.Errorf("the last line is %v, want a session_ended with reason error", end)
	}
	if got := number(t, end, "exit_code"); got != exitProvider {
		t.Errorf("the record says exit %d, want %d", got, exitProvider)
	}
}

// --- exit 4: harness error --------------------------------------------------

// TestTheTurnCapIsAHarnessError.
//
// The condition: the model never stops calling tools, so the exchange runs into
// Selection.Config.MaxTurns. docs/SLICE-1.md §9 names the turn cap a harness
// failure outright — a task the loop could not finish inside its own bound is
// the harness's bound, not the model's answer — so it is exit 4 and not exit 1.
func TestTheTurnCapIsAHarnessError(t *testing.T) {
	srv := scriptedProvider(t, sseToolCall("call-1", "list_dir", `{}`))
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")

	stdout, stderr, code, _ := runHeadless(t, "look around forever", engine.Options{
		ProviderBaseURL: srv.URL,
	})

	if code != exitHarness {
		t.Fatalf("exit code = %d, want %d. stderr:\n%s", code, exitHarness, stderr)
	}

	end := last(t, stream(t, stdout))
	if end["reason"] != "max_turns" {
		t.Errorf("session_ended reason = %v, want max_turns", end["reason"])
	}
	if got := number(t, end, "exit_code"); got != code {
		t.Errorf("the record says exit %d and the process returned %d", got, code)
	}
}

// TestAMissingCredentialIsAHarnessErrorAndWritesNothing.
//
// The other exit-4 condition, and the one that produces no stream at all: the
// session never opened, so there is no record to derive anything from and stdout
// stays empty. Exit 2 would be wrong here — that code means the command line was
// wrong, and stretching it to cover a machine that is not set up would make one
// number mean two things.
func TestAMissingCredentialIsAHarnessErrorAndWritesNothing(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "")

	stdout, stderr, code, dir := runHeadless(t, "fix it", engine.Options{})

	if code != exitHarness {
		t.Errorf("exit code = %d, want %d", code, exitHarness)
	}
	if stdout != "" {
		t.Errorf("a session that never opened wrote to stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, engine.APIKeyEnv) {
		t.Errorf("stderr does not name the variable that is missing:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".kopicode")); !os.IsNotExist(err) {
		t.Errorf("a session that never opened created %s", filepath.Join(dir, ".kopicode"))
	}
}

// TestAStreamThatCouldNotBeWrittenIsAHarnessError.
//
// The condition: stdout refuses the write. A --print whose output did not reach
// the reader has failed at the one job it has, and reporting the session's own
// success would hand a consumer half a record and a zero — the fail-open
// direction this repository keeps designing out.
func TestAStreamThatCouldNotBeWrittenIsAHarnessError(t *testing.T) {
	srv := scriptedProvider(t, sseBody("fine"))
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")

	dir := t.TempDir()
	selection, err := engine.ResolveSelection(dir, engine.SelectionOverrides{})
	if err != nil {
		t.Fatalf("resolving the arm: %v", err)
	}

	var stderr strings.Builder
	code := headless("fix it", brokenWriter{}, &stderr, engine.Options{
		Dir:             dir,
		Selection:       selection,
		ProviderBaseURL: srv.URL,
	})

	if code != exitHarness {
		t.Errorf("exit code = %d, want %d. stderr:\n%s", code, exitHarness, stderr.String())
	}
	if !strings.Contains(stderr.String(), "JSON stream") {
		t.Errorf("stderr does not say the stream failed:\n%s", stderr.String())
	}
	// The session still ran and still recorded itself. A surface that could not
	// print is not a session that did not happen.
	if events := findJournal(t, dir); !strings.Contains(events, "SessionEnded") {
		t.Error("the record has no ending, so the session was abandoned rather than closed")
	}
}

type brokenWriter struct{}

var errBrokenWriter = errors.New("the reader went away")

func (brokenWriter) Write([]byte) (int, error) { return 0, errBrokenWriter }

// --- the shape --------------------------------------------------------------

// TestTheStreamIsOneJSONObjectPerLine, whatever the session did.
//
// It is the property a consumer relies on before it reads a single field, and it
// is asserted over a session carrying tool calls, tool output and a permission
// refusal rather than over a hand-built value — the encoder's escaping and any
// newline inside a payload are exactly what would break it.
func TestTheStreamIsOneJSONObjectPerLine(t *testing.T) {
	srv := scriptedProvider(t,
		sseToolCall("call-1", "run_shell", `{"command":"echo <hi> & bye"}`),
		sseBody("a reply\nwith a newline in it"),
	)
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")

	stdout, stderr, code, _ := runHeadless(t, "try a shell", engine.Options{ProviderBaseURL: srv.URL})
	if stdout == "" {
		t.Fatalf("the session printed nothing (exit %d):\n%s", code, stderr)
	}

	ls := stream(t, stdout) // fails the test on any line that is not JSON
	for _, l := range ls {
		if kind, _ := l["kind"].(string); kind == "" {
			t.Errorf("a line carries no kind: %v", l)
		}
	}

	// A headless run has nobody to ask, so the shell is refused and the refusal
	// is on the record rather than swallowed.
	if !slices.Contains(kinds(ls), "permission_decided") {
		t.Errorf("the shell request produced no permission_decided line: %v", kinds(ls))
	}
	for _, l := range ls {
		if l["kind"] == "permission_decided" && l["decision"] != "deny" {
			t.Errorf("a headless run approved a shell command: %v", l)
		}
	}

	// The encoder does not rewrite the bytes the record holds. encoding/json
	// turns `<`, `>` and `&` into \u003c, \u003e and \u0026 by default, which is
	// valid JSON over different bytes than the journal has — the trap
	// journal.Marshal exists for. The parsed arguments carry all three.
	//
	// The `tool_call_requested` line on this same stream carries them unescaped
	// too. It did not until KAN-884: internal/parse built Call.Raw with
	// json.Marshal, so the escaping was already in the record before anything
	// derived from it, and this projection carried it across unchanged —
	// correctly, because a projection reports the record rather than repairing
	// it. With the record fixed upstream, nothing on this stream escapes, so the
	// assertion is over every line rather than over one substring.
	if !strings.Contains(stdout, `echo <hi> & bye`) {
		t.Errorf("the stream HTML-escapes what the record holds verbatim:\n%s", stdout)
	}
	for _, escape := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if strings.Contains(stdout, escape) {
			t.Errorf("the stream carries the HTML escape %s; the record holds those bytes verbatim:\n%s",
				escape, stdout)
		}
	}
}

// TestACancellationReachesTheStream.
//
// KAN-857's line on this surface, asserted on the projection rather than end to
// end: `headless` runs on context.Background() and a test has no way to cancel
// it, so causing a real interruption here would mean inventing a seam for it.
// The real cancellation is caused and asserted where it happens — internal/
// engine's TestAMidStreamCancellationIsOnTheRecordOnDisk drives one and reads
// the journal off disk. What is left to hold here is that the event survives the
// projection: the kind, the turn, the phase and the detail all reach the line,
// and none of them is dropped by an `omitempty` that made sense for another
// kind.
func TestACancellationReachesTheStream(t *testing.T) {
	r := recordOf(engine.Event{
		Kind:   engine.EventTurnCancelled,
		Seq:    8,
		Turn:   2,
		Reason: "provider_stream",
		Text:   "engine: provider reply turn 2 attempt 1: context canceled",
		Size:   57,
	})

	line, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("encoding the line: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("decoding %s: %v", line, err)
	}

	want := map[string]any{
		"kind":   "turn_cancelled",
		"seq":    float64(8),
		"turn":   float64(2),
		"reason": "provider_stream",
		"text":   "engine: provider reply turn 2 attempt 1: context canceled",
		"size":   float64(57),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v — in %s", k, got[k], v, line)
		}
	}
	// The turn is the whole of "which turn was interrupted", and a turn of 0 is
	// what `omitempty` produces for an event outside any turn. Dropping it here
	// would make a cancelled turn indistinguishable from a cancelled session.
	if _, ok := got["turn"]; !ok {
		t.Errorf("the line carries no turn: %s", line)
	}
	if _, ok := got["exit_code"]; ok {
		t.Errorf("a cancellation has no exit code and the line invented one: %s", line)
	}
}

// writeVerifyConfig gives the fixture directory a .kopicode/config.toml naming
// its own verification command, so the test does not depend on what discovery
// would infer from an otherwise empty tree.
func writeVerifyConfig(t *testing.T, dir string, argv string) {
	t.Helper()

	cfgDir := filepath.Join(dir, ".kopicode")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", cfgDir, err)
	}
	path := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(path, []byte("verify = "+argv+"\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
