package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider/mock"
)

// writeAskPolicyFile writes content to a fresh file under dir and returns its
// path.
func writeAskPolicyFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "ask-policy.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// TestReadingTheAskPolicyFile drives the flat-key reader (ADR-0013 decision 6)
// end to end: the one-key grammar it accepts, and — more load-bearing — every
// line it refuses rather than guesses at, mirroring
// internal/permission/allowlist_file_test.go's rigor for its structurally
// identical reader.
func TestReadingTheAskPolicyFile(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantNote string
		wantErr  string
	}{
		{
			name:     "a basic-string note",
			content:  `note = "proceed conservatively and record the assumption"` + "\n",
			wantNote: "proceed conservatively and record the assumption",
		},
		{
			name:     "comments and blank lines around the note",
			content:  "# a declared ask policy\n\nnote = 'assume the safe reading'   # for unattended runs\n",
			wantNote: "assume the safe reading",
		},
		{
			name:    "missing note",
			content: "# nothing declared\n",
			wantErr: `missing required key "note"`,
		},
		{
			name:    "an empty note",
			content: `note = ""` + "\n",
			wantErr: "empty string",
		},
		{
			name:    "note set twice",
			content: `note = "one"` + "\n" + `note = "two"` + "\n",
			wantErr: "set twice",
		},
		{
			name:    "a table header is refused, not silently skipped",
			content: "[note]\nx = 1\n",
			wantErr: "table headers are not supported",
		},
		{
			name:    "an unrecognised key",
			content: `root = "/repo"` + "\n",
			wantErr: "not a key this file recognizes",
		},
		{
			name:    "a dotted key",
			content: `ask.note = "x"` + "\n",
			wantErr: "not a bare key",
		},
		{
			name:    "an unquoted note value",
			content: "note = proceed\n",
			wantErr: "not a quoted string",
		},
		{
			name:    "a backslash escape in a basic string",
			content: `note = "line one\nline two"` + "\n",
			wantErr: "backslash escape",
		},
		{
			name:    "trailing junk after the closing quote",
			content: `note = "x" oops` + "\n",
			wantErr: "trailing text",
		},
		{
			name:    "a line that is not a pair",
			content: "note\n",
			wantErr: "not a key = value line",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeAskPolicyFile(t, t.TempDir(), tc.content)
			got, err := engine.LoadAskPolicyFile(path)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadAskPolicyFile succeeded, want an error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadAskPolicyFile failed: %v", err)
			}
			if got.Note != tc.wantNote {
				t.Errorf("Note = %q, want %q", got.Note, tc.wantNote)
			}
			if got.Path != path {
				t.Errorf("Path = %q, want %q", got.Path, path)
			}
		})
	}
}

// TestLoadAskPolicyFileRefusesAMissingFile: a caller who passed
// --ask-policy-file at a path that does not exist gets an error, not a silent
// fallback to the fixed headless refusal — the same posture LoadAllowlistFile
// takes, and for the same reason (there is no safe default this file could
// compile to).
func TestLoadAskPolicyFileRefusesAMissingFile(t *testing.T) {
	if _, err := engine.LoadAskPolicyFile(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("LoadAskPolicyFile succeeded reading a file that does not exist")
	}
}

// TestAskPolicyEndToEndThroughOpen is ADR-0013 decision 6's whole point: a
// small ask-policy file, loaded through the exact function
// `kopicode serve --ask-policy-file` will call ([engine.LoadAskPolicyFile]),
// wired into [engine.Options.AskPolicy], governing a real ask call run through
// the mock/replay provider. The model asks; nobody is present; the journaled
// answer must be a policy-attributed refusal whose text carries the
// orchestrator's own note, not the fixed "no human is present" dead end.
//
// This is the same seam TestPolicyFileEndToEndThroughOpen uses for the
// permission side (a real file on disk, LoadAskPolicyFile not a hand-built
// value, driven through engine.Open), and it asserts only observable outcomes —
// the journal event — never runAsk's internals.
func TestAskPolicyEndToEndThroughOpen(t *testing.T) {
	const note = "assume the most conservative reading and record the assumption in the summary"
	path := writeAskPolicyFile(t, t.TempDir(), `note = "`+note+`"`+"\n")

	ap, err := engine.LoadAskPolicyFile(path)
	if err != nil {
		t.Fatalf("LoadAskPolicyFile(%s): %v", path, err)
	}

	prov := script(t, askTurn("which retry backoff should I use?", "checked backoff.go, nothing declared"),
		oneAttemptPerTurn(2))

	// Ask is left nil deliberately: AskPolicy is the answerer, and setting both
	// is the conflict the next test proves Open refuses.
	s, _ := openWithProvider(t, prov, engine.Options{AskPolicy: &ap})

	if _, err := s.Run(t.Context(), "fix the retry backoff"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ansEv := sole[journal.AskAnswered](t, readJournal(t, s.Path()))
	if !ansEv.Refused {
		t.Error("AskAnswered.Refused = false; a policy answer is nobody actually answering this question")
	}
	if ansEv.Source != "policy" {
		t.Errorf("AskAnswered.Source = %q, want %q — the answer came from a declared policy, never a human",
			ansEv.Source, "policy")
	}
	if !strings.Contains(ansEv.Answer.Inline, note) {
		t.Errorf("AskAnswered.Answer = %q, want it to carry the orchestrator's note %q", ansEv.Answer.Inline, note)
	}
}

// TestOptionsAskPolicyRefusesALiveAsk is ADR-0013 decision 6's "both configured"
// case, the ask-side twin of TestOptionsPolicyRefusesAliveConsenter: an
// AskPolicy and a live Ask are two different answerers for the same question,
// and Open refuses to guess between them rather than silently picking one.
func TestOptionsAskPolicyRefusesALiveAsk(t *testing.T) {
	prov, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}

	ap := &engine.AskPolicyFile{Note: "proceed conservatively"}

	_, err = engine.Open(t.Context(), engine.Options{
		Dir:       t.TempDir(),
		SessionID: "ask-conflict",
		Now:       fixedClock(),
		Selection: testSelection(prov),
		// Provider is left nil deliberately: the refusal fires before
		// requireProviderCredential ever runs.
		AskPolicy: ap,
		Ask: func(context.Context, engine.AskRequest) (engine.AskAnswer, error) {
			return engine.AskAnswer{Text: "sure"}, nil
		},
	})
	if err == nil {
		t.Fatal("Open accepted both Options.AskPolicy and a live Options.Ask")
	}
	if !errors.Is(err, engine.ErrConfig) {
		t.Errorf("err = %v, want it to wrap engine.ErrConfig", err)
	}
}
