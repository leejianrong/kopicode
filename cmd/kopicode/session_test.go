package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/lineedit"
	"github.com/leejianrong/kopicode/internal/engine"
)

// This file drives the whole front end — resolve the arm, open a session,
// render its record, exit — over real HTTP against a server this test stands
// up.
//
// It goes through the live client rather than a replay provider on purpose, and
// not only because ADR-0003's allowlist puts internal/provider/mock out of
// reach. The thing under test is the *wiring*: that engine.Open builds a client
// that talks, that the loop renders what the session records, and that the exit
// code comes from the stop. A fake substituted at the provider interface would
// leave the client, the SSE reader and the request shape untested here, which
// is exactly the layer this file exists to cover.

// sseBody is one streamed reply: two content deltas, a finish, and the
// sentinel.
//
// It is hand-written rather than borrowed from internal/provider/fixture, which
// a front end may not import. That is a real limitation and worth stating: this
// asserts that the front end can drive *a* well-formed stream, not that it can
// drive OpenRouter's. The fixtures and internal/provider's own tests are where
// wire fidelity is held.
func sseBody(text string) string {
	first, second, _ := strings.Cut(text, " ")
	if second != "" {
		second = " " + second
	}

	var b strings.Builder
	frame := func(format string, args ...any) {
		fmt.Fprintf(&b, "data: "+format+"\n\n", args...)
	}
	frame(`{"id":"gen-1","object":"chat.completion.chunk","model":"qwen/qwen3-coder-next",`+
		`"provider":"Parasail","choices":[{"index":0,"delta":{"role":"assistant","content":%q},`+
		`"finish_reason":null}]}`, first)
	frame(`{"id":"gen-1","object":"chat.completion.chunk","choices":[{"index":0,`+
		`"delta":{"content":%q},"finish_reason":null}]}`, second)
	frame(`{"id":"gen-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// providerServer serves one streamed reply per request and records what it was
// asked for.
func providerServer(t *testing.T, text string) (*httptest.Server, *[]string) {
	t.Helper()

	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody(text)))
	}))
	t.Cleanup(srv.Close)
	return srv, &auth
}

// runSession drives the front end in a fresh directory with typed already
// typed, and returns stdout, stderr and the exit code.
func runSession(t *testing.T, typed string, opts engine.Options) (string, string, int, string) {
	t.Helper()

	dir := t.TempDir()
	selection, err := engine.ResolveSelection(dir, engine.SelectionOverrides{})
	if err != nil {
		t.Fatalf("resolving the default arm: %v", err)
	}

	opts.Dir = dir
	opts.Selection = selection

	var out, errOut strings.Builder
	code := session(streams{
		in:  strings.NewReader(typed),
		out: &out,
		err: &errOut,
		// A test is not a terminal, and saying so is the whole point: this is
		// the piped path, where SLICE-1 requires escape-free line-oriented
		// output.
		terminal: lineedit.NonInteractive(),
	}, opts)

	return out.String(), errOut.String(), code, dir
}

// TestTheFrontEndDrivesARealSession is the card's end-to-end: `kopicode`
// resolves an arm, opens a session through engine.Open, runs a turn against a
// provider, renders what the record holds, and exits 0.
func TestTheFrontEndDrivesARealSession(t *testing.T) {
	srv, auth := providerServer(t, "the fix is in Open")
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")

	stdout, stderr, code, dir := runSession(t, "why does it fail?\n", engine.Options{
		ProviderBaseURL: srv.URL,
	})

	if code != exitSuccess {
		t.Fatalf("exit code = %d, want %d. stderr:\n%s\nstdout:\n%s",
			code, exitSuccess, stderr, stdout)
	}
	if len(*auth) == 0 {
		t.Fatal("the session never called the provider")
	}

	// What the model said reached the screen.
	if !strings.Contains(stdout, "the fix is in Open") {
		t.Errorf("the reply did not reach stdout:\n%s", stdout)
	}
	// And so did the record's own bookends, which is what "derived from the
	// journal" looks like from outside.
	for _, want := range []string{"[session]", "ended: completed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, stdout)
		}
	}

	// The record is on disk where the surface said it was.
	events := findJournal(t, dir)
	if !strings.Contains(events, "SessionStarted") || !strings.Contains(events, "SessionEnded") {
		t.Errorf("the journal is missing its bookends:\n%s", events)
	}
	if !strings.Contains(events, "the fix is in Open") {
		t.Error("the journal does not hold the message the user was shown")
	}
}

// TestPipedOutputFromTheRealBinaryHasNoEscapes is KAN-793's acceptance
// criterion, asserted one layer higher than the repl package can reach: over a
// session that actually ran, with a provider, a journal and an exit code.
func TestPipedOutputFromTheRealBinaryHasNoEscapes(t *testing.T) {
	srv, _ := providerServer(t, "no escapes here")
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")

	stdout, _, _, _ := runSession(t, "one\ntwo\n", engine.Options{ProviderBaseURL: srv.URL})

	if stdout == "" {
		t.Fatal("the session printed nothing, so this asserted nothing")
	}
	if i := strings.IndexByte(stdout, 0x1b); i >= 0 {
		t.Errorf("piped output contains an escape at byte %d: %q\nfull output:\n%s",
			i, stdout[i:min(i+16, len(stdout))], stdout)
	}
	for i := range len(stdout) {
		if c := stdout[i]; c < 0x20 && c != '\n' {
			t.Errorf("piped output contains control byte %#x at %d", c, i)
		}
	}
}

// TestTheAPIKeyReachesTheProviderAndNothingElse.
//
// The credential has to travel — a session that could not authenticate is not a
// session — so what matters is that it goes exactly one place. It must not be
// on stdout, not on stderr, and not in the record (internal/engine's own leak
// test covers the journal in depth; this covers the front end, which is the
// layer that prints).
func TestTheAPIKeyReachesTheProviderAndNothingElse(t *testing.T) {
	// Deliberately not shaped like a real credential.
	//
	// An earlier version of this used OpenRouter's actual `sk-or-v1-` + 64 hex
	// form, and GitHub's push protection refused the branch — correctly, since
	// a scanner cannot tell a convincing fake from the real thing and a
	// repository that trains people to click "allow" on that warning has lost
	// the protection. The test loses nothing: the journal's redactor takes its
	// values from the environment rather than matching a shape, so what it
	// redacts is whatever this string is.
	const key = "kopicode-test-credential-not-a-real-key"

	srv, auth := providerServer(t, "hello")
	t.Setenv(engine.APIKeyEnv, key)

	stdout, stderr, _, dir := runSession(t, "hi\n", engine.Options{ProviderBaseURL: srv.URL})

	if len(*auth) == 0 || !strings.Contains((*auth)[0], key) {
		t.Fatalf("the provider did not receive the key; headers seen: %v", *auth)
	}
	if strings.Contains(stdout, key) {
		t.Error("the API key appeared on stdout")
	}
	if strings.Contains(stderr, key) {
		t.Error("the API key appeared on stderr")
	}
	if events := findJournal(t, dir); strings.Contains(events, key) {
		t.Error("the API key appeared in the journal")
	}
}

// TestAMissingKeyIsAHarnessErrorAndWritesNothing.
//
// Exit 2 is ADR-0007 decision 4's case — an id this binary does not recognise —
// and stretching it to cover a machine that is not set up would make the code
// meaning "your command line was wrong" also mean "your environment was". The
// other half is that nothing is created either way: the credential is checked
// before the journal is opened.
func TestAMissingKeyIsAHarnessErrorAndWritesNothing(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "")

	stdout, stderr, code, dir := runSession(t, "hi\n", engine.Options{})

	if code != exitHarness {
		t.Errorf("exit code = %d, want %d", code, exitHarness)
	}
	if !strings.Contains(stderr, engine.APIKeyEnv) {
		t.Errorf("stderr does not name the variable that is missing:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("a session that never opened wrote to stdout:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, ".kopicode")); !os.IsNotExist(err) {
		t.Errorf("a session that never opened created %s", filepath.Join(dir, ".kopicode"))
	}
}

// TestExitCodeComesFromTheStop. The surface owns no second table: whatever the
// loop concluded, engine.Stop.ExitCode decides what the process reports.
func TestExitCodeComesFromTheStop(t *testing.T) {
	srv, _ := providerServer(t, "done")
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")

	// An empty stdin is an immediate EOF: the session opens, records its
	// bookends and closes cleanly, which is engine.StopCompleted and exit 0.
	_, stderr, code, _ := runSession(t, "", engine.Options{ProviderBaseURL: srv.URL})
	if code != engine.StopCompleted.ExitCode() {
		t.Errorf("exit code = %d, want %d. stderr:\n%s",
			code, engine.StopCompleted.ExitCode(), stderr)
	}
}

// findJournal returns the session's events.jsonl, whichever session id was
// minted.
//
// It reads the file as text rather than decoding it, because a front end may
// not import internal/journal — which is the constraint this whole card is
// about. What is asserted here is presence and absence of strings, and those
// are the questions a text read answers correctly.
func findJournal(t *testing.T, dir string) string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, ".kopicode", "sessions", "*", "events.jsonl"))
	if err != nil {
		t.Fatalf("looking for the journal: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d journals under %s, want 1", len(matches), dir)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("reading %s: %v", matches[0], err)
	}
	return string(b)
}
