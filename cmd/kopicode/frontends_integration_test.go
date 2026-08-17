//go:build integration

// Package main's integration test covers *both* front ends, from one file, on
// purpose.
//
// ADR-0007 decision 2 requires --model and --harness on kopicode and on
// kopibench, because setting the arm per invocation is the whole point of one
// binary serving every model. Two test files would drift the same way two flag
// declarations would: the interesting property is that the two binaries behave
// identically, and it is only checkable where both are in view.
//
// It runs the real binaries rather than calling a function, because the
// behaviour under test is an exit code and a stderr message — what a user and a
// CI job actually see — and because ADR-0007 decision 4 is about *ordering*.
// Asserting that a refused invocation leaves the working tree untouched is the
// only way to show that nothing was opened, locked or written before the
// refusal, and that is not observable from inside the process.
package main_test

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// exit codes from docs/SLICE-1.md build step 14.
const (
	exitUsage   = 2
	exitHarness = 4
)

// defaultModelID is the built-in default, repeated here rather than imported:
// ADR-0003's allowlist lets a front end import internal/engine, internal/bench
// and internal/build only, and internal/arch walks test files too. Repeating one
// string is the price of not widening a boundary for a test, and
// TestTheDefaultModelIsTheOneTheBinaryUses below holds the copy to the binary.
const defaultModelID = "qwen/qwen3-coder-next"

// frontEnd is one binary under test.
type frontEnd struct {
	name string
	pkg  string
	// verb is the subcommand an ordinary invocation needs, empty when the
	// binary takes flags on their own.
	//
	// kopibench requires `run` (KAN-796): one invocation of it is one arm over
	// the whole corpus, and on the live provider that spends money, so a bare
	// `kopibench` must not be a request to do anything. What this file is about
	// — that both binaries resolve the arm the same way, and refuse an unknown
	// one before they touch anything — is unaffected, so the verb is data
	// rather than a second copy of every case.
	verb string
}

var frontEnds = []frontEnd{
	{name: "kopicode", pkg: "./cmd/kopicode"},
	{name: "kopibench", pkg: "./cmd/kopibench", verb: "run"},
}

// argv is the front end's ordinary invocation: its verb, if it has one, then
// the flags. `--version` deliberately does not go through it — that is asked of
// the binary itself and not of a command.
func (f frontEnd) argv(args ...string) []string {
	if f.verb == "" {
		return args
	}
	return append([]string{f.verb}, args...)
}

