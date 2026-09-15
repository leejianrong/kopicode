// Command aidergen generates a frozen kopicode benchmark corpus from Exercism
// practice exercises as vendored in Aider-AI/polyglot-benchmark.
//
// It reads a local checkout of the polyglot repo (pinned; nothing is fetched),
// maps each Go and Python exercise onto kopicode's existing task.json shape via
// internal/aidergen, admits only exercises whose oracle fails on the bare stub
// and passes on the reference solution, and freezes a stratified Go+Python
// subset — corpus.json with its content digest, one directory per task holding
// task.json + repo/ + pristine-tests/, and a reference solution overlay under
// bench/_solutions/. See docs/aider-polyglot-integration-scoping.md and
// KAN-1406.
//
// # Why this is not a cmd/ front end
//
// It imports internal/corpus (for the content digest and the loader's own
// validation), which docs/adr/0003-single-repo-internal-engine.md forbids a
// cmd/ front end from doing. It is a developer tool, not a product surface, so
// it lives under tools/ where that boundary does not apply. It makes no provider
// calls and costs nothing to run.
//
// Usage:
//
//	go run ./tools/aidergen --source <polyglot checkout> [--out bench/aider-go-python] [--limit 18]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/leejianrong/kopicode/internal/aidergen"
	"github.com/leejianrong/kopicode/internal/corpus"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "aidergen:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		source  = flag.String("source", "", "path to a local Aider-AI/polyglot-benchmark checkout (required)")
		out     = flag.String("out", "bench/aider-go-python", "corpus directory to write")
		limit   = flag.Int("limit", 18, "maximum tasks to admit per language track")
		commit  = flag.String("commit", "", "upstream commit for provenance (default: read from --source's git HEAD)")
		version = flag.String("corpus-version", "1.0.0", "corpus_version to record in corpus.json")
		verbose = flag.Bool("verbose", false, "log every candidate's admission result")
	)
	flag.Parse()

	if *source == "" {
		return errors.New("--source is required (a local polyglot-benchmark checkout)")
	}
	srcRoot, err := filepath.Abs(*source)
	if err != nil {
		return fmt.Errorf("resolving --source: %w", err)
	}
	outRoot, err := filepath.Abs(*out)
	if err != nil {
		return fmt.Errorf("resolving --out: %w", err)
	}

	upstream := *commit
	if upstream == "" {
		upstream, err = gitHEAD(srcRoot)
		if err != nil {
			return fmt.Errorf("reading upstream commit from %s (pass --commit to skip): %w", srcRoot, err)
		}
	}

	tracks := []aidergen.Track{aidergen.GoTrack(), aidergen.PythonTrack()}

	var admitted []candidate
	for _, track := range tracks {
		got, err := admitTrack(track, srcRoot, upstream, *limit, *verbose)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "aidergen: %s: admitted %d task(s)\n", track.Name, len(got))
		admitted = append(admitted, got...)
	}

	if err := freeze(outRoot, upstream, *out, *version, admitted); err != nil {
		return err
	}

	// The corpus must load under its own composition policy, digest and all —
	// the same check the loader will apply and the acceptance criterion for
	// KAN-1406.
	c, err := corpus.LoadWithPolicy(outRoot, aidergen.FrozenComposition())
	if err != nil {
		return fmt.Errorf("the frozen corpus does not load: %w", err)
	}
	fmt.Fprintf(os.Stderr, "aidergen: wrote and validated corpus %s (%s) with %d tasks\n",
		c.Version, c.Digest, len(c.Tasks))
	return nil
}

// candidate is an admitted exercise and its manifest.
type candidate struct {
	ex       *aidergen.Exercise
	manifest aidergen.TaskManifest
}

// admitTrack reads every exercise in a track, keeps those that pass the
// fails-before/passes-after admission filter, and returns the first limit of
// them in alphabetical slug order.
func admitTrack(track aidergen.Track, srcRoot, upstream string, limit int, verbose bool) ([]candidate, error) {
	practice := filepath.Join(srcRoot, track.Name, "exercises", "practice")
	entries, err := os.ReadDir(practice)
	if err != nil {
		return nil, fmt.Errorf("%s: reading %s: %w", track.Name, practice, err)
	}

	var slugs []string
	for _, e := range entries {
		if e.IsDir() {
			slugs = append(slugs, e.Name())
		}
	}
	sort.Strings(slugs)

	var admitted []candidate
	for _, slug := range slugs {
		if len(admitted) >= limit {
			break
		}
		ex, err := aidergen.ReadExercise(track, filepath.Join(practice, slug))
		if err != nil {
			if verbose {
				fmt.Fprintf(os.Stderr, "  skip %s/%s: %v\n", track.Name, slug, err)
			}
			continue
		}
		m, err := ex.Manifest(upstream)
		if err != nil {
			if verbose {
				fmt.Fprintf(os.Stderr, "  skip %s/%s: %v\n", track.Name, slug, err)
			}
			continue
		}
		ok, reason, err := passesAdmission(ex, m)
		if err != nil {
			return nil, err
		}
		if !ok {
			if verbose {
				fmt.Fprintf(os.Stderr, "  drop %s: %s\n", ex.ID(), reason)
			}
			continue
		}
		if verbose {
			fmt.Fprintf(os.Stderr, "  admit %s\n", ex.ID())
		}
		admitted = append(admitted, candidate{ex: ex, manifest: m})
	}
	return admitted, nil
}

