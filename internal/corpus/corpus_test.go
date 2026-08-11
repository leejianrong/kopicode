package corpus_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/corpus"
)

// corpusRoot is the real corpus, resolved from this package's directory.
func corpusRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "bench", "tasks"))
	if err != nil {
		t.Fatalf("resolving the corpus root: %v", err)
	}
	return root
}

func loadReal(t *testing.T) *corpus.Corpus {
	t.Helper()
	c, err := corpus.Load(corpusRoot(t))
	if err != nil {
		t.Fatalf("loading the corpus: %v", err)
	}
	return c
}

// TestLoadRealCorpusWalksEveryTaskDirectory is the guard against a validation
// suite that reports green having checked nothing.
//
// It enumerates the task directories itself, with plain os.ReadDir, and
// insists the loader returned exactly those. A loader that walked the wrong
// directory, stopped early, or quietly skipped a task it could not parse fails
// here rather than making the rest of this file vacuous.
func TestLoadRealCorpusWalksEveryTaskDirectory(t *testing.T) {
	root := corpusRoot(t)

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	var onDisk []string
	for _, e := range entries {
		if e.IsDir() {
			onDisk = append(onDisk, e.Name())
		}
	}
	sort.Strings(onDisk)

	if len(onDisk) < corpus.MinTasks {
		t.Fatalf("found %d task directories under %s, want at least %d: "+
			"this test is meaningless if the corpus is not there",
			len(onDisk), root, corpus.MinTasks)
	}

	c := loadReal(t)

	var loaded []string
	for _, task := range c.Tasks {
		loaded = append(loaded, task.ID)
	}
	sort.Strings(loaded)

	if strings.Join(loaded, ",") != strings.Join(onDisk, ",") {
		t.Errorf("loader walked a different set of tasks than the directory holds\n"+
			"  on disk: %v\n"+
			"  loaded:  %v", onDisk, loaded)
	}
}

