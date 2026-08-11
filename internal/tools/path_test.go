package tools_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/tools"
)

// --- paths that are inside ------------------------------------------------

func TestResolveAccepts(t *testing.T) {
	f := newFixture(t, map[string]string{
		"a.txt":     "a\n",
		"sub/b.txt": "b\n",
	})
	s := f.set(t)

	cases := []struct {
		name    string
		given   string
		wantRel string
	}{
		{"relative file", "a.txt", "a.txt"},
		{"nested file", "sub/b.txt", filepath.Join("sub", "b.txt")},
		{"the root itself", ".", "."},
		{"empty means the root", "", "."},
		{"absolute inside", filepath.Join(f.root, "sub", "b.txt"), filepath.Join("sub", "b.txt")},
		{"dot-dot that stays inside", "sub/../a.txt", "a.txt"},
		{"a file that does not exist yet", "sub/new.txt", filepath.Join("sub", "new.txt")},
		{"a file under a directory that does not exist yet", "x/y/new.txt", filepath.Join("x", "y", "new.txt")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := s.Root.Resolve(tools.ToolReadFile, tc.given)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.given, err)
			}
			if p.Rel != tc.wantRel {
				t.Errorf("Rel = %q, want %q", p.Rel, tc.wantRel)
			}
			if p.Given != tc.given {
				t.Errorf("Given = %q, want %q", p.Given, tc.given)
			}
		})
	}
}

// TestResolveFollowsLinksInsideTheRoot keeps the guard from being a blunt "no
// symlinks" rule. A relative link that stays inside is ordinary in a repository
// and must work, or the escape test below is passing for the wrong reason.
func TestResolveFollowsLinksInsideTheRoot(t *testing.T) {
	f := newFixture(t, map[string]string{"sub/b.txt": "b\n"})
	f.symlink(t, "sub", "link")
	s := f.set(t)

	p, err := s.Root.Resolve(tools.ToolReadFile, "link/b.txt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join("sub", "b.txt"); p.Rel != want {
		t.Errorf("Rel = %q, want %q", p.Rel, want)
	}

	out, err := s.ReadFile(context.Background(), tools.ReadRequest{Path: "link/b.txt"})
	if err != nil {
		t.Fatalf("ReadFile through an in-root link: %v", err)
	}
	if got := lines(out)[0]; got != "sub/b.txt: 1 line, 2 bytes" {
		t.Errorf("header = %q", got)
	}
}

// --- paths that are outside -----------------------------------------------

// TestResolveRejectsEscapes is one half of SLICE-1's Test Plan line "path
// resolution rejects ../ escapes and symlinks pointing outside the repo root".
// Both halves are here, and both were confirmed red before being confirmed
// green — see the PR.
func TestResolveRejectsEscapes(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	f.symlink(t, f.outside, "escape")          // absolute link out
	f.symlink(t, "../outside", "escape-rel")   // relative link out
	f.symlink(t, "../outside/secret.txt", "s") // relative link to a file out
	s := f.set(t)

	cases := []struct {
		name  string
		given string
	}{
		{"dot-dot", "../outside/secret.txt"},
		{"dot-dot to the parent itself", ".."},
		{"a deep pile of dot-dots", "../../../../../../etc/passwd"},
		{"absolute outside", filepath.Join(f.outside, "secret.txt")},
		{"absolute far outside", filepath.FromSlash("/etc/passwd")},
		{"a symlink to a directory outside", "escape/secret.txt"},
		{"a relative symlink to a directory outside", "escape-rel/secret.txt"},
		{"a symlink to a file outside", "s"},
		// The lexical trap: cleaning this string before following the link
		// gives repo/outside/secret.txt, which is inside. Following the link
		// first gives the sibling directory, which is not.
		{"dot-dot after a symlink", "escape/../outside/secret.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Root.Resolve(tools.ToolReadFile, tc.given)
			if !errors.Is(err, tools.ErrOutsideRoot) {
				t.Fatalf("Resolve(%q) = %v, want ErrOutsideRoot", tc.given, err)
			}
			wantFault(t, err, tools.FaultTask)

			var te *tools.Error
			if !errors.As(err, &te) {
				t.Fatalf("error is not a *tools.Error: %v", err)
			}
			if te.Tool != tools.ToolReadFile {
				t.Errorf("Tool = %q, want %q", te.Tool, tools.ToolReadFile)
			}
			if te.Path != tc.given {
				t.Errorf("Path = %q, want %q", te.Path, tc.given)
			}
			// The permission gate has to be able to name what it is being
			// asked about, so the refusal carries where the path actually
			// went, not only where it was written.
			if te.Resolved == "" {
				t.Error("Resolved is empty; a permission prompt has nothing to name")
			}
		})
	}
}

