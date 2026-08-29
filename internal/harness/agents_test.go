package harness_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

// writeAgentsFile puts an AGENTS.md at dir's root and returns dir.
func writeAgentsFile(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, harness.AgentsFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", harness.AgentsFileName, err)
	}
	return dir
}

// TestNoAgentsFileIsNotAnError. Most repositories will never have one, and the
// absence is the ordinary case KAN-1024's design settles on.
func TestNoAgentsFileIsNotAnError(t *testing.T) {
	got, ok, err := harness.LoadAgentsFile(t.TempDir())
	if err != nil {
		t.Fatalf("LoadAgentsFile on a directory with no AGENTS.md: %v", err)
	}
	if ok {
		t.Errorf("ok = true for an absent AGENTS.md, got %+v", got)
	}
	if got != (harness.AgentsFile{}) {
		t.Errorf("absent AGENTS.md produced %+v, want the zero value", got)
	}
}

// TestAgentsFileIsRead is the plain case: a file at the root is found and its
// content comes back verbatim.
func TestAgentsFileIsRead(t *testing.T) {
	dir := writeAgentsFile(t, t.TempDir(), "Run `make test` before every commit.\n")

	got, ok, err := harness.LoadAgentsFile(dir)
	if err != nil {
		t.Fatalf("LoadAgentsFile: %v", err)
	}
	if !ok {
		t.Fatal("ok = false for a present AGENTS.md")
	}
	if got.Content != "Run `make test` before every commit.\n" {
		t.Errorf("content = %q, want the file's own content", got.Content)
	}
	if got.Path != filepath.Join(dir, harness.AgentsFileName) {
		t.Errorf("path = %q, want %q", got.Path, filepath.Join(dir, harness.AgentsFileName))
	}
}

// TestAgentsFileIsFoundFromASubdirectory holds the shared walk to account:
// LoadAgentsFile finds the root's AGENTS.md the same way LoadFileConfig finds
// the root's config.toml, from several directories down.
func TestAgentsFileIsFoundFromASubdirectory(t *testing.T) {
	root := writeAgentsFile(t, t.TempDir(), "project instructions\n")
	deep := filepath.Join(root, "internal", "engine")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("creating %s: %v", deep, err)
	}

	got, ok, err := harness.LoadAgentsFile(deep)
	if err != nil {
		t.Fatalf("LoadAgentsFile: %v", err)
	}
	if !ok || got.Content != "project instructions\n" {
		t.Errorf("LoadAgentsFile from a subdirectory = (%+v, %v), want the root's own content", got, ok)
	}
}

// TestAgentsFileSearchStopsAtTheRepositoryBoundary mirrors
// TestTheSearchStopsAtTheRepositoryBoundary: a checkout nested inside another
// project must not inherit the outer project's AGENTS.md.
func TestAgentsFileSearchStopsAtTheRepositoryBoundary(t *testing.T) {
	outer := writeAgentsFile(t, t.TempDir(), "outer instructions\n")

	inner := filepath.Join(outer, "vendored")
	if err := os.MkdirAll(filepath.Join(inner, ".git"), 0o755); err != nil {
		t.Fatalf("creating the inner repository: %v", err)
	}

	got, ok, err := harness.LoadAgentsFile(inner)
	if err != nil {
		t.Fatalf("LoadAgentsFile: %v", err)
	}
	if ok {
		t.Errorf("the search escaped the inner repository and read %+v", got)
	}
}