// TestRealCorpusRunOrderIsTheManifestOrder pins the property ADR-0005 §4's
// sequential early stopping depends on: tasks run in a fixed, recorded order,
// not in whatever order the filesystem hands them over.
func TestRealCorpusRunOrderIsTheManifestOrder(t *testing.T) {
	root := corpusRoot(t)

	b, err := os.ReadFile(filepath.Join(root, corpus.ManifestName))
	if err != nil {
		t.Fatalf("reading %s: %v", corpus.ManifestName, err)
	}
	var manifest struct {
		Tasks []string `json:"tasks"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatalf("parsing %s: %v", corpus.ManifestName, err)
	}

	c := loadReal(t)
	if len(c.Tasks) != len(manifest.Tasks) {
		t.Fatalf("loaded %d tasks, manifest lists %d", len(c.Tasks), len(manifest.Tasks))
	}
	for i, want := range manifest.Tasks {
		if c.Tasks[i].ID != want {
			t.Errorf("task %d is %q, manifest says %q", i, c.Tasks[i].ID, want)
		}
	}
}

// TestRealCorpusComposition checks the constraints the corpus card set: two
// tasks that cannot be done without reading first, and one that needs a
// coordinated change across files.
func TestRealCorpusComposition(t *testing.T) {
	c := loadReal(t)

	var reads, multi []string
	for _, task := range c.Tasks {
		if task.HasTrait(corpus.TraitRequiresRead) {
			reads = append(reads, task.ID)
		}
		if task.HasTrait(corpus.TraitMultiFile) {
			multi = append(multi, task.ID)
		}
	}

	if len(reads) < 2 {
		t.Errorf("%d tasks marked %s (%v), want at least 2",
			len(reads), corpus.TraitRequiresRead, reads)
	}
	if len(multi) < 1 {
		t.Errorf("%d tasks marked %s (%v), want at least 1",
			len(multi), corpus.TraitMultiFile, multi)
	}
	t.Logf("%d tasks: %d requires_read %v, %d multi_file %v",
		len(c.Tasks), len(reads), reads, len(multi), multi)
}

// TestRealCorpusOraclesAreOffline asserts the determinism rules that a task
// manifest can carry: a Go oracle runs with the module proxy switched off, and
// a Python oracle does not write bytecode into the tree it is judging.
//
// Both are the difference between "this task happened not to use the network"
// and "this task cannot".
func TestRealCorpusOraclesAreOffline(t *testing.T) {
	for _, task := range loadReal(t).Tasks {
		switch task.Language {
		case "go":
			if got := task.Oracle.Env["GOPROXY"]; got != "off" {
				t.Errorf("%s: GOPROXY is %q, want \"off\" so the oracle cannot reach the network",
					task.ID, got)
			}
			if got := task.Oracle.Env["GOFLAGS"]; !strings.Contains(got, "-mod=readonly") {
				t.Errorf("%s: GOFLAGS is %q, want it to contain -mod=readonly", task.ID, got)
			}
		case "python":
			if got := task.Oracle.Env["PYTHONDONTWRITEBYTECODE"]; got != "1" {
				t.Errorf("%s: PYTHONDONTWRITEBYTECODE is %q, want \"1\" so running the "+
					"oracle does not modify the tree", task.ID, got)
			}
		default:
			t.Errorf("%s: unhandled language %q — add its determinism rule here",
				task.ID, task.Language)
		}
	}
}

// TestEveryTaskHasAReferenceSolution checks the other half of the corpus: the
// overlay that turns a starting tree into a passing one. Without it nothing
// can re-verify that an oracle fails before the fix and passes after, and an
// oracle nobody has checked in both directions may be measuring nothing.
//
// The file count also cross-checks the multi_file trait mechanically, in both
// directions, so a task cannot claim to be multi-file by editing its manifest.
func TestEveryTaskHasAReferenceSolution(t *testing.T) {
	c := loadReal(t)

	for _, task := range c.Tasks {
		dir := corpus.SolutionDir(c.Root, task.ID)

		files, err := relativeFiles(dir)
		if err != nil {
			t.Errorf("%s: reading its reference solution: %v", task.ID, err)
			continue
		}
		if len(files) == 0 {
			t.Errorf("%s: reference solution %s is empty", task.ID, dir)
			continue
		}

		for _, rel := range files {
			target := filepath.Join(task.RepoDir(), filepath.FromSlash(rel))
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s: solution file %q does not overlay anything in %s/: %v",
					task.ID, rel, corpus.RepoDirName, err)
			}
		}

		multi := task.HasTrait(corpus.TraitMultiFile)
		switch {
		case multi && len(files) < 2:
			t.Errorf("%s claims the %s trait but its solution touches %d file(s): %v",
				task.ID, corpus.TraitMultiFile, len(files), files)
		case !multi && len(files) > 1:
			t.Errorf("%s touches %d files (%v) but does not claim the %s trait",
				task.ID, len(files), files, corpus.TraitMultiFile)
		}
	}
}

// TestDigestIsStableAndContentSensitive covers what the frozen-corpus promise
// rests on: the same bytes give the same digest, and different bytes do not.
func TestDigestIsStableAndContentSensitive(t *testing.T) {
	dir := t.TempDir()
	writeCorpus(t, dir, corpus.MinTasks, nil)

	first, err := corpus.Digest(dir)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	second, err := corpus.Digest(dir)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if first != second {
		t.Fatalf("Digest is not stable: %s then %s", first, second)
	}
	if !strings.HasPrefix(first, "sha256:") {
		t.Errorf("Digest = %q, want a sha256: prefix", first)
	}

	writeFile(t, filepath.Join(dir, "task-00", "repo", "main.txt"), "changed\n")
	changed, err := corpus.Digest(dir)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if changed == first {
		t.Error("editing a task file did not change the digest, so the corpus is not frozen by it")
	}
}

// TestDigestIgnoresDocumentation keeps the freeze on the tasks rather than on
// the prose around them: fixing a typo in a README must not invalidate an
// experiment series.
func TestDigestIgnoresDocumentation(t *testing.T) {
	dir := t.TempDir()
	writeCorpus(t, dir, corpus.MinTasks, nil)

	before, err := corpus.Digest(dir)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}

	writeFile(t, filepath.Join(dir, "README.md"), "# notes\n")
	after, err := corpus.Digest(dir)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if before != after {
		t.Errorf("adding a README changed the digest: %s then %s", before, after)
	}
}

// TestLoadRejects is the validation guard, kept red on purpose one mutation at
// a time. Each case starts from a corpus that loads cleanly and breaks exactly
// one thing, so a rule that stops working shows up as a case that no longer
// fails rather than as a silently weaker check.
func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, dir string)
		wantErr string
	}{
		{
			name: "task directory nobody listed",
			mutate: func(t *testing.T, dir string) {
				writeTask(t, dir, "task-99", nil)
			},
			wantErr: "is not listed",
		},
		{
			name: "listed task with no directory",
			mutate: func(t *testing.T, dir string) {
				if err := os.RemoveAll(filepath.Join(dir, "task-00")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "there is no such directory",
		},
		{
			name: "id does not match its directory",
			mutate: func(t *testing.T, dir string) {
				editTask(t, dir, "task-01", func(m map[string]any) { m["id"] = "task-99" })
			},
			wantErr: "does not match its directory",
		},
		{
			name: "empty statement",
			mutate: func(t *testing.T, dir string) {
				editTask(t, dir, "task-02", func(m map[string]any) { m["statement"] = "  " })
			},
			wantErr: "statement is empty",
		},
		{
			name: "unknown language",
			mutate: func(t *testing.T, dir string) {
				editTask(t, dir, "task-03", func(m map[string]any) { m["language"] = "cobol" })
			},
			wantErr: "language \"cobol\"",
		},
		{
			name: "oracle needs something requires does not declare",
			mutate: func(t *testing.T, dir string) {
				editTask(t, dir, "task-04", func(m map[string]any) {
					oracle := m["oracle"].(map[string]any)
					oracle["argv"] = []any{"cargo", "test"}
				})
			},
			wantErr: "requires does not list it",
		},
		{
			name: "oracle smuggles in a shell",
			mutate: func(t *testing.T, dir string) {
				editTask(t, dir, "task-05", func(m map[string]any) {
					oracle := m["oracle"].(map[string]any)
					oracle["argv"] = []any{"go", "test", "./...", "&&", "true"}
				})
			},
			wantErr: "shell metacharacter",
		},
		{
			name: "turn budget over the cap",
			mutate: func(t *testing.T, dir string) {
				editTask(t, dir, "task-06", func(m map[string]any) { m["max_turns"] = 40 })
			},
			wantErr: "max_turns is 40",
		},
		{
			name: "starting tree is empty",
			mutate: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "task-07", "repo", "main.txt")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "is empty: there is no starting tree",
		},
		{
			name: "unknown field in a task manifest",
			mutate: func(t *testing.T, dir string) {
				editTask(t, dir, "task-08", func(m map[string]any) { m["orcale"] = "typo" })
			},
			wantErr: "unknown field",
		},
		{
			name: "task file edited without refreezing",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "task-09", "repo", "main.txt"), "edited\n")
			},
			wantErr: "do not match the digest",
		},
		{
			name: "corpus version left blank",
			mutate: func(t *testing.T, dir string) {
				editManifest(t, dir, func(m map[string]any) { m["corpus_version"] = "" })
			},
			wantErr: "corpus_version is empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCorpus(t, dir, corpus.MinTasks, nil)

			if _, err := corpus.Load(dir); err != nil {
				t.Fatalf("the unmutated corpus must load, or this case proves nothing: %v", err)
			}

			tc.mutate(t, dir)

			_, err := corpus.Load(dir)
			if err == nil {
				t.Fatalf("Load accepted a corpus with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Load error = %v\nwant it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadRejectsAShortCorpus is separate because it needs a different
// starting point: a corpus that is well-formed but too small to be the one the
// benchmark is supposed to run.
func TestLoadRejectsAShortCorpus(t *testing.T) {
	dir := t.TempDir()
	writeCorpus(t, dir, corpus.MinTasks-1, nil)

	_, err := corpus.Load(dir)
	if err == nil {
		t.Fatal("Load accepted a corpus with fewer tasks than MinTasks")
	}
	if !strings.Contains(err.Error(), "want at least") {
		t.Errorf("Load error = %v, want it to name the floor", err)
	}
}

// TestLoadRejectsACorpusWithoutTheRequiredTraits guards the composition
// constraint at load time, not only in the test above: a corpus where nothing
// forces a read before an edit is not the corpus this project agreed to build.
func TestLoadRejectsACorpusWithoutTheRequiredTraits(t *testing.T) {
	dir := t.TempDir()
	writeCorpus(t, dir, corpus.MinTasks, func(id string, m map[string]any) {
		m["traits"] = []any{}
	})

	_, err := corpus.Load(dir)
	if err == nil {
		t.Fatal("Load accepted a corpus with no requires_read or multi_file tasks")
	}
	for _, want := range []string{corpus.TraitRequiresRead, corpus.TraitMultiFile} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load error = %v, want it to mention %s", err, want)
		}
	}
}

// --- fixture helpers ------------------------------------------------------

// writeCorpus writes a synthetic corpus of n tasks that loads cleanly, and
// records the digest of what it wrote. tweak, if given, is applied to every
// task manifest before it is written.
func writeCorpus(t *testing.T, dir string, n int, tweak func(id string, m map[string]any)) {
	t.Helper()

	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("task-%02d", i)
		ids = append(ids, id)
		writeTask(t, dir, id, func(m map[string]any) {
			// The first two tasks carry the traits the corpus-level rules
			// require, so an unmutated fixture loads.
			switch id {
			case "task-00":
				m["traits"] = []any{corpus.TraitRequiresRead}
			case "task-01":
				m["traits"] = []any{corpus.TraitRequiresRead, corpus.TraitMultiFile}
			}
			if tweak != nil {
				tweak(id, m)
			}
		})
	}

	writeManifest(t, dir, ids)
}

// writeTask writes one synthetic task directory.
func writeTask(t *testing.T, dir, id string, tweak func(m map[string]any)) {
	t.Helper()

	manifest := map[string]any{
		"schema_version": corpus.SchemaVersion,
		"id":             id,
		"title":          "synthetic " + id,
		"statement":      "Make the tests pass.",
		"language":       "go",
		"requires":       []any{"go"},
		"oracle": map[string]any{
			"argv":            []any{"go", "test", "./..."},
			"env":             map[string]any{"GOPROXY": "off"},
			"timeout_seconds": 60,
		},
		"traits":    []any{},
		"max_turns": 20,
		"notes":     "synthetic fixture",
	}
	if tweak != nil {
		tweak(manifest)
	}

	writeFile(t, filepath.Join(dir, id, "repo", "main.txt"), "start\n")
	writeJSON(t, filepath.Join(dir, id, corpus.TaskManifestName), manifest)
}

// editTask rewrites an existing task manifest through tweak.
func editTask(t *testing.T, dir, id string, tweak func(m map[string]any)) {
	t.Helper()
	path := filepath.Join(dir, id, corpus.TaskManifestName)
	m := readJSON(t, path)
	tweak(m)
	writeJSON(t, path, m)
}

// editManifest rewrites the corpus manifest through tweak, leaving the digest
// it already records alone.
func editManifest(t *testing.T, dir string, tweak func(m map[string]any)) {
	t.Helper()
	path := filepath.Join(dir, corpus.ManifestName)
	m := readJSON(t, path)
	tweak(m)
	writeJSON(t, path, m)
}

// writeManifest writes corpus.json, recording the digest of the tasks that are
// on disk right now — which is what freezing means.
func writeManifest(t *testing.T, dir string, ids []string) {
	t.Helper()

	digest, err := corpus.Digest(dir)
	if err != nil {
		t.Fatalf("digesting the fixture corpus: %v", err)
	}

	tasks := make([]any, 0, len(ids))
	for _, id := range ids {
		tasks = append(tasks, id)
	}

	writeJSON(t, filepath.Join(dir, corpus.ManifestName), map[string]any{
		"schema_version": corpus.SchemaVersion,
		"corpus_version": "0.0.0-fixture",
		"digest":         digest,
		"description":    "synthetic fixture corpus",
		"tasks":          tasks,
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func writeJSON(t *testing.T, path string, value map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
	writeFile(t, path, string(b)+"\n")
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return m
}

// relativeFiles lists every file under dir, as slash-separated paths relative
// to it.
func relativeFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
