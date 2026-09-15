// Package aidergen maps an Exercism practice exercise, as vendored in
// Aider-AI/polyglot-benchmark, onto kopicode's existing corpus task shape.
//
// It is the field-mapping half of the generator behind KAN-1406: the pure,
// filesystem-and-subprocess-light logic that turns one exercise directory into a
// task.json manifest plus the repo/, _solutions/ and pristine-tests/ file plans
// a frozen corpus needs. The orchestration — walking a whole track, running the
// fails-before/passes-after admission filter, freezing the output and pinning
// the digest — lives in tools/aidergen, which drives this package.
//
// # Why a package and not just a script
//
// docs/aider-polyglot-integration-scoping.md §2 shows every task.json field is a
// mechanical function of the exercise's .meta/config.json and .docs. That makes
// the mapping worth unit-testing on its own, without a network clone or a real
// toolchain, which is exactly what living in an ordinary package with a
// tempdir-driven test buys. The tool on top stays thin.
//
// # No new schema
//
// This package deliberately re-declares task.json's on-disk shape ([TaskManifest])
// rather than importing an unexported type from internal/corpus. The schema is a
// compatibility surface pinned at [corpus.SchemaVersion]; the loader validates
// every field this package writes, so a drift between the two shapes fails a
// corpus load rather than passing silently.
package aidergen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/leejianrong/kopicode/internal/corpus"
)

// Track is one language track the generator supports. The set is deliberately
// the two toolchains kopicode's bench machine is guaranteed to have (ADR-0005,
// docs/aider-polyglot-integration-scoping.md §3); the other four polyglot tracks
// would each need a toolchain guarantee and a knownLanguages entry.
type Track struct {
	// Name is the upstream track directory, e.g. "go" or "python".
	Name string
	// Language is the value written to task.json's language field. It must be
	// one of corpus's knownLanguages.
	Language string
	// IDPrefix is prepended to the slug to form the task id, e.g. "aider-go".
	IDPrefix string
	// Requires is the executables the oracle needs on PATH.
	Requires []string
	// OracleTimeout is the per-task oracle timeout in seconds.
	OracleTimeout int
	// oracle builds the argv+env for a given set of test files.
	oracle func(testFiles []string) (corpus.Oracle, error)
	// runCommand is a human-readable form of the oracle for the statement
	// trailer, e.g. "go test ./...".
	runCommand func(testFiles []string) string
}

// GoTrack is the Go language track.
func GoTrack() Track {
	return Track{
		Name:          "go",
		Language:      "go",
		IDPrefix:      "aider-go",
		Requires:      []string{"go"},
		OracleTimeout: 120,
		oracle: func(_ []string) (corpus.Oracle, error) {
			return corpus.Oracle{
				Argv: []string{"go", "test", "./..."},
				// -mod=readonly and GOPROXY=off keep the oracle offline and
				// deterministic, matching the existing Go tasks in bench/tasks.
				Env: map[string]string{
					"GOFLAGS": "-mod=readonly",
					"GOPROXY": "off",
				},
				TimeoutSeconds: 120,
			}, nil
		},
		runCommand: func(_ []string) string { return "go test ./..." },
	}
}

// PythonTrack is the Python language track.
//
// The oracle names the test modules explicitly rather than relying on unittest
// discovery. Exercism test files are named <slug>_test.py, which does not match
// unittest's default test*.py discovery pattern — so `python3 -m unittest
// discover` would find zero tests and exit 0, silently turning a fails-before
// check into a false pass. Naming the module also sidesteps the corpus
// validator's rejection of a '*' shell metacharacter in argv, which a
// `-p '*_test.py'` pattern would trip.
func PythonTrack() Track {
	toModules := func(testFiles []string) []string {
		mods := make([]string, 0, len(testFiles))
		for _, f := range testFiles {
			base := filepath.Base(filepath.ToSlash(f))
			mods = append(mods, strings.TrimSuffix(base, ".py"))
		}
		return mods
	}
	return Track{
		Name:          "python",
		Language:      "python",
		IDPrefix:      "aider-python",
		Requires:      []string{"python3"},
		OracleTimeout: 60,
		oracle: func(testFiles []string) (corpus.Oracle, error) {
			mods := toModules(testFiles)
			if len(mods) == 0 {
				return corpus.Oracle{}, fmt.Errorf("no test files to build a unittest oracle from")
			}
			argv := append([]string{"python3", "-m", "unittest"}, mods...)
			return corpus.Oracle{
				Argv: argv,
				Env: map[string]string{
					"PYTHONDONTWRITEBYTECODE": "1",
				},
				TimeoutSeconds: 60,
			}, nil
		},
		runCommand: func(testFiles []string) string {
			return "python3 -m unittest " + strings.Join(toModules(testFiles), " ")
		},
	}
}

