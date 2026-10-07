package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/provider/mock"
	"github.com/leejianrong/kopicode/internal/tools"
)

// runShellSwitchable runs one scripted `command` through a switchable session
// that flips to auto (or not) just before the turn, and reports what the record
// says and how many times the person was asked.
func runShellSwitchable(t *testing.T, command string, auto bool) (events []journal.Event, asked int) {
	t.Helper()
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-shell", tools.ToolRunShell, `{"command":"`+command+`"}`)},
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		{text: "Understood.", usage: wireUsage{Prompt: 20, Completion: 4, Total: 24}},
	}
	prov := script(t, replies, oneAttemptPerTurn(2))
	s, _ := openWithProvider(t, prov, engine.Options{
		Switchable: true,
		Consent: func(context.Context, engine.ConsentRequest) (engine.ConsentReply, error) {
			asked++
			return engine.Reply(engine.ConsentDeny), nil
		},
	})
	if auto {
		if !s.SetAuto(true) || !s.Auto() {
			t.Fatal("a switchable session did not enter auto")
		}
	}
	if _, err := s.Run(t.Context(), "run it"); err != nil {
		t.Logf("Run: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return readJournal(t, s.Path()), asked
}

func TestASwitchableSessionAsksUntilItIsSwitchedToAuto(t *testing.T) {
	events, asked := runShellSwitchable(t, "echo hello", false)
	dec := sole[journal.PermissionDecided](t, events)
	if asked != 1 || dec.Source != permission.SourceUser.String() || dec.Decision != permission.VerdictDeny.String() {
		t.Fatalf("default mode: asked %d, decision %s from %s; want the person asked once and refusing", asked, dec.Decision, dec.Source)
	}

	events, asked = runShellSwitchable(t, "echo hello", true)
	dec = sole[journal.PermissionDecided](t, events)
	if asked != 0 || dec.Source != permission.SourceAuto.String() || dec.Decision != permission.VerdictAllow.String() {
		t.Fatalf("auto mode: asked %d, decision %s from %s; want auto allowing without asking", asked, dec.Decision, dec.Source)
	}
}

func TestAutoStillRefusesABlunderAndSaysWhatToDoInstead(t *testing.T) {
	events, asked := runShellSwitchable(t, "pip install -r requirements.txt", true)
	dec := sole[journal.PermissionDecided](t, events)
	if asked != 0 || dec.Decision != permission.VerdictDeny.String() || dec.Source != permission.SourceAuto.String() {
		t.Fatalf("decision %s from %s (asked %d); want auto's refusal", dec.Decision, dec.Source, asked)
	}
	if want := "uv add"; !strings.Contains(dec.Reason, want) {
		t.Errorf("reason %q does not point at %q", dec.Reason, want)
	}
}

func TestSetAutoIsRefusedOnASessionThatCannotSwitch(t *testing.T) {
	s, _ := openIn(t, shellFixture, engine.Options{Consent: func(context.Context, engine.ConsentRequest) (engine.ConsentReply, error) {
		return engine.Reply(engine.ConsentDeny), nil
	}})
	if s.SetAuto(true) || s.Auto() {
		t.Error("a session opened without Switchable switched modes")
	}
	_ = s.Close(t.Context())
}

func TestSwitchableNeedsSomethingToAskAndNoOtherAnswerer(t *testing.T) {
	prov, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	consent := func(context.Context, engine.ConsentRequest) (engine.ConsentReply, error) {
		return engine.Reply(engine.ConsentAllow), nil
	}
	for name, o := range map[string]engine.Options{
		"no consent":     {Switchable: true},
		"with auto mode": {Switchable: true, Consent: consent, ConsentMode: engine.ConsentAuto},
		"read only":      {Switchable: true, Consent: consent, ReadOnly: true},
		"start alone":    {StartAuto: true, Consent: consent},
	} {
		t.Run(name, func(t *testing.T) {
			opts := engine.Options{
				Dir: t.TempDir(), SessionID: "sw", Now: fixedClock(), Selection: testSelection(prov),
				Switchable: o.Switchable, StartAuto: o.StartAuto, Consent: o.Consent,
				ConsentMode: o.ConsentMode, ReadOnly: o.ReadOnly,
			}
			if _, err := engine.Open(t.Context(), opts); !errors.Is(err, engine.ErrConfig) {
				t.Errorf("Open = %v, want ErrConfig", err)
			}
		})
	}
}
