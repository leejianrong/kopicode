package aidergen_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/leejianrong/kopicode/internal/aidergen"
	"github.com/leejianrong/kopicode/internal/corpus"
)

// writeExercise lays out a minimal Exercism-shaped exercise under a temp dir and
// returns its path. It mirrors the real polyglot layout closely enough to
// exercise every mapping: a .meta/config.json naming file roles, .docs with an
// appendix, a stub, a test, an editor (cases) file and a go.mod.
func writeGoExercise(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "wordy")
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	cfg := map[string]any{
		"authors":      []string{"soniakeys"},
		"contributors": []string{"bitfield", "kytrinyx"},
		"files": map[string]any{
			"solution":    []string{"wordy.go"},
			"test":        []string{"wordy_test.go"},
			"example":     []string{".meta/example.go"},
			"editor":      []string{"cases_test.go"},
			"invalidator": []string{"go.mod"},
		},
		"blurb":      "Parse and evaluate simple math word problems.",
		"source":     "Extreme Startup",
		"source_url": "https://github.com/rchatley/extreme_startup",
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshalling config: %v", err)
	}
	write(".meta/config.json", string(raw))
	write(".meta/example.go", "package wordy\n\nfunc Answer(q string) (int, bool) { return 42, true }\n")
	write(".meta/gen.go", "package main\n") // .meta content must be excluded from repo/
	write(".docs/instructions.md", "# Wordy\n\nParse the question.\n")
	write(".docs/instructions.append.md", "Extra hint about signs.\n")
	write("wordy.go", "package wordy\n\nfunc Answer(question string) (int, bool) {\n\tpanic(\"implement me\")\n}\n")
	write("wordy_test.go", "package wordy\n")
	write("cases_test.go", "package wordy\n")
	write("go.mod", "module wordy\n\ngo 1.18\n")
	return dir
}

func TestReadExerciseAndManifest(t *testing.T) {
	dir := writeGoExercise(t)

	ex, err := aidergen.ReadExercise(aidergen.GoTrack(), dir)
	if err != nil {
		t.Fatalf("ReadExercise: %v", err)
	}

	if got, want := ex.ID(), "aider-go-wordy"; got != want {
		t.Errorf("ID = %q, want %q", got, want)
	}

	m, err := ex.Manifest("abc1234")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	if m.SchemaVersion != corpus.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", m.SchemaVersion, corpus.SchemaVersion)
	}
	if m.Title != "Parse and evaluate simple math word problems." {
		t.Errorf("Title = %q", m.Title)
	}
	if m.Language != "go" {
		t.Errorf("Language = %q, want go", m.Language)
	}
	if diff := cmp.Diff([]string{"go"}, m.Requires); diff != "" {
		t.Errorf("Requires mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"go", "test", "./..."}, m.Oracle.Argv); diff != "" {
		t.Errorf("Oracle.Argv mismatch (-want +got):\n%s", diff)
	}
	if m.Oracle.Env["GOPROXY"] != "off" || m.Oracle.Env["GOFLAGS"] != "-mod=readonly" {
		t.Errorf("Oracle.Env = %v, want GOPROXY=off GOFLAGS=-mod=readonly", m.Oracle.Env)
	}
	if m.Oracle.TimeoutSeconds != 120 {
		t.Errorf("Oracle.TimeoutSeconds = %d, want 120", m.Oracle.TimeoutSeconds)
	}
	if m.MaxTurns != 20 {
		t.Errorf("MaxTurns = %d, want 20", m.MaxTurns)
	}
	if diff := cmp.Diff([]string{corpus.TraitRequiresRead}, m.Traits); diff != "" {
		t.Errorf("Traits mismatch (-want +got):\n%s", diff)
	}

	// The statement carries the instructions, the appendix, and a run-command
	// trailer — but never the reference solution.
	for _, want := range []string{"Parse the question.", "Extra hint about signs.", "`go test ./...`"} {
		if !strings.Contains(m.Statement, want) {
			t.Errorf("statement missing %q\n%s", want, m.Statement)
		}
	}
	if strings.Contains(m.Statement, "return 42") {
		t.Errorf("statement leaked the reference solution:\n%s", m.Statement)
	}

	// Notes carry provenance and the licence attribution.
	for _, want := range []string{"abc1234", "wordy", "soniakeys", "MIT", "Exercism"} {
		if !strings.Contains(m.Notes, want) {
			t.Errorf("notes missing %q\n%s", want, m.Notes)
		}
	}

	// The manifest must round-trip through the corpus loader's own decoder,
	// which rejects unknown fields — the guard that a re-declared schema has not
	// drifted from the real one.
	blob, err := aidergen.MarshalTask(m)
	if err != nil {
		t.Fatalf("MarshalTask: %v", err)
	}
	assertDecodesUnderCorpusSchema(t, blob)
}

func TestMultiFileTraitFromMultipleSolutions(t *testing.T) {
	dir := writeGoExercise(t)
	// Rewrite config with two solution/example files.
	cfg := map[string]any{
		"files": map[string]any{
			"solution": []string{"a.go", "b.go"},
			"test":     []string{"wordy_test.go"},
			"example":  []string{".meta/example.go", ".meta/example.go"},
		},
		"blurb": "Two-file fix.",
	}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, ".meta", "config.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	ex, err := aidergen.ReadExercise(aidergen.GoTrack(), dir)
	if err != nil {
		t.Fatalf("ReadExercise: %v", err)
	}
	m, err := ex.Manifest("")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if !contains(m.Traits, corpus.TraitMultiFile) {
		t.Errorf("Traits = %v, want to include %q", m.Traits, corpus.TraitMultiFile)
	}
}

