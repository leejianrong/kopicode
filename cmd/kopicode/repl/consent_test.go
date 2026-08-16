package repl_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/repl"
	"github.com/leejianrong/kopicode/internal/engine"
)

// ask runs one turn that puts c to the user, with reply already typed, and
// returns the answer, what the user saw, and the error.
func ask(t *testing.T, typed string, ctxFn func(context.Context) context.Context, c repl.Consent) (repl.Answer, string, error) {
	t.Helper()

	var out strings.Builder
	var answer repl.Answer
	var askErr error

	loop, err := repl.New(repl.Config{
		// The prompt line, then the answer.
		In:  strings.NewReader("go\n" + typed),
		Out: &out,
		Turn: func(ctx context.Context, _ string, s repl.Surface) (engine.Result, error) {
			if ctxFn != nil {
				ctx = ctxFn(ctx)
			}
			answer, askErr = s.Ask(ctx, c)
			return engine.Result{Stop: engine.StopCompleted}, nil
		},
	})
	if err != nil {
		t.Fatalf("building the loop: %v", err)
	}
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return answer, out.String(), askErr
}

var shellRequest = repl.Consent{
	Kind:   "run_shell",
	Tool:   "run_shell",
	Detail: "go test ./...",
	Reason: "model-authored shell commands always require consent",
}

func TestConsentAnswers(t *testing.T) {
	tests := []struct {
		name  string
		typed string
		want  repl.Answer
	}{
		{"y allows once", "y\n", repl.AnswerAllow},
		{"yes allows once", "yes\n", repl.AnswerAllow},
		{"case does not matter", "Y\n", repl.AnswerAllow},
		{"a allows for the session", "a\n", repl.AnswerAllowSession},
		{"always allows for the session", "always\n", repl.AnswerAllowSession},
		{"n denies", "n\n", repl.AnswerDeny},
		{"enter denies", "\n", repl.AnswerDeny},
		{"whitespace denies", "   \n", repl.AnswerDeny},
		{"a typo denies", "yeah ok\n", repl.AnswerDeny},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := ask(t, tc.typed, nil, shellRequest)
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if got != tc.want {
				t.Errorf("answer = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAnUnanswerableQuestionIsNotAYes. Every path that cannot reach a person
// denies, and says why: a closed input, and a turn cancelled before the
// question could be put.
func TestAnUnanswerableQuestionIsNotAYes(t *testing.T) {
	t.Run("input ended", func(t *testing.T) {
		got, out, err := ask(t, "", nil, shellRequest)
		if got != repl.AnswerDeny {
			t.Errorf("answer = %v, want %v", got, repl.AnswerDeny)
		}
		if !errors.Is(err, repl.ErrNoConsent) {
			t.Errorf("err = %v, want it to wrap %v", err, repl.ErrNoConsent)
		}
		if !strings.Contains(out, "denied") {
			t.Errorf("the refusal was not shown:\n%s", out)
		}
	})

	t.Run("the turn was already cancelled", func(t *testing.T) {
		cancelled := func(ctx context.Context) context.Context {
			c, cancel := context.WithCancel(ctx)
			cancel()
			return c
		}
		got, out, err := ask(t, "y\n", cancelled, shellRequest)
		if got != repl.AnswerDeny {
			t.Errorf("answer = %v, want %v — a turn the user just interrupted must not be "+
				"the turn that gets consent", got, repl.AnswerDeny)
		}
		if !errors.Is(err, repl.ErrNoConsent) {
			t.Errorf("err = %v, want it to wrap %v", err, repl.ErrNoConsent)
		}
		if strings.Contains(out, "go test ./...") {
			t.Errorf("a cancelled turn still put the question on screen:\n%s", out)
		}
	})
}

// TestTheQuestionSaysWhatIsBeingConsentedTo — KAN-799 needs exactly one
// permission prompt for the test command to be readable and answerable.
func TestTheQuestionSaysWhatIsBeingConsentedTo(t *testing.T) {
	_, out, _ := ask(t, "y\n", nil, shellRequest)

	for _, want := range []string{
		"run_shell",
		"command: go test ./...",
		"model-authored shell commands always require consent",
		"[y]es",
		"[N]o",
		"[a]lways",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the consent prompt does not contain %q:\n%s", want, out)
		}
	}
}

// TestAWriteShowsTheResolvedPath. Consenting to "../../etc/hosts" and
// consenting to "/etc/hosts" are different acts, and only one of them is
// legible — which is why permission.Request carries the resolved path at all.
func TestAWriteShowsTheResolvedPath(t *testing.T) {
	_, out, _ := ask(t, "n\n", nil, repl.Consent{
		Kind:     "write_outside_root",
		Tool:     "write_file",
		Detail:   "../../etc/hosts",
		Resolved: "/etc/hosts",
		Reason:   "writes outside the repo root (/repo) require consent",
	})

	if !strings.Contains(out, "/etc/hosts") {
		t.Errorf("the resolved path was not shown:\n%s", out)
	}
	if !strings.Contains(out, "path: ../../etc/hosts") {
		t.Errorf("the model's own spelling was not shown alongside it:\n%s", out)
	}
}

// TestTheAnswerStringsAreTheJournalsWireValues. The engine writes
// PermissionDecided.Decision from what the surface returns, so the vocabulary
// has to match the one that event documents.
func TestTheAnswerStringsAreTheJournalsWireValues(t *testing.T) {
	for answer, want := range map[repl.Answer]string{
		repl.AnswerDeny:         "deny",
		repl.AnswerAllow:        "allow",
		repl.AnswerAllowSession: "allow_session",
	} {
		if got := answer.String(); got != want {
			t.Errorf("Answer(%d).String() = %q, want %q", uint8(answer), got, want)
		}
	}
}

// TestDenyIsTheZeroAnswer. A caller that drops the value on an error path
// still fails closed, which is the property internal/permission holds for the
// Outcome and this side has to hold for the Answer.
func TestDenyIsTheZeroAnswer(t *testing.T) {
	var zero repl.Answer
	if zero != repl.AnswerDeny {
		t.Errorf("the zero Answer is %v, want %v", zero, repl.AnswerDeny)
	}
}

// TestAskAsksOnce. A loop that re-asked on an unrecognised reply is a loop a
// piped session cannot leave, and re-prompting a human who typed something
// else is how a considered no becomes an accidental yes.
func TestAskAsksOnce(t *testing.T) {
	_, out, _ := ask(t, "what?\n", nil, shellRequest)
	if n := strings.Count(out, "[y]es"); n != 1 {
		t.Errorf("the question was asked %d times, want 1:\n%s", n, out)
	}
}
