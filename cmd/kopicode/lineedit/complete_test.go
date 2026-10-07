package lineedit_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/lineedit"
)

func completing(input string, words ...string) (*lineedit.Editor, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return lineedit.New(lineedit.Config{
		In: strings.NewReader(input), Out: out, Terminal: &fakeTerminal{interactive: true}, Prompt: "> ",
		Complete: func(line string) []string {
			var c []string
			for _, w := range words {
				if strings.HasPrefix(w, line) {
					c = append(c, w)
				}
			}
			return c
		},
	}), out
}

func TestTabCompletesAUniqueMatch(t *testing.T) {
	e, _ := completing("/rel\t"+enter, "/release ", "/context")
	if got := readOne(t, e); got != "/release " {
		t.Errorf("got %q", got)
	}
}

func TestTabExtendsToTheCommonPrefixThenListsTheRest(t *testing.T) {
	e, _ := completing("/d\t"+enter, "/deploy-prod", "/deploy-stage")
	if got := readOne(t, e); got != "/deploy-" {
		t.Errorf("line = %q, want the shared prefix", got)
	}
	e, out := completing("/deploy-\t"+enter, "/deploy-prod", "/deploy-stage")
	if got := readOne(t, e); got != "/deploy-" {
		t.Errorf("line = %q, want it left alone when nothing more is shared", got)
	}
	if !strings.Contains(out.String(), "/deploy-prod  /deploy-stage") {
		t.Errorf("the candidates were not listed:\n%q", out.String())
	}
}

func TestTabWithNoMatchOrNoCompleterDoesNothing(t *testing.T) {
	e, _ := completing("/zz\t"+enter, "/release")
	if got := readOne(t, e); got != "/zz" {
		t.Errorf("got %q", got)
	}
	e, _, _ = interactiveEditor("ab\tc" + enter)
	if got := readOne(t, e); got != "abc" {
		t.Errorf("a tab with no completer inserted or lost text: %q", got)
	}
}

func TestTabMidLineDoesNotComplete(t *testing.T) {
	e, _ := completing("/rel\x1b[D\t"+enter, "/release ")
	if got := readOne(t, e); got != "/rel" {
		t.Errorf("got %q; completion is for the end of the line only", got)
	}
}