func TestModelSelectionOnBothFrontEnds(t *testing.T) {
	for _, fe := range frontEnds {
		t.Run(fe.name, func(t *testing.T) {
			bin := build(t, fe)

			t.Run("an unknown model from the flag is a usage error", func(t *testing.T) {
				dir := repoDir(t)
				res := run(t, bin, dir, nil, fe.argv("--model", "qwen/qwen3-coder-nxt")...)

				if res.code != exitUsage {
					t.Errorf("exit code = %d, want %d (usage). stderr:\n%s", res.code, exitUsage, res.stderr)
				}
				for _, want := range []string{"qwen/qwen3-coder-nxt", "supported models", defaultModelID} {
					if !strings.Contains(res.stderr, want) {
						t.Errorf("stderr does not mention %q:\n%s", want, res.stderr)
					}
				}
				if strings.Contains(strings.ToLower(res.stderr), "provider") &&
					!strings.Contains(res.stderr, "pull request") {
					t.Errorf("the refusal reads like a provider error; ADR-0007 decision 4 makes it "+
						"a startup usage error:\n%s", res.stderr)
				}
			})

			t.Run("an unknown model refuses before anything is written", func(t *testing.T) {
				dir := repoDir(t)
				writeConfig(t, dir, "model = \"qwen/not-a-model\"\n")

				before := treeOf(t, dir)
				res := run(t, bin, dir, nil, fe.argv()...)
				if res.code != exitUsage {
					t.Fatalf("exit code = %d, want %d. stderr:\n%s", res.code, exitUsage, res.stderr)
				}

				after := treeOf(t, dir)
				if !equal(before, after) {
					t.Errorf("a refused session changed the working tree.\nbefore: %v\nafter:  %v\n"+
						"resolution happens before the journal is opened and before SessionStarted is "+
						"written (ADR-0007 decision 4), so a session that never started leaves no record",
						before, after)
				}

				// Positive control: the comparison has to be able to see a file
				// appear, or the assertion above would pass over a journal.
				if err := os.WriteFile(filepath.Join(dir, "control"), []byte("x"), 0o644); err != nil {
					t.Fatalf("writing the control file: %v", err)
				}
				if equal(before, treeOf(t, dir)) {
					t.Fatal("positive control failed: the tree comparison did not notice a new file")
				}
			})

			t.Run("a config file chooses the model and the flag beats it", func(t *testing.T) {
				dir := repoDir(t)
				writeConfig(t, dir, "model = \"qwen/not-a-model\"\n")

				// The config file alone refuses.
				if res := run(t, bin, dir, nil, fe.argv()...); res.code != exitUsage {
					t.Errorf("with a bad model in the config file, exit code = %d, want %d. stderr:\n%s",
						res.code, exitUsage, res.stderr)
				}

				// The flag overrides it, and the run gets past resolution — far
				// enough to fail on something else. *What* it then fails on
				// differs per binary and changes as each is built out, so what
				// is asserted here is the distinction this case is about: exit
				// 4 rather than the exit 2 the config file alone produced, with
				// the binary saying why.
				res := run(t, bin, dir, nil, fe.argv("--model", defaultModelID)...)
				if res.code != exitHarness {
					t.Errorf("with --model overriding the config file, exit code = %d, want %d. stderr:\n%s",
						res.code, exitHarness, res.stderr)
				}
				if !strings.Contains(res.stderr, fe.name+":") {
					t.Errorf("stderr does not say which binary stopped or why:\n%s", res.stderr)
				}
			})

			t.Run("a resolved run still exits non-zero", func(t *testing.T) {
				// The REPL is KAN-793 and the bench runner is KAN-796. Adding
				// flag parsing must not turn an unimplemented binary into a
				// clean exit 0, which is how a broken harness passes a smoke
				// test.
				res := run(t, bin, repoDir(t), nil, fe.argv()...)
				if res.code != exitHarness {
					t.Errorf("exit code = %d, want %d. stderr:\n%s", res.code, exitHarness, res.stderr)
				}
			})

			t.Run("an unknown harness is a usage error", func(t *testing.T) {
				res := run(t, bin, repoDir(t), nil, fe.argv("--harness", "aggressive")...)
				if res.code != exitUsage {
					t.Errorf("exit code = %d, want %d. stderr:\n%s", res.code, exitUsage, res.stderr)
				}
				if !strings.Contains(res.stderr, "default") {
					t.Errorf("stderr does not list the compiled-in configurations:\n%s", res.stderr)
				}
			})

			t.Run("no environment variable is in the chain", func(t *testing.T) {
				// ADR-0007 decision 3. An ambient variable that silently chose
				// the model would be invisible in the shell history and in the
				// diff, which is the provenance failure the decision is about.
				env := []string{
					"KOPICODE_MODEL=qwen/not-a-model",
					"KOPICODE_HARNESS=aggressive",
					"MODEL=qwen/not-a-model",
					"HARNESS=aggressive",
				}
				res := run(t, bin, repoDir(t), env, fe.argv()...)
				if res.code != exitHarness {
					t.Errorf("an environment variable changed the resolution: exit code = %d, want %d. "+
						"stderr:\n%s", res.code, exitHarness, res.stderr)
				}
			})

			t.Run("--debug names the arm and where it came from", func(t *testing.T) {
				dir := repoDir(t)
				res := run(t, bin, dir, nil, fe.argv("--debug", "--model", defaultModelID)...)

				for _, want := range []string{
					"model_id=" + defaultModelID,
					"model_source=flag",
					"harness=default",
					"harness_config_hash=",
					"parasail/bf16",
				} {
					if !strings.Contains(res.stderr, want) {
						t.Errorf("--debug output does not contain %q:\n%s", want, res.stderr)
					}
				}
			})

			t.Run("diagnostics are off by default", func(t *testing.T) {
				// slog is off unless --debug, and on stderr rather than stdout
				// because --print owns stdout (CLAUDE.md).
				res := run(t, bin, repoDir(t), nil, fe.argv("--model", defaultModelID)...)
				if strings.Contains(res.stderr, "harness_config_hash") {
					t.Errorf("diagnostics appeared without --debug:\n%s", res.stderr)
				}
				if res.stdout != "" {
					t.Errorf("the binary wrote to stdout before the loop exists:\n%s", res.stdout)
				}
			})

			t.Run("--version still works", func(t *testing.T) {
				res := run(t, bin, repoDir(t), nil, "--version")
				if res.code != 0 {
					t.Errorf("exit code = %d, want 0. stderr:\n%s", res.code, res.stderr)
				}
				if strings.TrimSpace(res.stdout) == "" {
					t.Error("--version printed nothing")
				}
			})
		})
	}
}

