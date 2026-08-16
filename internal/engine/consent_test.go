package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

// shellFixture is the recording whose second turn asks to run a command, so a
// session driving it reaches the consent gate.
const shellFixture = "two_turn_native_tool_call"

// consentTurn runs a session whose consent is answered by consent, and returns
// what the record holds about permission.
//
// It asserts on the journal rather than on a call count, because "was consent
// asked and what was decided" is a question about the record: the engine writes
// PermissionRequested and PermissionDecided, and a bench run's auto-approval
// must not be indistinguishable from a human saying yes.
func consentEvents(t *testing.T, consent engine.Consenter) []journal.Event {
	t.Helper()

	s, _ := openIn(t, shellFixture, engine.Options{Consent: consent})
	if _, err := s.Run(t.Context(), "run the tests"); err != nil {
		// A refused tool is an observation, not a failure of the exchange, so
		// this only fires when something genuinely broke.
		t.Logf("Run: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return readJournal(t, s.Path())
}

// TestANilConsenterRefuses is the fail-closed default, and it is the one that
// matters most: a front end that forgot to wire consent must not thereby get a
// session that runs model-authored shell unasked.
//
// The recording the harness replays only reads a file, so this checks the
// adapter directly as well as through a session — the gate is what turns a
// refusal into an observation, and both halves have to hold.
func TestANilConsenterRefuses(t *testing.T) {
	verdict, err := engine.AskForTest(t.Context(), nil, engine.ConsentRequest{
		Kind: "run_shell", Tool: "run_shell", Detail: "rm -rf /",
	})

	if verdict != "deny" {
		t.Errorf("verdict = %s, want deny — forgetting to wire consent must not be the quiet "+
			"way to approve everything", verdict)
	}
	if err == nil {
		t.Error("a request nobody could be asked produced no error")
	}
	if err != nil && !strings.Contains(err.Error(), "run_shell") {
		t.Errorf("the refusal does not name what was refused: %v", err)
	}
}

// TestAConsenterErrorIsARefusal. An asker that fails — a closed stdin, a turn
// the user just interrupted — must not be read as consent. Treating an
// unanswerable question as a yes is the failure the whole permission package
// exists to make impossible.
func TestAConsenterErrorIsARefusal(t *testing.T) {
	boom := errors.New("stdin closed")
	verdict, err := engine.AskForTest(t.Context(),
		func(context.Context, engine.ConsentRequest) (engine.ConsentAnswer, error) {
			return engine.ConsentAllow, boom
		},
		engine.ConsentRequest{Kind: "run_shell"},
	)

	if verdict != "deny" {
		t.Errorf("verdict = %s, want deny", verdict)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap %v", err, boom)
	}
}

// TestAnUndeclaredAnswerIsNotAnApproval. ConsentAnswer is a small enumeration
// and a value outside it is a surface bug; the safe reading of a surface bug is
// no.
func TestAnUndeclaredAnswerIsNotAnApproval(t *testing.T) {
	verdict, err := engine.AskForTest(t.Context(),
		func(context.Context, engine.ConsentRequest) (engine.ConsentAnswer, error) {
			return engine.ConsentAnswer(200), nil
		},
		engine.ConsentRequest{Kind: "run_shell"},
	)

	if verdict != "deny" {
		t.Errorf("verdict = %s, want deny", verdict)
	}
	if err == nil {
		t.Error("an answer nobody declared was accepted silently")
	}
}

// TestTheAnswersMapOntoTheJournalsVocabulary.
//
// The engine writes PermissionDecided.Decision from the verdict this adapter
// returns, so the two vocabularies have to line up exactly — internal/permission
// says so in its package doc and holds no test that can see this side.
func TestTheAnswersMapOntoTheJournalsVocabulary(t *testing.T) {
	for answer, want := range map[engine.ConsentAnswer]string{
		engine.ConsentDeny:         "deny",
		engine.ConsentAllow:        "allow",
		engine.ConsentAllowSession: "allow_session",
	} {
		if got := answer.String(); got != want {
			t.Errorf("ConsentAnswer(%d).String() = %q, want %q", uint8(answer), got, want)
		}

		verdict, err := engine.AskForTest(t.Context(),
			func(context.Context, engine.ConsentRequest) (engine.ConsentAnswer, error) {
				return answer, nil
			},
			engine.ConsentRequest{Kind: "run_shell"},
		)
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if got := verdict; got != want {
			t.Errorf("answer %s produced verdict %q, want %q — the journal records the verdict, "+
				"so a mismatch is a decision the record attributes wrongly", answer, got, want)
		}
	}
}

// TestDenyIsTheZeroAnswer. A surface that fell through a switch, or a caller
// that dropped the value on an error path, still fails closed.
func TestDenyIsTheZeroAnswer(t *testing.T) {
	var zero engine.ConsentAnswer
	if zero != engine.ConsentDeny {
		t.Errorf("the zero ConsentAnswer is %v, want %v", zero, engine.ConsentDeny)
	}
}

// TestConsentIsNotAskedForAReadOnlySession.
//
// Reads never ask (internal/permission's fixed rules), and a session that asked
// about every file the agent opened would produce a user who approves without
// reading — which costs the shell prompt its meaning. The shipped recordings
// only read, so the record must hold no permission events at all.
func TestConsentIsNotAskedForAReadOnlySession(t *testing.T) {
	asked := 0
	events := consentEvents(t, func(context.Context, engine.ConsentRequest) (engine.ConsentAnswer, error) {
		asked++
		return engine.ConsentAllow, nil
	})

	if asked != 0 {
		t.Errorf("consent was asked %d time(s) for a session that only read", asked)
	}
	for _, ev := range events {
		switch ev.Type() {
		case journal.TypePermissionRequested, journal.TypePermissionDecided:
			t.Errorf("a read-only session journaled %s", ev.Type())
		}
	}
}
