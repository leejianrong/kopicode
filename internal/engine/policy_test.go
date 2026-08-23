package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/provider/mock"
	"github.com/leejianrong/kopicode/internal/tools"
)

// TestOptionsPolicyUnsetMatchesTheExistingDefault is KAN-987's non-regression
// proof: ADR-0011 decision 3 requires that a caller who never touches
// Options.Policy sees byte-identical behaviour to before this field existed.
// `run --print` with no --policy-file is exactly this case — Consent stays
// nil and ConsentMode is ConsentUnattended, the wiring
// cmd/kopicode/print.go's headless already used before KAN-987 — so this
// drives that same shape through Open and checks the resulting
// PermissionDecided against permission.NewUnattendedDeny's own Decide output
// directly, rather than against a hand-copied string that could quietly
// drift from it.
func TestOptionsPolicyUnsetMatchesTheExistingDefault(t *testing.T) {
	events := shellConsentEvents(t, nil, engine.ConsentUnattended)
	dec := sole[journal.PermissionDecided](t, events)

	want, err := permission.NewUnattendedDeny().Decide(t.Context(), permission.Request{Kind: permission.KindRunShell})
	if err != nil {
		t.Fatalf("computing the reference decision: %v", err)
	}
	if dec.Decision != want.Verdict.String() {
		t.Errorf("decision = %q, want %q", dec.Decision, want.Verdict)
	}
	if dec.Source != want.Source.String() {
		t.Errorf("source = %q, want %q", dec.Source, want.Source)
	}
	if dec.Reason != want.Reason {
		t.Errorf("reason = %q, want %q — leaving Options.Policy unset must reach the exact policy "+
			"this surface used before KAN-987 introduced the field, not some other refusal", dec.Reason, want.Reason)
	}
}

// TestOptionsPolicyRefusesAliveConsenter is ADR-0011's "both configured" case
// (Options.Policy's own doc comment): two different live answerers to "may
// this action proceed" is a caller mistake this package refuses outright
// rather than picking one silently.
func TestOptionsPolicyRefusesAliveConsenter(t *testing.T) {
	prov, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}

	pf := &engine.PolicyFile{Root: t.TempDir(), Allow: [][]string{{"go", "test"}}}
	dir := t.TempDir()

	_, err = engine.Open(t.Context(), engine.Options{
		Dir:       dir,
		SessionID: "conflict",
		Now:       fixedClock(),
		Selection: testSelection(prov),
		// Provider is left nil deliberately: the refusal below fires before
		// requireProviderCredential ever runs, so this test never needs a
		// working provider to prove its point.
		Policy: pf,
		Consent: func(context.Context, engine.ConsentRequest) (engine.ConsentAnswer, error) {
			return engine.ConsentDeny, nil
		},
	})
	if err == nil {
		t.Fatal("Open accepted both Options.Policy and a live Options.Consent")
	}
	if !errors.Is(err, engine.ErrConfig) {
		t.Errorf("err = %v, want it to wrap engine.ErrConfig", err)
	}
}

// TestOptionsPolicyToleratesConsentModeAlone proves the narrower half of the
// same rule: headless's own wiring always sets ConsentMode to
// ConsentUnattended regardless of whether --policy-file was passed, so
// Options.Policy set alongside a non-default ConsentMode (with Consent left
// nil) must not be treated as the ambiguous case above — otherwise
// --policy-file could never be used from `run --print` at all.
func TestOptionsPolicyToleratesConsentModeAlone(t *testing.T) {
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-1", tools.ToolRunShell, `{"command":"echo hello"}`)},
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		{text: "Understood.", usage: wireUsage{Prompt: 20, Completion: 4, Total: 24}},
	}
	prov := script(t, replies, oneAttemptPerTurn(2))

	pf := &engine.PolicyFile{Root: t.TempDir(), Allow: [][]string{tools.ShellArgv("echo hello")}}
	s, _ := openWithProvider(t, prov, engine.Options{Policy: pf, ConsentMode: engine.ConsentUnattended})
	if _, err := s.Run(t.Context(), "run the tests"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dec := sole[journal.PermissionDecided](t, readJournal(t, s.Path()))
	if dec.Decision != permission.VerdictAllow.String() {
		t.Errorf("decision = %q, want allow — the declared allowlist matches this argv exactly", dec.Decision)
	}
}