// TestEscapeMessageNamesTheSymlink checks the refusal is actionable. A model
// that wrote a plausible relative path cannot tell why it was refused unless
// the message says a link took it out.
func TestEscapeMessageNamesTheSymlink(t *testing.T) {
	f := newFixture(t, nil)
	f.symlink(t, f.outside, "escape")
	s := f.set(t)

	_, err := s.Root.Resolve(tools.ToolReadFile, "escape/secret.txt")
	var te *tools.Error
	if !errors.As(err, &te) {
		t.Fatalf("want *tools.Error, got %v", err)
	}
	if want := "a symbolic link in the path leads outside"; !strings.Contains(te.Detail, want) {
		t.Errorf("Detail = %q, want it to mention %q", te.Detail, want)
	}
}

// TestToolsRefuseEscapes proves the guard is reached through every tool, not
// only through Resolve. A helper nothing calls is not a guard.
func TestToolsRefuseEscapes(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	f.symlink(t, f.outside, "escape")
	s := f.set(t)
	ctx := context.Background()

	for _, given := range []string{"../outside", "escape"} {
		t.Run("read_file "+given, func(t *testing.T) {
			_, err := s.ReadFile(ctx, tools.ReadRequest{Path: given + "/secret.txt"})
			if !errors.Is(err, tools.ErrOutsideRoot) {
				t.Fatalf("got %v, want ErrOutsideRoot", err)
			}
		})
		t.Run("list_dir "+given, func(t *testing.T) {
			_, err := s.ListDir(ctx, tools.ListRequest{Path: given})
			if !errors.Is(err, tools.ErrOutsideRoot) {
				t.Fatalf("got %v, want ErrOutsideRoot", err)
			}
		})
		t.Run("grep "+given, func(t *testing.T) {
			_, err := s.Grep(ctx, tools.GrepRequest{Pattern: "secret", Path: given})
			if !errors.Is(err, tools.ErrOutsideRoot) {
				t.Fatalf("got %v, want ErrOutsideRoot", err)
			}
		})
	}
}

// TestWalksDoNotFollowLinksOutOfTheRoot covers the escape that never passes
// through Resolve at all: a link discovered *during* a walk. list_dir and grep
// both enumerate paths the caller never named.
func TestWalksDoNotFollowLinksOutOfTheRoot(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "inside\n"})
	f.symlink(t, f.outside, "escape")
	s := f.set(t)
	ctx := context.Background()

	out, err := s.ListDir(ctx, tools.ListRequest{Recursive: true})
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if strings.Contains(out, "secret.txt") {
		t.Errorf("recursive listing walked through the link:\n%s", out)
	}
	if !strings.Contains(out, "escape@") {
		t.Errorf("the link itself should be listed, marked as a link:\n%s", out)
	}

	got, err := s.Grep(ctx, tools.GrepRequest{Pattern: "secret"})
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if strings.Contains(got, "secret.txt") {
		t.Errorf("grep searched through the link:\n%s", got)
	}
}

// --- classification -------------------------------------------------------

func TestFaultOf(t *testing.T) {
	if got := tools.FaultOf(nil); got != tools.FaultNone {
		t.Errorf("FaultOf(nil) = %q, want none", got)
	}
	if got := tools.FaultOf(errors.New("who knows")); got != tools.FaultInternal {
		t.Errorf("FaultOf(unclassified) = %q, want internal — an unclassified error "+
			"must not be credited to the model", got)
	}
	if got, want := tools.FaultNone.String(), ""; got != want {
		t.Errorf("FaultNone.String() = %q, want %q — it is ToolResult.ErrorKind's zero", got, want)
	}
	if got, want := tools.FaultTask.String(), "task"; got != want {
		t.Errorf("FaultTask.String() = %q, want %q", got, want)
	}
	if got, want := tools.FaultInternal.String(), "internal"; got != want {
		t.Errorf("FaultInternal.String() = %q, want %q", got, want)
	}
}
