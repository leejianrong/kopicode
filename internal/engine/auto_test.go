package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/provider/mock"
)

// TestAutoConsentIsAttributedToAuto is ADR-0017's attribution promise through
// the real Open path: a decision made by the auto mode is on the record as
// "auto" — not a user's, and not a policy file's.
func TestAutoConsentIsAttributedToAuto(t *testing.T) {
	events := shellConsentEvents(t, nil, engine.ConsentAuto)
	dec := sole[journal.PermissionDecided](t, events)
	if dec.Source != permission.SourceAuto.String() {
		t.Errorf("source = %q, want %q", dec.Source, permission.SourceAuto)
	}
	if dec.Decision != permission.VerdictAllow.String() {
		t.Errorf("decision = %q, want allow for `echo hello` inside the root (%s)", dec.Decision, dec.Reason)
	}
}

// TestAutoConsentRefusesAnswerersThatWouldCompete: auto mode is its own
// answerer, so a caller that also supplied a Consent or a declared Policy has
// asked for two things and gets neither.
func TestAutoConsentRefusesAnswerersThatWouldCompete(t *testing.T) {
	prov, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	for name, mutate := range map[string]func(*engine.Options){
		"Consent": func(o *engine.Options) {
			o.Consent = func(context.Context, engine.ConsentRequest) (engine.ConsentAnswer, error) {
				return engine.ConsentAllow, nil
			}
		},
		"Policy": func(o *engine.Options) {
			o.Policy = &engine.PolicyFile{Root: t.TempDir(), Allow: [][]string{{"go", "test"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := engine.Options{
				Dir: t.TempDir(), SessionID: "auto-conflict", Now: fixedClock(),
				Selection: testSelection(prov), ConsentMode: engine.ConsentAuto,
			}
			mutate(&opts)
			_, err := engine.Open(t.Context(), opts)
			if !errors.Is(err, engine.ErrConfig) {
				t.Errorf("Open = %v, want an error wrapping ErrConfig", err)
			}
		})
	}
}

// TestAutoNeverAllowNeedsAutoMode: a caller who supplied a never-allow list is
// relying on it, so a mode that would not consult it is refused, not ignored.
func TestAutoNeverAllowNeedsAutoMode(t *testing.T) {
	prov, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	for name, opts := range map[string]engine.Options{
		"interactive": {ConsentMode: engine.ConsentInteractive, AutoNeverAllow: []string{"terraform apply"}},
		"unattended":  {ConsentMode: engine.ConsentUnattended, AutoNeverAllow: []string{"terraform apply"}},
		"malformed":   {ConsentMode: engine.ConsentAuto, AutoNeverAllow: []string{"rm -rf; ls"}},
	} {
		t.Run(name, func(t *testing.T) {
			opts.Dir = t.TempDir()
			opts.SessionID = "never-allow"
			opts.Now = fixedClock()
			opts.Selection = testSelection(prov)
			if _, err := engine.Open(t.Context(), opts); !errors.Is(err, engine.ErrConfig) {
				t.Errorf("Open = %v, want an error wrapping ErrConfig", err)
			}
		})
	}
}

// TestAutoIsNeverTheDefault: the zero ConsentMode must stay interactive. Auto
// has to be declared; a field a caller forgot to set may never widen what the
// session can do.
func TestAutoIsNeverTheDefault(t *testing.T) {
	var zero engine.ConsentMode
	if zero == engine.ConsentAuto {
		t.Fatal("the zero ConsentMode is ConsentAuto; auto mode would be a silent default")
	}
	if zero != engine.ConsentInteractive {
		t.Fatalf("the zero ConsentMode = %v, want ConsentInteractive", zero)
	}
}
