// Package corpus loads and validates the frozen benchmark task corpus that
// lives at bench/tasks.
//
// The corpus is data, not code: a directory per task, each holding a task.json
// manifest and a repo/ tree the agent is pointed at. This package is the only
// thing that reads that layout, so the schema has one definition rather than
// one per consumer.
//
// # Why loading validates
//
// docs/adr/0005-benchmark-and-ab-methodology.md makes the corpus an
// experiment-series boundary: results are only comparable within one frozen
// corpus. A loader that happily reads a corpus somebody has edited turns that
// boundary into a convention nobody can check, so [Load] fails on a corpus
// whose contents no longer match the digest recorded in corpus.json. Bumping
// the version is then a deliberate act, and a result that records
// (Version, Digest) records something that could not have drifted underneath
// it.
//
// The same reasoning drives discovery. Tasks are found by walking the
// directory rather than read from a list, because a hand-written list is how an
// eleventh task ends up unvalidated. The list in corpus.json is still there —
// it fixes the run order that ADR-0005 §4's sequential early stopping needs —
// but it is cross-checked against what is on disk rather than trusted.
//
// This package does not run anything. Executing an oracle is the bench
// runner's job.
package corpus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SchemaVersion is the manifest schema this package understands. Both
// corpus.json and every task.json carry it.
const SchemaVersion = 1

// MinTasks is the smallest corpus this loader accepts. The slice-1 corpus is
// ten tasks (docs/SLICE-1.md build plan step 15); the floor exists so a
// validation run over a corpus that lost most of its tasks — an empty
// directory, a bad checkout, a walk that started in the wrong place — fails
// instead of reporting green over almost nothing.
const MinTasks = 10

// ManifestName is the corpus-level manifest, at the root of the corpus tree.
const ManifestName = "corpus.json"

// TaskManifestName is the per-task manifest, at the root of a task directory.
const TaskManifestName = "task.json"

// RepoDirName is the directory inside a task that holds the starting tree. It
// is the directory the agent is pointed at, and the only part of a task the
// agent may see.
const RepoDirName = "repo"

// Corpus is a loaded, validated corpus.
type Corpus struct {
	// SchemaVersion is the manifest schema the corpus was written against.
	SchemaVersion int
	// Version identifies the experiment series. Results from two different
	// versions are not comparable and must not be pooled.
	Version string
	// Digest is the content digest recorded in corpus.json, which [Load] has
	// verified against the files on disk.
	Digest string
	// Description is a one-line summary of what the corpus is for.
	Description string
	// Tasks are the tasks in canonical run order.
	Tasks []Task
	// Root is the directory the corpus was loaded from.
	Root string
}

// Task is one benchmark task.
type Task struct {
	// SchemaVersion is the manifest schema this task was written against.
	SchemaVersion int
	// ID is the task's identifier, and also its directory name.
	ID string
	// Title is a short human-readable label for report tables.
	Title string
	// Statement is the natural-language task, in the form a user would type
	// it. It is what the engine receives; nothing else in the manifest is
	// shown to the model.
	Statement string
	// Language is the primary language of the starting tree.
	Language string
	// Requires names the executables the oracle needs on PATH. A task that
	// needs anything outside this set is not deterministic enough to be in the
	// corpus.
	Requires []string
	// Oracle is the command that decides pass or fail.
	Oracle Oracle
	// Traits record what the task exercises, so the corpus composition
	// constraints can be checked rather than asserted.
	Traits []string
	// MaxTurns is the turn budget. ADR-0005 §6 caps a corpus task at 20.
	MaxTurns int
	// Notes explain why the task discriminates and what it depends on. For
	// humans reading the corpus; never sent to the model.
	Notes string
	// Dir is the absolute path of the task directory.
	Dir string
}

// RepoDir is the absolute path of the task's starting tree.
func (t Task) RepoDir() string {
	return filepath.Join(t.Dir, RepoDirName)
}

// HasTrait reports whether the task declares the named trait.
func (t Task) HasTrait(name string) bool {
	for _, tr := range t.Traits {
		if tr == name {
			return true
		}
	}
	return false
}

// Oracle is the command that decides whether a task was completed. It is an
// argv, never a shell string: a corpus that needs a shell has smuggled in
// whatever that shell's startup files say, and there goes determinism.
type Oracle struct {
	// Argv is the command and its arguments, run in the task's repo
	// directory.
	Argv []string
	// Env are the environment variables the oracle needs set. They exist to
	// remove variance — GOPROXY=off so a task cannot reach the network,
	// PYTHONDONTWRITEBYTECODE=1 so running the oracle does not modify the
	// tree — not to configure the task.
	Env map[string]string
	// TimeoutSeconds bounds the run. A task whose suite is slower than this
	// does not belong in a corpus that runs once per arm.
	TimeoutSeconds int
}

// manifestFile is the on-disk shape of corpus.json.
type manifestFile struct {
	SchemaVersion int      `json:"schema_version"`
	Version       string   `json:"corpus_version"`
	Digest        string   `json:"digest"`
	Description   string   `json:"description"`
	Tasks         []string `json:"tasks"`
}

