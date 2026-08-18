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
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// recordNotice is what the REPL prints once its session is open, and it is the
// signal this file synchronises on. See [startHolder] for why, which is KAN-908.
const recordNotice = "record: "

// lockedHolder is a kopicode session left running in dir, with its standard
// input held open so it sits at the prompt rather than exiting on EOF.
type lockedHolder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *watcher
	errOut *watcher

	dir string
	// pid and record identify the holder: the pid the refusal must name, and
	// the record directory that must be the only one in the tree.
	pid    int
	record string
	// settled is the tree as it stood once it stopped changing, and is what a
	// caller compares against afterwards. Taking it here rather than in the
	// caller is deliberate: it is the last of the two identical walks that
	// [awaitQuiescent] made, so there is no gap between "the tree has settled"
	// and "this is what it settled to".
	settled []string

	// done is closed when the process has been reaped. waitErr is what Wait
	// said, kept so that both stop and the readiness wait can consult it
	// without either calling Wait twice.
	done    chan struct{}
	waitErr error

	stopOnce sync.Once
}

// startHolder launches a session and waits until it is **fully** started.
//
// Fully started, and not merely holding the lock, is the whole point of this
// function, and getting it wrong is KAN-908: an earlier version polled for
// .kopicode/lock to describe a holder and returned as soon as it did. That is a
// real signal — the lock file is written inside lock.Acquire, and the refusal
// quotes it — but it is the *first* thing a session writes, not the last.
// engine.Open goes on to open the tool root and then the journal, so a caller
// that snapshotted the tree at that moment caught the holder's own
// .kopicode/sessions/<id>/events.jsonl appearing a few milliseconds later and
// attributed it to whatever ran next. It failed about one full-package run in
// three, and the record it blamed on the refused process was provably the
// holder's: the directory name matched the `session` field in the lock file the
// holder itself had written.
//
// So the signal is the REPL's own "record:" notice, which main.go prints after
// engine.Open has returned — which is after Start appended and fsynced
// SessionStarted. Confirming the file at the announced path exists turns the
// notice from a printed line into a fact about the tree.
//
// And then the tree is watched until it holds still, which is KAN-909 and the
// part that matters most here. The notice tells us the session is up; it does
// not tell us the session has finished writing, and the belief that it has is
// another inference about another package's internals — true today, silently
// false the day the REPL grows a history file or a heartbeat. [awaitQuiescent]
// measures it instead, so that a future idle write produces a legible failure
// naming the paths rather than an intermittent one blaming the process that ran
// next. The argument for why the tree *should* settle is still worth having, and
// it is this: the REPL and the line editor touch no files, and .kopicode/blobs
// is created lazily by a spill an idle session never performs. It is now an
// explanation of the check's result rather than a substitute for it.
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
	h := &lockedHolder{
		cmd:    cmd,
		stdin:  stdin,
		out:    newWatcher(recordNotice),
		errOut: newWatcher(""),
		dir:    dir,
		done:   make(chan struct{}),
	}
	cmd.Stdout = h.out
	cmd.Stderr = h.errOut

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the holder: %v", err)
	}
	go func() {
		h.waitErr = cmd.Wait()
		close(h.done)
	}()
	t.Cleanup(h.stop)

	// Three outcomes, and only one of them is a hang: the notice arrives, the
	// holder dies before printing it, or neither happens. Selecting on the
	// process as well as on the notice is what turns a crashed holder into a
	// failure that shows its stderr instead of a test that sits for the whole
	// timeout.
	select {
	case <-h.out.seen:
	case <-h.done:
		t.Fatalf("the holder exited before it opened a session (%v).\nstdout:\n%s\nstderr:\n%s",
			h.waitErr, h.out.text(), h.errOut.text())
	case <-time.After(30 * time.Second):
		t.Fatalf("the holder never announced its record.\nstdout:\n%s\nstderr:\n%s",
			h.out.text(), h.errOut.text())
	}

	h.record = recordPath(h.out.text())
	if h.record == "" {
		t.Fatalf("could not read the record path out of the holder's output:\n%s", h.out.text())
	}
	// The notice is printed after engine.Open returned, so the record exists by
	// now. Checking says so as a fact about the filesystem rather than as an
	// inference about the order of two statements in another package.
	events := filepath.Join(h.record, "events.jsonl")
	if _, err := os.Stat(events); err != nil {
		t.Fatalf("the holder announced %s but %s is not there yet, so the readiness "+
			"signal does not mean what this test needs it to mean: %v", h.record, events, err)
	}

	h.pid = holderPID(filepath.Join(dir, ".kopicode", "lock"))
	if h.pid <= 0 {
		t.Fatalf("the holder is running but %s does not describe it",
			filepath.Join(dir, ".kopicode", "lock"))
	}

	// And then the measured half. Everything above is an argument that the
	// holder has finished writing; this is the check that it has (KAN-909).
	h.settled = awaitQuiescent(t, dir)
	return h
}

