package engine_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/provider/mock"
)

// TestOpenLogsDiagnosticsWithoutSessionContent is KAN-771's done-when made
// concrete rather than left as a comment: opening and running a session
// through [engine.Open] emits at least one engine diagnostic over slog, and
// none of what it emits is session content — the user's prompt, the model's
// reply, or a tool call's id and arguments. Those already have exactly one
// home and it is the journal; a logger that also carried them would be the
// parallel transcript CLAUDE.md's boundary rule exists to prevent, arriving
// disguised as observability.
//
// The two forbidden substrings that matter most are the assistant's final
// reply and the tool call's id: both come off the very same fixture the
// positive assertion drives, so a slog call site added later that reached for
// "just log the reply for debugging" would fail this test rather than ship
// quietly.
func TestOpenLogsDiagnosticsWithoutSessionContent(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const prompt = `Why does Greet("") render as "Hello, !"?`
	s, _ := openIn(t, "two_turn_native_tool_call", engine.Options{})
	if _, err := s.Run(t.Context(), prompt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out := buf.String()
	if out == "" {
		t.Fatal("Open and Run together emitted no slog output at all; the new diagnostic " +
			"call sites in internal/engine/open.go did not fire")
	}

	// At least one of the call sites this card added must have fired. openIn
	// always supplies its own Provider (the mock/replay one), so the
	// live-client "connecting to provider" line never fires here — this is
	// the substitution branch's line instead.
	wantAny := []string{"opening session", "session lock acquired", "provider supplied by caller"}
	found := false
	for _, want := range wantAny {
		if strings.Contains(out, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("slog output contains none of the expected diagnostic messages %v; got:\n%s", wantAny, out)
	}

	// Session content must appear nowhere in the diagnostic stream.
	forbidden := map[string]string{
		prompt:                    "the user's prompt (a UserMessage fact)",
		"empty-name branch":       "the model's final reply (an AssistantMessage fact)",
		"call_kopicode00000001":   "the tool call's id (a ToolCallRequested/Parsed fact)",
		"internal/greet/greet.go": "the tool call's own argument (a ToolCallParsed fact)",
	}
	for bad, why := range forbidden {
		if strings.Contains(out, bad) {
			t.Errorf("slog output contains %q (%s); session content must live only in the "+
				"journal:\n%s", bad, why, out)
		}
	}
}

// TestOpenLogsTheLiveProviderConnectionWithoutTheKey drives the branch
// TestOpenLogsDiagnosticsWithoutSessionContent cannot reach: openIn always
// substitutes its own Provider, so Open's live-client path — the one every
// real `kopicode`/`kopibench` invocation actually takes — never runs there.
//
// Open never calls Provider.Complete, so a base URL nothing is listening on is
// enough: nothing is dialled before the session is closed.
func TestOpenLogsTheLiveProviderConnectionWithoutTheKey(t *testing.T) {
	const fakeKey = "kopicode-fake-key-not-a-credential-0771"
	t.Setenv(engine.APIKeyEnv, fakeKey)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	prov, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}

	s, err := engine.Open(t.Context(), engine.Options{
		Dir:             t.TempDir(),
		Selection:       testSelection(prov),
		SessionID:       "live-provider-log",
		Now:             fixedClock(),
		ProviderBaseURL: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	out := buf.String()
	if !strings.Contains(out, "connecting to provider") {
		t.Errorf("Open (no Options.Provider, a real key set) did not log the live client "+
			"connection; got:\n%s", out)
	}
	if strings.Contains(out, fakeKey) {
		t.Errorf("the credential reached slog:\n%s", out)
	}
}