// passesAdmission runs the exercise's oracle on the bare stub (must fail) and on
// the reference-solution overlay (must pass), in a throwaway working tree. An
// exercise that already passes on the stub measures nothing; one that still
// fails after the reference fix would cap every arm for a reason unrelated to
// the model. See docs/aider-polyglot-integration-scoping.md §2.
func passesAdmission(ex *aidergen.Exercise, m aidergen.TaskManifest) (ok bool, reason string, err error) {
	work, err := os.MkdirTemp("", "aidergen-admit-")
	if err != nil {
		return false, "", fmt.Errorf("%s: temp dir: %w", ex.ID(), err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	repo := filepath.Join(work, "repo")
	if err := ex.WriteRepo(repo); err != nil {
		return false, "", err
	}

	code, _, err := runOracle(m, repo)
	if err != nil {
		return false, "", fmt.Errorf("%s: oracle on stub: %w", ex.ID(), err)
	}
	if code == 0 {
		return false, "oracle passes on the bare stub, so the task measures nothing", nil
	}

	// Overlay the reference solution directly onto the working tree and re-run.
	if err := ex.WriteSolution(repo); err != nil {
		return false, "", err
	}
	code, output, err := runOracle(m, repo)
	if err != nil {
		return false, "", fmt.Errorf("%s: oracle on solution: %w", ex.ID(), err)
	}
	if code != 0 {
		return false, fmt.Sprintf("oracle still fails after the reference solution (exit %d): %s",
			code, firstLines(output, 3)), nil
	}
	return true, "", nil
}

// runOracle runs the manifest's oracle argv in dir and returns its exit code and
// combined output.
func runOracle(m aidergen.TaskManifest, dir string) (int, string, error) {
	timeout := time.Duration(m.Oracle.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, m.Oracle.Argv[0], m.Oracle.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = childEnv(m.Oracle.Env)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()
	if ctx.Err() != nil {
		return -1, buf.String(), fmt.Errorf("oracle timed out after %s", timeout)
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return 0, buf.String(), nil
	case errors.As(runErr, &exitErr):
		return exitErr.ExitCode(), buf.String(), nil
	default:
		return -1, buf.String(), runErr
	}
}

// gitHEAD returns the HEAD commit of the git repository at dir.
func gitHEAD(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	cmd.Env = childEnv(nil)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w: %s", err, buf.String())
	}
	return strings.TrimSpace(buf.String()), nil
}

// toolchainPassThrough is the allowlist of environment variables the go, python3
// and git subprocesses need to find themselves and their caches. Everything else
// is dropped so an inherited GOFLAGS, PYTHONPATH, GOPROXY or GIT_DIR cannot
// change what a subprocess does without changing anything visible in the
// command. It mirrors internal/corpus's oracle test env builder (KAN-854); the
// per-oracle overlay (task.Oracle.Env) goes on top.
var toolchainPassThrough = []string{
	"PATH", "HOME", "TMPDIR", "TEMP", "TMP",
	"GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE",
	// Windows needs these to start a process at all.
	"SystemRoot", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "ComSpec",
}

// childEnv builds a subprocess environment from the toolchain allowlist plus the
// given overlay. Building rather than inheriting is the subprocess rule this
// repository enforces statically (internal/arch/subprocess_test.go): an
// allowlist naturally drops GIT_DIR and the GO*/PYTHON* overrides that would
// otherwise redirect a command whose Dir is correct.
func childEnv(overlay map[string]string) []string {
	env := make([]string, 0, len(toolchainPassThrough)+len(overlay))
	for _, name := range toolchainPassThrough {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	// Sort the overlay so the environment is deterministic across runs.
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+overlay[k])
	}
	return env
}

// freeze writes the admitted tasks, the reference solutions, the licence and
// readme, and finally corpus.json with the computed digest.
func freeze(outRoot, upstream, outRel, version string, admitted []candidate) error {
	if err := os.MkdirAll(outRoot, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", outRoot, err)
	}

	ids := make([]string, 0, len(admitted))
	for _, c := range admitted {
		id := c.manifest.ID
		ids = append(ids, id)

		taskDir := filepath.Join(outRoot, id)
		if err := os.MkdirAll(taskDir, 0o755); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}

		blob, err := aidergen.MarshalTask(c.manifest)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(taskDir, corpus.TaskManifestName), blob, 0o644); err != nil {
			return fmt.Errorf("%s: writing task.json: %w", id, err)
		}
		if err := c.ex.WriteRepo(filepath.Join(taskDir, corpus.RepoDirName)); err != nil {
			return err
		}
		// The pristine test copy lives under a leading-underscore directory so
		// the Go toolchain skips it in ./... (its *_test.go files have no
		// package siblings and would fail `go vet`), while corpus.Digest — which
		// walks every file — still freezes it. It is out of repo/, so the bench
		// runner never copies it into the agent's worktree.
		if err := c.ex.WritePristineTests(filepath.Join(taskDir, corpus.PristineTestsDirName)); err != nil {
			return err
		}
		if err := c.ex.WriteSolution(corpus.SolutionDir(outRoot, id)); err != nil {
			return err
		}
	}

	if err := writeLicense(filepath.Join(outRoot, "LICENSE.md")); err != nil {
		return err
	}
	if err := writeReadme(filepath.Join(outRoot, "README.md"), upstream, outRel, version, admitted); err != nil {
		return err
	}

	// corpus.json is excluded from the digest, so it must be written after the
	// digest is computed over everything else.
	digest, err := corpus.Digest(outRoot)
	if err != nil {
		return fmt.Errorf("computing digest: %w", err)
	}
	manifest := struct {
		SchemaVersion int      `json:"schema_version"`
		Version       string   `json:"corpus_version"`
		Digest        string   `json:"digest"`
		Description   string   `json:"description"`
		Tasks         []string `json:"tasks"`
	}{
		SchemaVersion: corpus.SchemaVersion,
		Version:       version,
		Digest:        digest,
		Description: "kopicode aider-polyglot Go+Python subset: Exercism practice exercises " +
			"(Aider-AI/polyglot-benchmark), each solvable offline, oracle-graded by its own " +
			"test suite; admitted only if it fails on the stub and passes on the reference solution.",
		Tasks: ids,
	}
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling corpus.json: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(filepath.Join(outRoot, corpus.ManifestName), out, 0o644); err != nil {
		return fmt.Errorf("writing corpus.json: %w", err)
	}
	return nil
}

