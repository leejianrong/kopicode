package skills_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/skills"
)

func put(t *testing.T, root, dir, content string) {
	t.Helper()
	p := filepath.Join(root, dir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, skills.FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverReadsFrontmatterForms(t *testing.T) {
	root := t.TempDir()
	put(t, root, "plain", "---\nname: plain\ndescription: Do the plain thing\n---\nBody here.\n")
	put(t, root, "quoted", "---\nname: \"quoted\"\ndescription: 'Quoted: with colon'\n---\nx")
	put(t, root, "folded", "---\nname: folded\ndescription: >\n  Spread over\n  two lines\n---\nx")
	put(t, root, "noname", "---\ndescription: Name comes from the directory\n---\nx")
	put(t, root, "crlf", "---\r\nname: crlf\r\ndescription: Windows line endings\r\n---\r\nbody")

	got, warns := skills.Discover([]skills.Root{{Dir: root, Label: "repo", InRepo: true}})
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	want := map[string]string{
		"plain": "Do the plain thing", "quoted": "Quoted: with colon", "folded": "Spread over two lines",
		"noname": "Name comes from the directory", "crlf": "Windows line endings",
	}
	if len(got) != len(want) {
		t.Fatalf("found %d skills, want %d: %+v", len(got), len(want), got)
	}
	for _, s := range got {
		if want[s.Name] != s.Description {
			t.Errorf("%s: description %q, want %q", s.Name, s.Description, want[s.Name])
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Name > got[i].Name {
			t.Error("not sorted by name")
		}
	}
}

func TestEarlierRootShadowsLater(t *testing.T) {
	repo, user := t.TempDir(), t.TempDir()
	put(t, repo, "deploy", "---\nname: deploy\ndescription: the repo's way\n---\nrepo")
	put(t, user, "deploy", "---\nname: deploy\ndescription: my way\n---\nuser")
	put(t, user, "mine", "---\nname: mine\ndescription: only mine\n---\nx")
	got, _ := skills.Discover([]skills.Root{{Dir: repo, Label: "repo", InRepo: true}, {Dir: user, Label: "user"}})
	d, _ := skills.Find(got, "deploy")
	if d.Description != "the repo's way" || d.Label != "repo" {
		t.Errorf("deploy = %+v, want the repository's to win", d)
	}
	if _, ok := skills.Find(got, "mine"); !ok {
		t.Error("a user skill with a new name was lost")
	}
}

func TestBadSkillsAreWarnedAboutNotDropped(t *testing.T) {
	root := t.TempDir()
	put(t, root, "nodesc", "---\nname: nodesc\n---\nx")
	put(t, root, "badname", "---\nname: Bad Name\ndescription: d\n---\nx")
	put(t, root, "unclosed", "---\nname: unclosed\ndescription: d\nx")
	put(t, root, "good", "---\nname: good\ndescription: d\n---\nx")
	if err := os.MkdirAll(filepath.Join(root, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, warns := skills.Discover([]skills.Root{{Dir: root}, {Dir: filepath.Join(root, "missing")}})
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("got %+v", got)
	}
	if len(warns) != 3 {
		t.Fatalf("warnings = %v, want one each for nodesc, badname, unclosed (and none for a directory with no SKILL.md or a missing root)", warns)
	}
}

func TestBodyDropsFrontmatter(t *testing.T) {
	root := t.TempDir()
	put(t, root, "a", "---\nname: a\ndescription: d\n---\n\n# Steps\n1. do it\n")
	got, _ := skills.Discover([]skills.Root{{Dir: root}})
	body, err := skills.Body(got[0])
	if err != nil || body != "# Steps\n1. do it" {
		t.Errorf("body = %q, %v", body, err)
	}
}

func TestOversizeSkillIsRefused(t *testing.T) {
	root := t.TempDir()
	put(t, root, "big", "---\nname: big\ndescription: d\n---\n"+strings.Repeat("x", 300<<10))
	got, warns := skills.Discover([]skills.Root{{Dir: root}})
	if len(got) != 0 || len(warns) != 1 {
		t.Errorf("got %v, warnings %v", got, warns)
	}
}

func TestCatalogueListsOnlyWhatTheModelCanRead(t *testing.T) {
	root := t.TempDir()
	repoSkills, user := filepath.Join(root, ".agents", "skills"), t.TempDir()
	put(t, repoSkills, "release", "---\nname: release\ndescription: cut a release\n---\nx")
	put(t, user, "private", "---\nname: private\ndescription: personal\n---\nx")
	all, _ := skills.Discover([]skills.Root{{Dir: repoSkills, InRepo: true}, {Dir: user}})
	cat := skills.Catalogue(all, root)
	if !strings.Contains(cat, "- release: cut a release (.agents/skills/release/SKILL.md)") {
		t.Errorf("catalogue = %q", cat)
	}
	if strings.Contains(cat, "private") {
		t.Error("a skill the model cannot read was listed")
	}
	if skills.Catalogue(all[:0], root) != "" {
		t.Error("an empty set must say nothing")
	}
}

func TestValidName(t *testing.T) {
	for s, want := range map[string]bool{"a": true, "go-test_2": true, "": false, "Up": false, "has space": false, "-x": false, "a/b": false} {
		if skills.ValidName(s) != want {
			t.Errorf("ValidName(%q) != %v", s, want)
		}
	}
}