func TestPythonOracleNamesModulesNotDiscover(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "affine-cipher")
	write := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := map[string]any{
		"files": map[string]any{
			"solution": []string{"affine_cipher.py"},
			"test":     []string{"affine_cipher_test.py"},
			"example":  []string{".meta/example.py"},
		},
		"blurb": "Affine cipher.",
	}
	raw, _ := json.Marshal(cfg)
	write(".meta/config.json", string(raw))
	write(".meta/example.py", "def encode(): pass\n")
	write(".docs/instructions.md", "Do the cipher.\n")
	write("affine_cipher.py", "def encode(): pass\n")
	write("affine_cipher_test.py", "import unittest\n")

	ex, err := aidergen.ReadExercise(aidergen.PythonTrack(), dir)
	if err != nil {
		t.Fatalf("ReadExercise: %v", err)
	}
	m, err := ex.Manifest("")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	want := []string{"python3", "-m", "unittest", "affine_cipher_test"}
	if diff := cmp.Diff(want, m.Oracle.Argv); diff != "" {
		t.Errorf("Oracle.Argv mismatch (-want +got):\n%s", diff)
	}
	// No argv element may carry a shell metacharacter the corpus validator bans.
	for _, arg := range m.Oracle.Argv {
		if strings.ContainsAny(arg, "*?[]{}|&;<>()$`\\\"'") {
			t.Errorf("argv %q holds a shell metacharacter", arg)
		}
	}
	assertDecodesUnderCorpusSchema(t, mustMarshal(t, m))
}

func TestWriteRepoExcludesMetaAndDocs(t *testing.T) {
	dir := writeGoExercise(t)
	ex, err := aidergen.ReadExercise(aidergen.GoTrack(), dir)
	if err != nil {
		t.Fatalf("ReadExercise: %v", err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := ex.WriteRepo(repoDir); err != nil {
		t.Fatalf("WriteRepo: %v", err)
	}

	got := listFiles(t, repoDir)
	want := []string{"cases_test.go", "go.mod", "wordy.go", "wordy_test.go"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("repo/ contents mismatch (-want +got):\n%s", diff)
	}
}

func TestWriteSolutionMapsExampleOntoSolutionPath(t *testing.T) {
	dir := writeGoExercise(t)
	ex, err := aidergen.ReadExercise(aidergen.GoTrack(), dir)
	if err != nil {
		t.Fatalf("ReadExercise: %v", err)
	}
	solDir := filepath.Join(t.TempDir(), "sol")
	if err := ex.WriteSolution(solDir); err != nil {
		t.Fatalf("WriteSolution: %v", err)
	}
	got := listFiles(t, solDir)
	if diff := cmp.Diff([]string{"wordy.go"}, got); diff != "" {
		t.Errorf("solution overlay files mismatch (-want +got):\n%s", diff)
	}
	content, err := os.ReadFile(filepath.Join(solDir, "wordy.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "return 42, true") {
		t.Errorf("solution wordy.go did not get the example's content:\n%s", content)
	}
}

func TestPristineTestsIncludeTestAndEditor(t *testing.T) {
	dir := writeGoExercise(t)
	ex, err := aidergen.ReadExercise(aidergen.GoTrack(), dir)
	if err != nil {
		t.Fatalf("ReadExercise: %v", err)
	}
	if diff := cmp.Diff([]string{"cases_test.go", "wordy_test.go"}, ex.PristineTestFiles()); diff != "" {
		t.Errorf("PristineTestFiles mismatch (-want +got):\n%s", diff)
	}
	pd := filepath.Join(t.TempDir(), "pristine")
	if err := ex.WritePristineTests(pd); err != nil {
		t.Fatalf("WritePristineTests: %v", err)
	}
	if diff := cmp.Diff([]string{"cases_test.go", "wordy_test.go"}, listFiles(t, pd)); diff != "" {
		t.Errorf("pristine-tests/ contents mismatch (-want +got):\n%s", diff)
	}
}

func TestReadExerciseRejectsMismatchedExampleCount(t *testing.T) {
	dir := writeGoExercise(t)
	cfg := map[string]any{
		"files": map[string]any{
			"solution": []string{"a.go", "b.go"},
			"test":     []string{"wordy_test.go"},
			"example":  []string{".meta/example.go"}, // only one example for two solutions
		},
		"blurb": "x",
	}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, ".meta", "config.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := aidergen.ReadExercise(aidergen.GoTrack(), dir); err == nil {
		t.Fatal("ReadExercise accepted a mismatched example/solution count")
	}
}

// assertDecodesUnderCorpusSchema decodes a rendered task.json the same way
// internal/corpus does — strictly, rejecting unknown fields — so a field this
// package emits that the loader would not accept fails here rather than at load.
func assertDecodesUnderCorpusSchema(t *testing.T, blob []byte) {
	t.Helper()
	var probe struct {
		SchemaVersion int      `json:"schema_version"`
		ID            string   `json:"id"`
		Title         string   `json:"title"`
		Statement     string   `json:"statement"`
		Language      string   `json:"language"`
		Requires      []string `json:"requires"`
		Oracle        struct {
			Argv           []string          `json:"argv"`
			Env            map[string]string `json:"env"`
			TimeoutSeconds int               `json:"timeout_seconds"`
		} `json:"oracle"`
		Traits   []string `json:"traits"`
		MaxTurns int      `json:"max_turns"`
		Notes    string   `json:"notes"`
	}
	dec := json.NewDecoder(strings.NewReader(string(blob)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&probe); err != nil {
		t.Fatalf("task.json does not decode under the corpus schema: %v\n%s", err, blob)
	}
}

func mustMarshal(t *testing.T, m aidergen.TaskManifest) []byte {
	t.Helper()
	b, err := aidergen.MarshalTask(m)
	if err != nil {
		t.Fatalf("MarshalTask: %v", err)
	}
	return b
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("listing %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