// exerciseConfig is the subset of .meta/config.json this package reads. The file
// carries more fields (source, blurb, …); unknown fields are ignored rather than
// rejected, because config.json is upstream's schema, not kopicode's.
type exerciseConfig struct {
	Authors      []string `json:"authors"`
	Contributors []string `json:"contributors"`
	Files        struct {
		Solution    []string `json:"solution"`
		Test        []string `json:"test"`
		Example     []string `json:"example"`
		Editor      []string `json:"editor"`
		Invalidator []string `json:"invalidator"`
	} `json:"files"`
	Blurb     string `json:"blurb"`
	Source    string `json:"source"`
	SourceURL string `json:"source_url"`
}

// Exercise is one parsed Exercism exercise directory.
type Exercise struct {
	// Track is the language track the exercise belongs to.
	Track Track
	// Slug is the exercise directory name, e.g. "wordy".
	Slug string
	// Dir is the absolute path of the exercise directory.
	Dir string
	// Config is the parsed .meta/config.json.
	Config exerciseConfig
	// Statement is the assembled task statement (instructions plus trailer).
	Statement string
}

// TaskManifest is the on-disk shape of a task.json. It mirrors internal/corpus's
// unexported taskFile; see the package doc comment for why it is re-declared.
type TaskManifest struct {
	SchemaVersion int            `json:"schema_version"`
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Statement     string         `json:"statement"`
	Language      string         `json:"language"`
	Requires      []string       `json:"requires"`
	Oracle        oracleManifest `json:"oracle"`
	Traits        []string       `json:"traits"`
	MaxTurns      int            `json:"max_turns"`
	Notes         string         `json:"notes"`
}

type oracleManifest struct {
	Argv           []string          `json:"argv"`
	Env            map[string]string `json:"env"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

// ID is the exercise's task id: the track prefix plus the slug.
func (e *Exercise) ID() string {
	return e.Track.IDPrefix + "-" + e.Slug
}

// ReadExercise reads and assembles one exercise from its directory.
//
// It reads .meta/config.json and .docs/instructions.md (plus the optional
// .append.md), and builds the statement. It does not decide admission — that is
// the caller's fails-before/passes-after check — but it does reject an exercise
// whose config.json is missing the pieces a valid task needs, so a malformed
// upstream directory is a loud skip rather than a silent half-task.
func ReadExercise(track Track, dir string) (*Exercise, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("aidergen: resolving %s: %w", dir, err)
	}
	slug := filepath.Base(abs)

	cfgPath := filepath.Join(abs, ".meta", "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("aidergen: reading %s: %w", cfgPath, err)
	}
	var cfg exerciseConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("aidergen: parsing %s: %w", cfgPath, err)
	}

	if len(cfg.Files.Solution) == 0 {
		return nil, fmt.Errorf("aidergen: %s: config.json declares no solution files", slug)
	}
	if len(cfg.Files.Test) == 0 {
		return nil, fmt.Errorf("aidergen: %s: config.json declares no test files", slug)
	}
	if len(cfg.Files.Example) != len(cfg.Files.Solution) {
		return nil, fmt.Errorf(
			"aidergen: %s: %d example files but %d solution files; the overlay maps them "+
				"one-to-one", slug, len(cfg.Files.Example), len(cfg.Files.Solution))
	}
	if strings.TrimSpace(cfg.Blurb) == "" {
		return nil, fmt.Errorf("aidergen: %s: config.json has no blurb to use as the title", slug)
	}

	statement, err := buildStatement(abs, track, cfg.Files.Test)
	if err != nil {
		return nil, fmt.Errorf("aidergen: %s: %w", slug, err)
	}

	return &Exercise{
		Track:     track,
		Slug:      slug,
		Dir:       abs,
		Config:    cfg,
		Statement: statement,
	}, nil
}

// buildStatement assembles the natural-language task from the exercise's docs.
//
// The instructions are the problem, exactly what a person attempting the
// exercise is given; they are not a leak of the fix, so they are carried
// verbatim. A trailer names the command the model can run to check its work,
// mirroring the existing corpus statements.
func buildStatement(dir string, track Track, testFiles []string) (string, error) {
	docsDir := filepath.Join(dir, ".docs")
	instructions, err := os.ReadFile(filepath.Join(docsDir, "instructions.md"))
	if err != nil {
		return "", fmt.Errorf("reading instructions.md: %w", err)
	}

	var b strings.Builder
	b.Write(bytes.TrimRight(instructions, "\n"))

	if appendix, err := os.ReadFile(filepath.Join(docsDir, "instructions.append.md")); err == nil {
		b.WriteString("\n\n")
		b.Write(bytes.TrimRight(appendix, "\n"))
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("reading instructions.append.md: %w", err)
	}

	b.WriteString("\n\nImplement the stub so that the test suite passes. Run `")
	b.WriteString(track.runCommand(testFiles))
	b.WriteString("` to check.")

	return b.String(), nil
}