// taskFile is the on-disk shape of task.json.
type taskFile struct {
	SchemaVersion int        `json:"schema_version"`
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Statement     string     `json:"statement"`
	Language      string     `json:"language"`
	Requires      []string   `json:"requires"`
	Oracle        oracleFile `json:"oracle"`
	Traits        []string   `json:"traits"`
	MaxTurns      int        `json:"max_turns"`
	Notes         string     `json:"notes"`
}

type oracleFile struct {
	Argv           []string          `json:"argv"`
	Env            map[string]string `json:"env"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

// Load reads and validates the corpus rooted at dir.
//
// Every failure it can report is a reason the corpus would produce numbers
// nobody should trust, so they are all fatal: a malformed manifest, a task
// directory nobody listed, a listed task with no directory, a corpus smaller
// than [MinTasks], or contents that no longer match the recorded digest.
func Load(dir string) (*Corpus, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("corpus: resolving %s: %w", dir, err)
	}

	manifest, err := readManifest(root)
	if err != nil {
		return nil, err
	}

	found, err := discover(root)
	if err != nil {
		return nil, err
	}

	if err := checkListing(manifest.Tasks, found); err != nil {
		return nil, err
	}

	tasks := make([]Task, 0, len(manifest.Tasks))
	for _, id := range manifest.Tasks {
		task, err := readTask(root, id)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}

	c := &Corpus{
		SchemaVersion: manifest.SchemaVersion,
		Version:       manifest.Version,
		Digest:        manifest.Digest,
		Description:   manifest.Description,
		Tasks:         tasks,
		Root:          root,
	}

	if err := validateCorpus(c); err != nil {
		return nil, err
	}

	computed, err := Digest(root)
	if err != nil {
		return nil, err
	}
	if computed != manifest.Digest {
		return nil, fmt.Errorf(
			"corpus: contents do not match the digest recorded in %s\n"+
				"  recorded: %s\n"+
				"  computed: %s\n"+
				"a corpus is an experiment-series boundary (ADR-0005): if this change is "+
				"intended, bump corpus_version and record the computed digest, and treat "+
				"results from the two versions as separate series",
			ManifestName, manifest.Digest, computed)
	}

	return c, nil
}

// readManifest reads corpus.json.
func readManifest(root string) (manifestFile, error) {
	path := filepath.Join(root, ManifestName)
	b, err := os.ReadFile(path)
	if err != nil {
		return manifestFile{}, fmt.Errorf("corpus: reading %s: %w", ManifestName, err)
	}

	var m manifestFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return manifestFile{}, fmt.Errorf("corpus: parsing %s: %w", ManifestName, err)
	}
	return m, nil
}

// readTask reads and validates one task directory.
func readTask(root, id string) (Task, error) {
	dir := filepath.Join(root, id)
	path := filepath.Join(dir, TaskManifestName)

	b, err := os.ReadFile(path)
	if err != nil {
		return Task{}, fmt.Errorf("corpus: reading %s/%s: %w", id, TaskManifestName, err)
	}

	var f taskFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Task{}, fmt.Errorf("corpus: parsing %s/%s: %w", id, TaskManifestName, err)
	}

	task := Task{
		SchemaVersion: f.SchemaVersion,
		ID:            f.ID,
		Title:         f.Title,
		Statement:     f.Statement,
		Language:      f.Language,
		Requires:      f.Requires,
		Oracle: Oracle{
			Argv:           f.Oracle.Argv,
			Env:            f.Oracle.Env,
			TimeoutSeconds: f.Oracle.TimeoutSeconds,
		},
		Traits:   f.Traits,
		MaxTurns: f.MaxTurns,
		Notes:    f.Notes,
		Dir:      dir,
	}

	if err := validateTask(task, id); err != nil {
		return Task{}, err
	}
	return task, nil
}

// discover returns the task directory names present under root. Tasks are
// found by walking rather than by reading the list in corpus.json, so a task
// directory that nobody listed is a loud failure rather than a silent skip.
func discover(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("corpus: reading %s: %w", root, err)
	}

	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
			continue
		}
		name := e.Name()
		if name == ManifestName || strings.HasSuffix(name, ".md") {
			continue
		}
		return nil, fmt.Errorf(
			"corpus: unexpected file %s in the corpus root: a corpus holds %s, "+
				"documentation, and one directory per task",
			name, ManifestName)
	}

	sort.Strings(ids)
	return ids, nil
}

// checkListing cross-checks the canonical order in corpus.json against the
// directories on disk. Both directions matter: an unlisted directory would
// never run, and a listed directory that does not exist would fail late.
func checkListing(listed, found []string) error {
	seen := make(map[string]bool, len(listed))
	for _, id := range listed {
		if seen[id] {
			return fmt.Errorf("corpus: %s lists %q twice", ManifestName, id)
		}
		seen[id] = true
	}

	onDisk := make(map[string]bool, len(found))
	for _, id := range found {
		onDisk[id] = true
	}

	var problems []string
	for _, id := range found {
		if !seen[id] {
			problems = append(problems, fmt.Sprintf(
				"task directory %q is not listed in %s, so it would never run", id, ManifestName))
		}
	}
	for _, id := range listed {
		if !onDisk[id] {
			problems = append(problems, fmt.Sprintf(
				"%s lists %q but there is no such directory", ManifestName, id))
		}
	}
	if len(problems) > 0 {
		return errors.New("corpus: " + strings.Join(problems, "; "))
	}
	return nil
}
