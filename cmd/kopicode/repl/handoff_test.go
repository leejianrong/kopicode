package repl_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/repl"
	"github.com/leejianrong/kopicode/internal/engine"
)

// handoffRig records what /handoff asked of the session.
type handoffRig struct {
	goals    []string
	edited   []string
	started  []string
	turns    []string
	draftErr error
	editErr  error
	startErr error
	onDisk   string
}

func (r *handoffRig) control(withEditor bool) repl.HandoffControl {
	c := repl.HandoffControl{
		Draft: func(_ context.Context, goal string) (repl.HandoffDraft, error) {
			r.goals = append(r.goals, goal)
			if r.draftErr != nil {
				return repl.HandoffDraft{}, r.draftErr
			}
			return repl.HandoffDraft{Document: "# Handoff\n\n## Goal\n\nship it\n", Path: "/work/.kopicode/handoff/s1.md"}, nil
		},
		Read: func(string) (string, error) { return r.onDisk, nil },
		Start: func(_ context.Context, doc string) (string, error) {
			r.started = append(r.started, doc)
			return "s2", r.startErr
		},
	}
	if withEditor {
		c.Edit = func(path string) error {
			r.edited = append(r.edited, path)
			r.onDisk += " (edited)"
			return r.editErr
		}
	}
	return c
}

func (r *handoffRig) run(t *testing.T, input string, ctl repl.HandoffControl) (string, error) {
	t.Helper()
	var w strings.Builder
	loop, err := repl.New(repl.Config{
		In: strings.NewReader(input), Out: &w, Handoff: ctl,
		Turn: func(_ context.Context, p string, _ repl.Surface) (engine.Result, error) {
			r.turns = append(r.turns, p)
			return engine.Result{Stop: engine.StopCompleted}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := loop.Run(context.Background())
	return w.String(), runErr
}

func TestHandoffDraftsShowsEditsAndStartsFromTheEditedFile(t *testing.T) {
	r := &handoffRig{onDisk: "the document"}
	out, err := r.run(t, "/handoff Finish The Parser\ny\n", r.control(true))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.goals) != 1 || r.goals[0] != "Finish The Parser" {
		t.Errorf("goal = %q, want the person's words with their case", r.goals)
	}
	if len(r.edited) != 1 || len(r.started) != 1 || r.started[0] != "the document (edited)" {
		t.Errorf("edited %v, started %q: the session must start from the file as edited", r.edited, r.started)
	}
	for _, want := range []string{"## Goal", "ship it", "/work/.kopicode/handoff/s1.md", "new session s2 started from the handoff"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(r.turns) != 0 {
		t.Errorf("/handoff reached the model as a prompt: %q", r.turns)
	}
}

func TestHandoffStaysPutOnAnythingButAnExactYes(t *testing.T) {
	for _, answer := range []string{"\n", "n\n", "yes please\n", ""} {
		r := &handoffRig{onDisk: "doc"}
		out, err := r.run(t, "/handoff\n"+answer, r.control(false))
		if err != nil {
			t.Fatalf("%q: %v", answer, err)
		}
		if len(r.started) != 0 {
			t.Errorf("answer %q started a session", answer)
		}
		if !strings.Contains(out, "staying in this session") {
			t.Errorf("answer %q: output lacks the staying line:\n%s", answer, out)
		}
	}
}

func TestHandoffWithoutAnEditorSaysToEditTheFile(t *testing.T) {
	r := &handoffRig{onDisk: "doc"}
	out, _ := r.run(t, "/handoff\nn\n", r.control(false))
	if !strings.Contains(out, "$VISUAL or $EDITOR") {
		t.Errorf("output does not say how to get an editor:\n%s", out)
	}
}

func TestHandoffRefusalsLeaveTheSessionAlone(t *testing.T) {
	t.Run("nothing to hand off", func(t *testing.T) {
		r := &handoffRig{draftErr: engine.ErrNothingToHand}
		out, err := r.run(t, "/handoff\n", r.control(true))
		if err != nil || len(r.started) != 0 || !strings.Contains(out, "has not run a turn") {
			t.Fatalf("err=%v started=%v out=%s", err, r.started, out)
		}
	})
	t.Run("a failed draft", func(t *testing.T) {
		r := &handoffRig{draftErr: errors.New("provider down")}
		out, err := r.run(t, "/handoff\n", r.control(true))
		if err != nil || len(r.started) != 0 || !strings.Contains(out, "provider down") {
			t.Fatalf("err=%v started=%v out=%s", err, r.started, out)
		}
	})
	t.Run("an editor that fails", func(t *testing.T) {
		r := &handoffRig{onDisk: "doc", editErr: errors.New("exit status 1")}
		out, err := r.run(t, "/handoff\ny\n", r.control(true))
		if err != nil || len(r.started) != 0 || !strings.Contains(out, "nothing started") {
			t.Fatalf("err=%v started=%v out=%s", err, r.started, out)
		}
	})
	t.Run("an emptied file", func(t *testing.T) {
		r := &handoffRig{onDisk: "  \n"}
		out, err := r.run(t, "/handoff\ny\n", r.control(false))
		if err != nil || len(r.started) != 0 || !strings.Contains(out, "is empty") {
			t.Fatalf("err=%v started=%v out=%s", err, r.started, out)
		}
	})
}

func TestHandoffThatCannotOpenTheNewSessionEndsTheLoopAndNamesTheFile(t *testing.T) {
	r := &handoffRig{onDisk: "doc", startErr: errors.New("locked")}
	out, err := r.run(t, "/handoff\ny\n/anything\n", r.control(false))
	if err == nil {
		t.Fatal("the loop went on with no session running")
	}
	if !strings.Contains(out, "/work/.kopicode/handoff/s1.md") || len(r.turns) != 0 {
		t.Errorf("out=%s turns=%v", out, r.turns)
	}
}

func TestHandoffIsUnavailableWithoutAControl(t *testing.T) {
	r := &handoffRig{}
	out, err := r.run(t, "/handoff\n", repl.HandoffControl{})
	if err != nil || !strings.Contains(out, "/handoff is not available") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}