// Manifest builds the task.json for the exercise. commit is the upstream
// polyglot-benchmark commit the exercise was read from, recorded in the notes
// for provenance.
func (e *Exercise) Manifest(commit string) (TaskManifest, error) {
	oracle, err := e.Track.oracle(e.Config.Files.Test)
	if err != nil {
		return TaskManifest{}, fmt.Errorf("aidergen: %s: %w", e.Slug, err)
	}

	traits := []string{corpus.TraitRequiresRead}
	if len(e.Config.Files.Solution) > 1 {
		traits = append(traits, corpus.TraitMultiFile)
	}

	return TaskManifest{
		SchemaVersion: corpus.SchemaVersion,
		ID:            e.ID(),
		Title:         e.Config.Blurb,
		Statement:     e.Statement,
		Language:      e.Track.Language,
		Requires:      e.Track.Requires,
		Oracle: oracleManifest{
			Argv:           oracle.Argv,
			Env:            oracle.Env,
			TimeoutSeconds: oracle.TimeoutSeconds,
		},
		Traits:   traits,
		MaxTurns: 20,
		Notes:    e.notes(commit),
	}, nil
}

// notes records where the exercise came from and under what licence. It is
// never shown to the model; it is the provenance a reviewer of the corpus reads,
// and it carries the attribution the MIT licence requires (§5).
func (e *Exercise) notes(commit string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Vendored from the Exercism %s track via Aider-AI/polyglot-benchmark", e.Track.Name)
	if commit != "" {
		fmt.Fprintf(&b, " @ %s", commit)
	}
	fmt.Fprintf(&b, ". Upstream slug: %s.", e.Slug)
	if len(e.Config.Authors) > 0 {
		fmt.Fprintf(&b, " Authors: %s.", strings.Join(e.Config.Authors, ", "))
	}
	if len(e.Config.Contributors) > 0 {
		fmt.Fprintf(&b, " Contributors: %s.", strings.Join(e.Config.Contributors, ", "))
	}
	if e.Config.SourceURL != "" {
		fmt.Fprintf(&b, " Source: %s.", e.Config.SourceURL)
	}
	b.WriteString(" Licensed MIT © Exercism and the exercise's authors and contributors; " +
		"see LICENSE.md. Generated by tools/aidergen (KAN-1406), not hand-authored; " +
		"needs only the declared toolchain and the standard library.")
	return b.String()
}

