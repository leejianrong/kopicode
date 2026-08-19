package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/lineedit"
	"github.com/leejianrong/kopicode/internal/engine"
)

// This file drives KAN-939 through the real front end: session(), the same
// function --resume calls in main.go, over real HTTP against a server this
// test stands up — twice, against the same directory and session id, so the
// second invocation is what a second `kopicode --resume <id>` process would
// actually do.
//
// It does not exec the compiled binary. Doing so would need a way to point
// the live client at a fake server, and there is no such flag —
// engine.Options.ProviderBaseURL exists for exactly this kind of test and is
// not exposed on the command line, the same reason session_test.go's own
// tests call session() rather than building kopicode. What is asserted here
// is the wiring session() (and therefore main.go's --resume flag, which
// builds the same Options this file builds by hand) puts together; a
// separate, network-free check in resume_integration_test.go drives the real
// binary's flag parsing itself, on the one path that needs no server at all:
// a refusal.

// bodyCapturingServer serves one streamed reply per request and records the
// full JSON body of every request it received, so a test can hold what the
// *second* session actually sent to account rather than trusting that it
// must have been assembled correctly.
func bodyCapturingServer(t *testing.T, text string) (*httptest.Server, *[]string) {
	t.Helper()

	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody(text)))
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

// TestResumeFlagCarriesHistoryAcrossTwoProcesses is the card's front-end
// proof: one session() call opens fresh, runs one turn and exits; a second
// session() call — same directory, same session id, Resume set, exactly as
// main.go's --resume flag builds it — runs a second turn, and the *wire
// request* the second process actually sent carries the first process's
// whole exchange.
func TestResumeFlagCarriesHistoryAcrossTwoProcesses(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")

	dir := t.TempDir()
	selection, err := engine.ResolveSelection(dir, engine.SelectionOverrides{})
	if err != nil {
		t.Fatalf("resolving the default arm: %v", err)
	}

	firstSrv, _ := providerServer(t, "Hi, ready to help.")
	firstOut, firstErr, firstCode, _ := drive(t, dir, "why does it fail?\n", engine.Options{
		Dir: dir, Selection: selection, SessionID: "cli-resume-e2e", ProviderBaseURL: firstSrv.URL,
	})
	if firstCode != exitSuccess {
		t.Fatalf("first session exit = %d, want %d. stderr:\n%s\nstdout:\n%s",
			firstCode, exitSuccess, firstErr, firstOut)
	}

	secondSrv, bodies := bodyCapturingServer(t, "You asked about the fix.")
	secondOut, secondErr, secondCode, _ := drive(t, dir, "what did I just ask?\n", engine.Options{
		Dir: dir, Selection: selection, SessionID: "cli-resume-e2e", Resume: true, ProviderBaseURL: secondSrv.URL,
	})
	if secondCode != exitSuccess {
		t.Fatalf("resumed session exit = %d, want %d. stderr:\n%s\nstdout:\n%s",
			secondCode, exitSuccess, secondErr, secondOut)
	}

	if len(*bodies) == 0 {
		t.Fatal("the resumed session never called its provider")
	}
	sent := (*bodies)[0]
	for _, want := range []string{"why does it fail?", "Hi, ready to help.", "what did I just ask?"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the resumed session's first wire request does not contain %q:\n%s", want, sent)
		}
	}

	// The record on disk is one session's, continuing rather than restarting:
	// KAN-939 does not mint a second session directory for a resume.
	events := findJournal(t, dir)
	if strings.Count(events, `"type":"SessionStarted"`) != 2 {
		t.Errorf("want exactly two SessionStarted events (the open and the resume) in one journal:\n%s", events)
	}
	if !strings.Contains(events, `"session_id":"cli-resume-e2e"`) {
		t.Errorf("the journal does not carry the session id both processes shared:\n%s", events)
	}
}

// drive is runSession without minting its own directory or resolving its own
// selection, so a caller can run session() twice against the same tree.
func drive(t *testing.T, dir, typed string, opts engine.Options) (string, string, int, string) {
	t.Helper()

	var out, errOut strings.Builder
	code := session(streams{
		in:       strings.NewReader(typed),
		out:      &out,
		err:      &errOut,
		terminal: lineedit.NonInteractive(),
	}, opts)

	return out.String(), errOut.String(), code, dir
}
