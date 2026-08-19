package main

import (
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/lineedit"
	"github.com/leejianrong/kopicode/internal/engine"
)

// This file is network-free by construction, the same reason
// resume_integration_test.go's own doc comment gives for staying out of the
// `integration`-tagged file: parseForkSource and confirmFork are pure
// argv/stdin logic that never reaches a provider, so there is nothing here
// that needs a real binary or a fake server. The compiled-binary half —
// --fork parsed off a real argv, --fork refused on `run --print`, --resume
// and --fork refusing each other — is fork_integration_test.go's.

func TestParseForkSource(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    engine.ForkSource
		wantErr bool
	}{
		{name: "ordinary", raw: "20260819T101112Z-ab12cd34:2",
			want: engine.ForkSource{SessionID: "20260819T101112Z-ab12cd34", Turn: 2}},
		{name: "turn zero is legal", raw: "some-session:0",
			want: engine.ForkSource{SessionID: "some-session", Turn: 0}},
		{name: "no colon", raw: "no-colon-here", wantErr: true},
		{name: "empty id", raw: ":2", wantErr: true},
		{name: "empty turn", raw: "id:", wantErr: true},
		{name: "non-numeric turn", raw: "id:two", wantErr: true},
		{name: "negative turn", raw: "id:-1", wantErr: true},
		{name: "empty string", raw: "", wantErr: true},
		{
			// The split is on the *last* colon, so an id that happens to
			// contain one (unusual, but Options.SessionID accepts any
			// string) still separates correctly from the turn at the end.
			name: "id containing a colon splits on the last one",
			raw:  "weird:id:5",
			want: engine.ForkSource{SessionID: "weird:id", Turn: 5},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseForkSource(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseForkSource(%q) = %+v, nil; want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseForkSource(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("parseForkSource(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestConfirmForkRequiresTheExactPhrase holds confirmFork to the "no [y/N]
// shorthand" rule repl.Confirm's own doc comment argues: nothing short of
// the exact phrase, typed back in full, proceeds.
func TestConfirmForkRequiresTheExactPhrase(t *testing.T) {
	tests := []struct {
		name  string
		typed string
		want  bool
	}{
		{name: "the exact phrase", typed: forkConfirmPhrase + "\n", want: true},
		{name: "a bare yes", typed: "yes\n", want: false},
		{name: "a bare y", typed: "y\n", want: false},
		{name: "an empty line", typed: "\n", want: false},
		{name: "closed input", typed: "", want: false},
		{name: "the phrase with different case", typed: strings.ToUpper(forkConfirmPhrase) + "\n", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut strings.Builder
			std := streams{
				in:       strings.NewReader(tc.typed),
				out:      &out,
				err:      &errOut,
				terminal: lineedit.NonInteractive(),
			}
			got := confirmFork(std, "/some/dir", engine.ForkSource{SessionID: "src", Turn: 1})
			if got != tc.want {
				t.Errorf("confirmFork(%q) = %v, want %v", tc.typed, got, tc.want)
			}
			if !tc.want && !strings.Contains(errOut.String(), "cancelled") {
				t.Errorf("a declined confirmation did not say so on stderr: %q", errOut.String())
			}
			if strings.Contains(out.String(), "DELETE") == false {
				t.Errorf("the destructive warning was not printed: %q", out.String())
			}
		})
	}
}

// TestConfirmForkNamesTheDirectoryAndSource holds the warning's content to
// account: a human deciding whether to type the phrase back needs to see
// which directory is about to be wiped and which session it will be
// replaced with, not just that *something* destructive is about to happen.
func TestConfirmForkNamesTheDirectoryAndSource(t *testing.T) {
	var out, errOut strings.Builder
	std := streams{
		in:       strings.NewReader(forkConfirmPhrase + "\n"),
		out:      &out,
		err:      &errOut,
		terminal: lineedit.NonInteractive(),
	}
	if !confirmFork(std, "/repo/path", engine.ForkSource{SessionID: "session-abc", Turn: 3}) {
		t.Fatalf("confirmFork with the correct phrase = false, want true. stderr: %s", errOut.String())
	}
	for _, want := range []string{"/repo/path", "session-abc", "turn 3"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the warning does not mention %q:\n%s", want, out.String())
		}
	}
}