// TestPolicyFileEndToEndThroughOpen drives ADR-0011's whole point: a small
// declared-allowlist file, loaded through the exact function
// cmd/kopicode/print.go's --policy-file flag calls
// ([engine.LoadPolicyFile]), wired into [engine.Options.Policy], governing a
// real two-shell-command session run through the mock/replay provider —
// one command that exact-matches the file's declared allow list, one that
// does not.
//
// cmd/kopicode's own test suite cannot drive this through `runPrint` itself:
// unlike engine.Options.ProviderBaseURL, there is no flag or environment
// variable that points `run --print`'s provider at a test server, so every
// end-to-end test in cmd/kopicode/print_test.go already drives `headless`
// directly with hand-built Options rather than parsing flags — see that
// file's own doc comment. This test reaches as far into that surface's
// territory as is reachable without live-provider spend: it exercises
// LoadPolicyFile itself (not a hand-built PolicyFile value) against a real
// file on disk, wires the result into Options.Policy exactly as
// cmd/kopicode/print.go's runPrint does, and drives the session through
// engine.Open — everything downstream of the flag string being parsed.
// cmd/kopicode/print_test.go separately covers that a malformed
// --policy-file is refused with exit 2, which is the part only reachable
// through runPrint itself.
func TestPolicyFileEndToEndThroughOpen(t *testing.T) {
	root := t.TempDir()
	allowedArgv := tools.ShellArgv("echo hello")

	quoted := make([]string, len(allowedArgv))
	for i, a := range allowedArgv {
		quoted[i] = strconv.Quote(a)
	}
	content := "# a declared allowlist for this test session\n" +
		"root = " + strconv.Quote(root) + "\n" +
		"allow = [[" + strings.Join(quoted, ", ") + "]]\n"

	path := filepath.Join(t.TempDir(), "policy.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the fixture policy file: %v", err)
	}

	pf, err := engine.LoadPolicyFile(path)
	if err != nil {
		t.Fatalf("LoadPolicyFile(%s): %v", path, err)
	}

	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-1", tools.ToolRunShell, `{"command":"echo hello"}`)},
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		{calls: []wireCall{nativeCall("call-2", tools.ToolRunShell, `{"command":"rm -rf /"}`)},
			usage: wireUsage{Prompt: 12, Completion: 5, Total: 17}},
		{text: "done", usage: wireUsage{Prompt: 20, Completion: 4, Total: 24}},
	}
	prov := script(t, replies, oneAttemptPerTurn(3))

	s, _ := openWithProvider(t, prov, engine.Options{Policy: &pf, ConsentMode: engine.ConsentUnattended})
	if _, err := s.Run(t.Context(), "run the tests"); err != nil {
		// A refused tool is an observation on the record, not a failed exchange.
		t.Logf("Run: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	decisions := payloadsOf[journal.PermissionDecided](t, readJournal(t, s.Path()))
	if len(decisions) != 2 {
		t.Fatalf("got %d PermissionDecided events, want 2 (one per shell call)", len(decisions))
	}
	for _, dec := range decisions {
		if dec.Source != permission.SourcePolicy.String() {
			t.Errorf("decision %+v: source = %q, want %q — never a human's", dec, dec.Source, permission.SourcePolicy)
		}
	}
	if decisions[0].Decision != permission.VerdictAllow.String() {
		t.Errorf("first decision = %q, want allow (matches the declared allowlist exactly)", decisions[0].Decision)
	}
	if decisions[1].Decision != permission.VerdictDeny.String() {
		t.Errorf("second decision = %q, want deny (not on the declared allowlist)", decisions[1].Decision)
	}
}
