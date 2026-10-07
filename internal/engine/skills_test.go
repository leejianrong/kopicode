package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

func writeSkill(t *testing.T, dir, name, desc, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverSkillsOrdersRootsAndMarksWhatTheModelCanRead(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("KOPICODE_HOME", home)
	t.Setenv("HOME", t.TempDir())

	writeSkill(t, filepath.Join(repo, ".agents", "skills"), "release", "repo release", "r")
	writeSkill(t, filepath.Join(repo, ".kopicode", "skills"), "lint", "repo lint", "l")
	writeSkill(t, filepath.Join(home, "skills"), "release", "user release", "u")
	writeSkill(t, filepath.Join(home, "skills"), "mine", "personal", "m")

	got, warns := engine.DiscoverSkills(repo, nil)
	if len(warns) != 0 || len(got) != 3 {
		t.Fatalf("got %d skills, warnings %v", len(got), warns)
	}
	rel, _ := engine.FindSkill(got, "release")
	if rel.Description != "repo release" || !rel.InRepo {
		t.Errorf("release = %+v, want the repository's, readable by the model", rel)
	}
	if mine, _ := engine.FindSkill(got, "mine"); mine.InRepo {
		t.Error("a user skill was marked readable by the model")
	}

	// A session started in a subdirectory cannot read the repo root's skills.
	sub := filepath.Join(repo, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	fromSub, _ := engine.DiscoverSkills(sub, nil)
	if r, _ := engine.FindSkill(fromSub, "release"); r.InRepo {
		t.Error("skills above the session root were marked readable")
	}
}

func TestSkillPromptInlinesTheBodyAndTheTask(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "deploy", "ship it", "1. Tag.\n2. Push.")
	all, _ := engine.DiscoverSkills(t.TempDir(), []string{dir})
	sk, ok := engine.FindSkill(all, "deploy")
	if !ok {
		t.Fatal("deploy not found through skills_paths")
	}
	p, err := engine.SkillPrompt(sk, " to staging ")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`Follow the skill "deploy"`, "1. Tag.\n2. Push.", "Task: to staging"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	bare, _ := engine.SkillPrompt(sk, "")
	if !strings.Contains(bare, "without further instructions") || strings.Contains(bare, "Task:") {
		t.Errorf("a bare invocation says the wrong thing:\n%s", bare)
	}
}

func TestSkillCatalogueIsJournalledOnceAndSeenByTheModel(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, ".agents", "skills"), "release", "cut a release", "x")
	t.Setenv("KOPICODE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	all, _ := engine.DiscoverSkills(dir, nil)

	prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
	s, err := engine.Open(context.Background(), engine.Options{
		Dir: dir, Selection: resumeSelection(t), SessionID: "skills", Provider: prov, Now: fixedClock(), Skills: all,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close(context.Background())

	loaded := payloadsOf[journal.ProjectInstructionsLoaded](t, readJournal(t, s.Path()))
	if len(loaded) != 1 || loaded[0].Scope != journal.InstructionsScopeSkills {
		t.Fatalf("loaded = %+v, want one skills-scope event", loaded)
	}
	idx := indexOfContent(prov.requests[0].Messages, "- release: cut a release (.agents/skills/release/SKILL.md)")
	if idx < 0 {
		t.Fatalf("the model was not shown the catalogue: %+v", prov.requests[0].Messages)
	}
}

func TestNoSkillsMeansNothingIsSaid(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
	s, err := engine.Open(context.Background(), engine.Options{
		Dir: dir, Selection: resumeSelection(t), SessionID: "noskills", Provider: prov, Now: fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.Run(context.Background(), "hi")
	_ = s.Close(context.Background())
	if n := len(payloadsOf[journal.ProjectInstructionsLoaded](t, readJournal(t, s.Path()))); n != 0 {
		t.Errorf("%d instruction events for a session with nothing to say", n)
	}
}