// TestRunPrintRefusesBeforeItWritesAnything is KAN-794's exit-2 case asserted
// where the ordering is actually observable: from outside the process, over the
// working tree.
//
// `run --print` is the surface that will be pointed at a repository by a script,
// so "nothing was opened, locked or written" has to hold for it and not only for
// the REPL. Two conditions are refused here — a model this binary does not know,
// and a task nobody gave it — and neither may leave a journal, because a
// half-session record for a session that never started is precisely what
// ADR-0007 decision 4 orders the resolution to prevent.
//
// stdout is checked as well as the tree: a headless surface that printed
// anything at all on a refusal would hand a consumer a JSON stream describing a
// run that did not happen.
func TestRunPrintRefusesBeforeItWritesAnything(t *testing.T) {
	bin := build(t, frontEnds[0])

	tests := []struct {
		name string
		args []string
	}{
		{"an unknown model", []string{"run", "--print", "--model", "qwen/qwen3-coder-nxt", "fix it"}},
		{"no task", []string{"run", "--print"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := repoDir(t)
			before := treeOf(t, dir)

			res := run(t, bin, dir, nil, tc.args...)
			if res.code != exitUsage {
				t.Fatalf("exit code = %d, want %d (usage). stderr:\n%s", res.code, exitUsage, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("a refused run wrote to stdout, which --print owns:\n%s", res.stdout)
			}
			if !equal(before, treeOf(t, dir)) {
				t.Errorf("a refused run changed the working tree.\nbefore: %v\nafter:  %v",
					before, treeOf(t, dir))
			}
		})
	}
}

// TestTheDefaultModelIsTheOneTheBinaryUses holds the string this file repeats to
// the one the binary resolves, so the copy cannot go stale unnoticed.
//
// Without it, a changed default would leave this file asserting things about a
// model id nothing uses, and every case above would keep passing.
func TestTheDefaultModelIsTheOneTheBinaryUses(t *testing.T) {
	bin := build(t, frontEnds[0])
	res := run(t, bin, repoDir(t), nil, "--debug")

	if !strings.Contains(res.stderr, "model_id="+defaultModelID) {
		t.Errorf("the binary's default model is not %q; this test file's copy is stale:\n%s",
			defaultModelID, res.stderr)
	}
	if !strings.Contains(res.stderr, "model_source=default") {
		t.Errorf("with no flag and no config file the model did not come from the built-in "+
			"default:\n%s", res.stderr)
	}
}

// The session lock, docs/SLICE-1.md §8, asserted where it is actually
// observable: two real processes, one holding and one refused.
//
// It belongs in this file for the same reason the model-selection cases do.
// What has to be true is an exit code, a message on stderr, and a working tree
// that a refused invocation did not touch — none of which can be seen from
// inside the process, and the last of which is a claim about *ordering* rather
// than about any function's return value.

// lockedHolder is a kopicode session left running in dir, with its standard
// input held open so it sits at the prompt rather than exiting on EOF.
type lockedHolder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	dir   string
	pid   int
}

// startHolder launches a session and waits until it holds the working tree.
//
// Waiting on the lock file rather than on the process's output is deliberate:
// the file is the thing the refusal will quote, so a test that starts once the
// file describes a holder is a test synchronised on the state under test rather
// than on a printed line that could move.
func startHolder(t *testing.T, bin, dir string) *lockedHolder {
	t.Helper()

	cmd := exec.Command(bin)
	cmd.Dir = dir
	// Built rather than inherited, per CLAUDE.md. The key is a placeholder: the
	// session opens a client with it and never makes a request, and the point
	// of setting it is that a machine with no credential must not refuse for
	// *that* reason in a test about locking.
	cmd.Env = sessionEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("opening the holder's stdin: %v", err)
	}
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the holder: %v", err)
	}
	h := &lockedHolder{cmd: cmd, stdin: stdin, dir: dir}
	t.Cleanup(func() { h.stop() })

	deadline := time.Now().Add(20 * time.Second)
	for {
		pid := holderPID(filepath.Join(dir, ".kopicode", "lock"))
		if pid > 0 {
			h.pid = pid
			return h
		}
		if time.Now().After(deadline) {
			t.Fatalf("the holder never took %s.\nstdout:\n%s\nstderr:\n%s",
				filepath.Join(dir, ".kopicode", "lock"), out.String(), errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stop closes the holder's input, which is how the REPL is asked to finish, and
// waits for it to go.
func (h *lockedHolder) stop() {
	_ = h.stdin.Close()
	_ = h.cmd.Wait()
}

// holderPID reads the pid out of a lock file, or 0 when there is nothing to
// read yet.
//
// The struct is declared here rather than imported: ADR-0003's allowlist gives
// a front end internal/engine, internal/bench and internal/build, and
// internal/arch walks test files too. One field is a cheap price for not
// widening a boundary, and the lock file is a documented on-disk format.
func holderPID(path string) int {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var holder struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(body, &holder); err != nil {
		return 0
	}
	return holder.PID
}

// sessionEnv is the environment a session runs under: the PATH it needs to
// exist, and a credential so that the provider is not what stops it.
func sessionEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"OPENROUTER_API_KEY=not-a-real-key-and-never-sent",
	}
}

func TestASecondSessionInOneWorkingTreeIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the session lock is a documented no-op on Windows (docs/SLICE-1.md §8)")
	}
	bin := build(t, frontEnds[0])
	dir := repoDir(t)

	holder := startHolder(t, bin, dir)

	before := treeOf(t, dir)
	res := run(t, bin, dir, []string{"OPENROUTER_API_KEY=not-a-real-key-and-never-sent"})

	if res.code != exitHarness {
		t.Fatalf("exit code = %d, want %d. stderr:\n%s", res.code, exitHarness, res.stderr)
	}
	if res.stdout != "" {
		t.Errorf("a refused session wrote to stdout:\n%s", res.stdout)
	}

	// It names the holder. A pid on its own would be a weak answer — reused,
	// and silent about what is running — so the message carries the lock file
	// it can be checked against as well.
	for _, want := range []string{
		"already running",
		"pid " + strconv.Itoa(holder.pid),
		filepath.Join(dir, ".kopicode", "lock"),
	} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, res.stderr)
		}
	}

	// And it wrote no record. The refusal happens before the journal is opened,
	// so a session that never started leaves nothing a reader could mistake for
	// one that ran and did nothing.
	if after := treeOf(t, dir); !equal(before, after) {
		t.Errorf("a refused session changed the working tree.\nbefore: %v\nafter:  %v", before, after)
	}

	// The tree is free once the holder goes. flock is held by the open file
	// description, so nothing has to clean up after it — which is the property
	// that keeps a crashed session from bricking a repository.
	holder.stop()
	res = run(t, bin, dir, []string{"OPENROUTER_API_KEY=not-a-real-key-and-never-sent"})
	if strings.Contains(res.stderr, "already running") {
		t.Errorf("the working tree was still locked after the holder exited:\n%s", res.stderr)
	}
}