// repoRelFiles returns the exercise files that belong in repo/: everything under
// the exercise directory except the .meta/ and .docs/ trees, whose contents
// become the reference solution and the statement respectively. Paths are
// forward-slash and relative to the exercise directory.
func (e *Exercise) repoRelFiles() ([]string, error) {
	var rels []string
	err := filepath.WalkDir(e.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(e.Dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".meta" || rel == ".docs" {
				return fs.SkipDir
			}
			return nil
		}
		if rel == "." {
			return nil
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("aidergen: %s: walking exercise: %w", e.Slug, err)
	}
	sort.Strings(rels)
	return rels, nil
}

// WriteRepo materialises repo/ for the exercise under destRepoDir.
func (e *Exercise) WriteRepo(destRepoDir string) error {
	rels, err := e.repoRelFiles()
	if err != nil {
		return err
	}
	for _, rel := range rels {
		if err := copyFile(filepath.Join(e.Dir, rel), filepath.Join(destRepoDir, filepath.FromSlash(rel))); err != nil {
			return fmt.Errorf("aidergen: %s: repo/%s: %w", e.Slug, rel, err)
		}
	}
	return nil
}

// WriteSolution materialises the reference-solution overlay under destSolDir.
// Each example file's content is written to its matching solution file's path,
// so overlaying destSolDir onto repo/ replaces the stub with the reference
// implementation.
func (e *Exercise) WriteSolution(destSolDir string) error {
	for i, sol := range e.Config.Files.Solution {
		src := filepath.Join(e.Dir, filepath.FromSlash(e.Config.Files.Example[i]))
		dst := filepath.Join(destSolDir, filepath.FromSlash(sol))
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("aidergen: %s: solution %s: %w", e.Slug, sol, err)
		}
	}
	return nil
}

// PristineTestFiles returns the repo-relative paths of the files a grader must
// restore before running the oracle: the test files, and — for Go — the editor
// (cases) files that the test files compile against. See
// docs/aider-polyglot-integration-scoping.md §4 and KAN-1407.
func (e *Exercise) PristineTestFiles() []string {
	seen := map[string]bool{}
	var out []string
	add := func(files []string) {
		for _, f := range files {
			f = filepath.ToSlash(f)
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	add(e.Config.Files.Test)
	add(e.Config.Files.Editor)
	sort.Strings(out)
	return out
}

// WritePristineTests materialises the pristine test copies under destDir,
// preserving each file's repo-relative path. This copy is kept out of repo/ so a
// grader can restore the original tests over an agent-mutated tree without the
// agent ever seeing a second copy.
func (e *Exercise) WritePristineTests(destDir string) error {
	for _, rel := range e.PristineTestFiles() {
		src := filepath.Join(e.Dir, filepath.FromSlash(rel))
		dst := filepath.Join(destDir, filepath.FromSlash(rel))
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("aidergen: %s: pristine test %s: %w", e.Slug, rel, err)
		}
	}
	return nil
}

// copyFile copies one regular file, creating parent directories as needed.
func copyFile(src, dst string) error {
	content, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, content, 0o644)
}

// FrozenComposition is the corpus-level composition policy the frozen Go+Python
// aider corpus is built to satisfy, and the one both the generator's self-check
// and the loading test use so there is a single definition of what this corpus
// promises.
//
// MinMultiFile is 0 on purpose: Exercism Go/Python exercises are overwhelmingly
// single-solution-file, so a pure-aider corpus can legitimately contain no
// multi_file task — the floor KAN-1405 made injectable precisely so this corpus
// could set it to zero. The task and read floors are not zero: they guard
// against a generation run that admitted far too few exercises to be a
// benchmark, which is a real failure rather than a valid smaller experiment.
func FrozenComposition() corpus.CompositionPolicy {
	return corpus.CompositionPolicy{
		MinTasks:        30,
		MinRequiresRead: 30,
		MinMultiFile:    0,
	}
}

// MarshalTask renders a task manifest as indented JSON with a trailing newline,
// matching the hand-authored manifests in bench/tasks.
func MarshalTask(m TaskManifest) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("aidergen: marshalling task %s: %w", m.ID, err)
	}
	return append(b, '\n'), nil
}