func writeLicense(path string) error {
	const text = `# Licence and attribution

The task content in this corpus is vendored from Exercism practice exercises, as
collected in [Aider-AI/polyglot-benchmark](https://github.com/Aider-AI/polyglot-benchmark).

Exercise content is copyright © Exercism and the individual exercise authors and
contributors named in each task's ` + "`notes`" + ` field, and is used under the MIT
Licence. The polyglot-benchmark repository does not ship a per-exercise LICENSE
file for the Go and Python trees, so this notice traces the licence to the
upstream track repositories ` + "(`exercism/go`, `exercism/python`)" + `, which are MIT.

Aider's own benchmark harness (Apache-2.0) is *not* vendored here: this corpus is
task content only, generated by ` + "`tools/aidergen`" + `.

    MIT License

    Copyright (c) Exercism and the exercise authors and contributors.

    Permission is hereby granted, free of charge, to any person obtaining a copy
    of this software and associated documentation files (the "Software"), to deal
    in the Software without restriction, including without limitation the rights
    to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
    copies of the Software, and to permit persons to whom the Software is
    furnished to do so, subject to the following conditions:

    The above copyright notice and this permission notice shall be included in all
    copies or substantial portions of the Software.

    THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
    IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
    FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
    AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
    LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
    OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
    SOFTWARE.
`
	return os.WriteFile(path, []byte(text), 0o644)
}

func writeReadme(path, upstream, outRel, version string, admitted []candidate) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# aider-polyglot Go+Python corpus (v%s)\n\n", version)
	b.WriteString("Generated, not hand-authored. This is a frozen subset of Exercism practice\n")
	b.WriteString("exercises vendored from Aider-AI/polyglot-benchmark, mapped onto kopicode's\n")
	b.WriteString("task.json shape by `tools/aidergen`. See\n")
	b.WriteString("`docs/aider-polyglot-integration-scoping.md` for the design.\n\n")
	fmt.Fprintf(&b, "- Upstream: Aider-AI/polyglot-benchmark @ `%s`\n", upstream)
	fmt.Fprintf(&b, "- Regenerate: `go run ./tools/aidergen --source <polyglot checkout> --out %s`\n", outRel)
	b.WriteString("- Licence: MIT © Exercism; see `LICENSE.md`. Attribution per task is in its `notes`.\n\n")

	var goIDs, pyIDs []string
	for _, c := range admitted {
		switch c.manifest.Language {
		case "go":
			goIDs = append(goIDs, c.manifest.ID)
		case "python":
			pyIDs = append(pyIDs, c.manifest.ID)
		}
	}
	fmt.Fprintf(&b, "## Tasks (%d Go, %d Python)\n\n", len(goIDs), len(pyIDs))
	for _, c := range admitted {
		fmt.Fprintf(&b, "- `%s` — %s\n", c.manifest.ID, c.manifest.Title)
	}

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// firstLines returns at most n lines of s, for compact drop diagnostics.
func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}