// TestSessionsInSeparateWorkingTreesDoNotBlockEachOther is the other half of
// the requirement, and the half that decides whether `make bench-smoke` runs at
// all: the bench runner puts every task in its own worktree and runs ten at
// once, so a lock keyed on anything they share would serialise it or deadlock
// it outright.
func TestSessionsInSeparateWorkingTreesDoNotBlockEachOther(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the session lock is a documented no-op on Windows (docs/SLICE-1.md §8)")
	}
	bin := build(t, frontEnds[0])

	first := startHolder(t, bin, repoDir(t))
	second := startHolder(t, bin, repoDir(t))

	if first.pid == second.pid {
		t.Fatal("the two holders are the same process")
	}
	// startHolder fails the test if a session never reaches its lock, so
	// reaching here with two live holders is the assertion.
	first.stop()
	second.stop()
}

// build compiles one front end into the test's temp directory.
func build(t *testing.T, fe frontEnd) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), fe.name)
	if runtime.GOOS == "windows" {
		out += ".exe"
	}

	cmd := exec.Command("go", "build", "-o", out, fe.pkg)
	// The module root: `go build ./cmd/...` is relative to it, and this test
	// package sits one level down.
	cmd.Dir = repoRoot(t)
	cmd.Env = toolchainEnv()

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", fe.pkg, err, out)
	}
	return out
}

// toolchainEnv is the environment `go build` runs under.
//
// It is built rather than inherited, because an inherited GOFLAGS, GOOS or
// GOARCH silently changes what gets compiled and the command still reads
// correctly (CLAUDE.md, and internal/arch/subprocess_test.go). The rest of the
// ambient environment is kept deliberately: GOCACHE, GOMODCACHE, PATH and HOME
// are how the toolchain finds itself, and a build without them is a build of
// nothing.
func toolchainEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "GOFLAGS="),
			strings.HasPrefix(kv, "GOOS="),
			strings.HasPrefix(kv, "GOARCH="):
			continue
		}
		env = append(env, kv)
	}
	return env
}

type result struct {
	code   int
	stdout string
	stderr string
}

// run executes a front end in dir with exactly the environment given, plus the
// PATH it needs to exist at all.
//
// The environment is built from nothing rather than inherited, which is what
// makes the "no environment variable in the chain" case meaningful: the child
// sees the variables this test chose and no others.
func run(t *testing.T, bin, dir string, env []string, args ...string) result {
	t.Helper()

	var stdout, stderr strings.Builder
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
	default:
		t.Fatalf("running %s %v: %v", bin, args, err)
	}

	return result{code: cmd.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

// repoDir is a temp directory that looks like a repository, so the config search
// stops there rather than walking out into whatever is above the temp directory.
func repoDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("creating the fixture repository: %v", err)
	}
	return dir
}

func writeConfig(t *testing.T, dir, content string) {
	t.Helper()

	cfgDir := filepath.Join(dir, ".kopicode")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", cfgDir, err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing the config file: %v", err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	return root
}

func treeOf(t *testing.T, dir string) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