// stop closes the holder's input, which is how the REPL is asked to finish, and
// waits for it to go. It is safe to call more than once: the test calls it
// explicitly to prove the tree is freed, and t.Cleanup calls it again.
func (h *lockedHolder) stop() {
	h.stopOnce.Do(func() { _ = h.stdin.Close() })
	<-h.done
}

// watcher collects a child's output and signals once a marker has appeared in
// it.
//
// It exists because the readiness signal and the failure message are the same
// bytes, read from two goroutines: os/exec's copier writes, and the test reads.
// A strings.Builder shared between them is a data race that -race would find on
// whichever run happened to look. An empty want never fires, which is what
// stderr wants — collected for the message, never waited on.
type watcher struct {
	want string
	seen chan struct{}

	mu   sync.Mutex
	buf  strings.Builder
	once sync.Once
}

func newWatcher(want string) *watcher {
	return &watcher{want: want, seen: make(chan struct{})}
}

func (w *watcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	hit := w.want != "" && strings.Contains(w.buf.String(), w.want)
	w.mu.Unlock()
	if hit {
		w.once.Do(func() { close(w.seen) })
	}
	return len(p), nil
}

func (w *watcher) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// recordPath pulls the announced record directory out of the REPL's output.
func recordPath(out string) string {
	for _, line := range strings.Split(out, "\n") {
		i := strings.Index(line, recordNotice)
		if i < 0 {
			continue
		}
		if p := strings.TrimSpace(line[i+len(recordNotice):]); p != "" {
			return p
		}
	}
	return ""
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

// sessionDirs lists the session record directories under dir.
//
// It backs an assertion that does not depend on timing at all: whatever else is
// racing, the number of records in a tree that has hosted one session and one
// refusal is one. See [TestASecondSessionInOneWorkingTreeIsRefused].
func sessionDirs(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(dir, ".kopicode", "sessions"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("listing session records: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
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

	// The tree as it stood when it stopped changing, not as it stood when the
	// holder said it was ready. startHolder waited for the second (KAN-909), so
	// there is nothing left here to race with.
	before := holder.settled
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
	//
	// The holder's session id goes in the message because the one way this
	// assertion has ever failed was a readiness race in this file rather than a
	// defect in the lock, and the two are told apart by whose record appeared
	// (KAN-908). If the extra directory is the holder's, the bug is here.
	if after := treeOf(t, dir); !equal(before, after) {
		t.Errorf("a refused session changed the working tree.\nbefore: %v\nafter:  %v\n"+
			"the holder's own record is %s", before, after, holder.record)
	}

	// The same property again, and this one cannot race: a tree that has hosted
	// one session and one refusal holds exactly one record, the holder's. It is
	// worth stating separately because it is the ADR-0007 decision 4 property in
	// its own terms — no half-session record for a session that never started —
	// where the comparison above is a tree snapshot that any concurrent write
	// could disturb.
	records := sessionDirs(t, dir)
	if len(records) != 1 || filepath.Join(dir, ".kopicode", "sessions", records[0]) != holder.record {
		t.Errorf("session records under %s are %v; want only the holder's, %s",
			dir, records, holder.record)
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

// toolchainPassThrough is what `go build` needs to find itself and its
// caches. Everything else is dropped, and the drops are the whole point of
// the list.
//
// This mirrors internal/build's toolchainPassThrough (inject_test.go) and
// internal/corpus's passThrough (oracle_integration_test.go) rather than
// importing either: internal/build's copy is an unexported variable in a
// _test.go file, so it is not reachable even though cmd/kopicode already
// imports the internal/build package proper for --version, and
// internal/corpus is a sibling test package with the same problem. Three
// call sites, three copies — KAN-854 is the record of that being deliberate,
// so read this before adding a fourth builder rather than reaching for
// os.Environ() again.
//
// KAN-854 replaced this list's predecessor, a denylist that dropped only
// GOFLAGS, GOOS and GOARCH from the inherited environment. A denylist is a
// claim that every variable able to change what `go build` produces has been
// enumerated, and the Go toolchain keeps adding ones that were not on it —
// GOEXPERIMENT changes which language/runtime experiments compile in,
// GOPRIVATE changes module resolution, GOTOOLCHAIN changes which compiler
// runs, and GOWORK changes whether a workspace file overrides the module
// this test means to build. An allowlist does not need to have anticipated
// any of them; it need only be missing an entry the build turns out to
// require, which fails loudly as a broken test rather than silently as a
// build that used the wrong toolchain.
var toolchainPassThrough = []string{
	"PATH", "HOME", "TMPDIR", "TEMP", "TMP",
	"GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOPROXY",
	// GOTOOLCHAIN decides which compiler runs. It is passed through rather
	// than pinned because the `go test` process that got here resolved it
	// the same way, and forcing a different answer would test a build
	// nobody makes.
	"GOTOOLCHAIN",
	// Windows needs these to start a process at all.
	"SystemRoot", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "ComSpec",
}

// toolchainEnv is the environment `go build` runs under.
//
// GOFLAGS is set empty rather than merely omitted, matching internal/build's
// toolchainEnv: an allowlist keeps an inherited GOFLAGS out of the child's
// environment, but `go env -w GOFLAGS=…` writes it to a file the toolchain
// reads on its own, which no allowlist over the environment can reach; an
// explicit empty value in the environment overrides that file.
func toolchainEnv() []string {
	env := make([]string, 0, len(toolchainPassThrough)+1)
	for _, name := range toolchainPassThrough {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return append(env, "GOFLAGS=")
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
	return pathsOf(scan(t, dir))
}

func pathsOf(entries []treeEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.path)
	}
	return out
}

// treeEntry is one path in a walked tree, with the size that path had.
//
// The size is not part of the byte-for-byte comparison — [treeOf] drops it, and
// the assertions it feeds are about paths appearing. It is here for
// [awaitQuiescent], which has to be *more* sensitive than the assertion it
// stands in front of: a precondition that can miss a write its assertion would
// catch is not a precondition. An append to an existing file moves a size and no
// path, and that is the shape of the idle write — a log, a history file — this
// is guarding against.
type treeEntry struct {
	path string
	size int64
}

// scan walks dir and reports every path under it, sorted, directories rendered
// with a trailing slash.
//
// An entry that vanishes between the walk finding it and the stat reading it is
// skipped rather than fatal. Nothing in these fixtures deletes anything, but
// [awaitQuiescent] walks the same tree repeatedly while another process is live
// in it, and a walker that a race can kill is a poor instrument for detecting
// one.
func scan(t *testing.T, dir string) []treeEntry {
	t.Helper()

	var out []treeEntry
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			out = append(out, treeEntry{path: rel + "/"})
			return nil
		}
		info, infoErr := d.Info()
		if errors.Is(infoErr, fs.ErrNotExist) {
			return nil
		}
		if infoErr != nil {
			return infoErr
		}
		out = append(out, treeEntry{path: rel, size: info.Size()})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// How long a tree has to hold still before [awaitQuiescent] believes it, and how
// long it will wait for that to happen.
//
// **The window: 100ms between two identical walks.** The gap it has to cover is
// the one KAN-908 measured — a holder wrote .kopicode/lock and then its journal
// a few milliseconds later, and the test looked in between. 100ms is about two
// orders of magnitude above that, which is the margin worth having on a shared
// CI runner where a scheduling stall is the usual reason a gap widens. Going
// wider buys progressively less: a write landing more than 100ms after a session
// announced itself is not a startup write but a background one, and the timeout
// is what deals with those.
//
// **The cost: one extra 100ms per holder.** 100ms for the refusal case and 200ms
// for the two-holder case, against a package that runs for about fifteen
// seconds. It is paid once per holder rather than per poll, because the common
// case settles on the first pair.
//
// **The timeout: 10s**, which is a hundred chances to hold still. A tree that
// has not managed one by then is not slow, it is being written to continuously,
// and that is a finding rather than a reason to carry on.
const (
	quiescenceWindow  = 100 * time.Millisecond
	quiescenceTimeout = 10 * time.Second
)

// awaitQuiescent blocks until dir stops changing, and reports the settled tree.
//
// It exists because the alternative is an argued invariant, and this file has
// now been wrong about one of those once already in this exact place. KAN-908
// replaced "the lock file is written, so the session is up" with "the notice is
// printed, so nothing more will be written" — both inferences about another
// package's internals, and the second goes stale in silence the day the REPL
// grows a history file, a status cache, a heartbeat, or any lazily created
// directory. It would then fail the way the first did: intermittently, and
// blaming the process that merely ran next.
//
// So the invariant is measured rather than argued. Two consecutive identical
// walks is the weakest check that tells "settled" apart from "between two
// writes", and it absorbs the ordinary case rather than failing on it: a
// one-shot write landing just after the notice moves the tree once, the next
// pair agrees, and the snapshot returned is the one that includes it. Only a
// tree that never holds still fails, and it fails saying what kept moving.
func awaitQuiescent(t *testing.T, dir string) []string {
	t.Helper()

	deadline := time.Now().Add(quiescenceTimeout)
	prev := scan(t, dir)
	for {
		time.Sleep(quiescenceWindow)
		cur := scan(t, dir)
		if sameEntries(prev, cur) {
			return pathsOf(cur)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never settled: it was still changing after %s, so a byte-for-byte "+
				"comparison across another process's lifetime cannot mean anything.\n"+
				"still changing:\n%s\n"+
				"something in the session's idle path is writing to the tree. That is the thing "+
				"to fix; this check exists to name it rather than let it surface later as an "+
				"intermittent failure blaming whichever process ran next.",
				dir, quiescenceTimeout, strings.Join(changes(prev, cur), "\n"))
		}
		prev = cur
	}
}

func sameEntries(a, b []treeEntry) bool {
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

// changes describes what moved between two walks, so the timeout names the paths
// to go and look at rather than only reporting that something happened.
func changes(before, after []treeEntry) []string {
	was := make(map[string]int64, len(before))
	for _, e := range before {
		was[e.path] = e.size
	}
	seen := make(map[string]bool, len(after))

	var out []string
	for _, e := range after {
		seen[e.path] = true
		size, existed := was[e.path]
		switch {
		case !existed:
			out = append(out, "  appeared: "+e.path)
		case size != e.size:
			out = append(out, fmt.Sprintf("  grew:     %s (%d -> %d bytes)", e.path, size, e.size))
		}
	}
	for _, e := range before {
		if !seen[e.path] {
			out = append(out, "  vanished: "+e.path)
		}
	}
	if len(out) == 0 {
		// Unreachable unless sameEntries and this function disagree, which is
		// worth saying out loud rather than printing an empty list.
		out = append(out, "  (the two walks differ but no path did; the comparison is wrong)")
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
