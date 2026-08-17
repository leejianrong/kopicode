package lock_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/lock"
	"github.com/leejianrong/kopicode/internal/repo"
)

// helperEnvVar is how the re-executed test binary knows it is the helper rather
// than the suite.
const helperEnvVar = "KOPICODE_LOCK_HELPER_ROOT"

// TestMain lets this binary be re-executed as a lock holder that dies.
//
// The alternative — a second Acquire in this process — proves exclusion but not
// the property the whole design rests on, which is that a *dead* holder's lock
// goes away without anyone cleaning up. Only a real process exit can show that.
func TestMain(m *testing.M) {
	if root := os.Getenv(helperEnvVar); root != "" {
		holdAndDie(root)
		return
	}
	os.Exit(m.Run())
}

// holdAndDie takes the lock, says so on stdout, and exits without releasing it.
//
// Exiting mid-hold is the point: nothing here calls Release, and no deferred
// cleanup runs, so the only thing that can free the lock is the kernel
// reclaiming the open file description.
func holdAndDie(root string) {
	l, err := lock.Acquire(root, lock.Holder{Session: "helper", Record: filepath.Join(root, "record")})
	if err != nil {
		os.Stderr.WriteString("helper: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString(l.Path())
	os.Exit(0)
}

func TestASecondAcquireInTheSameTreeIsRefused(t *testing.T) {
	requireExclusion(t)

	root := t.TempDir()
	first, err := lock.Acquire(root, lock.Holder{Session: "s1", Record: "/tmp/record/s1"})
	if err != nil {
		t.Fatalf("the first Acquire failed: %v", err)
	}
	t.Cleanup(func() { _ = first.Release() })

	_, err = lock.Acquire(root, lock.Holder{Session: "s2"})
	if err == nil {
		t.Fatal("a second session took the lock; docs/SLICE-1.md §8 refuses it")
	}
	if !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("the refusal does not wrap ErrHeld: %v", err)
	}

	var held *lock.HeldError
	if !errors.As(err, &held) {
		t.Fatalf("the refusal is not a *HeldError: %v", err)
	}
	if held.Holder.PID != os.Getpid() {
		t.Errorf("holder pid = %d, want this process, %d", held.Holder.PID, os.Getpid())
	}
	if held.Holder.Session != "s1" {
		t.Errorf("holder session = %q, want %q", held.Holder.Session, "s1")
	}
	if held.Holder.Record != "/tmp/record/s1" {
		t.Errorf("holder record = %q, want the first session's", held.Holder.Record)
	}
	if held.Holder.Started.IsZero() {
		t.Error("the holder record carries no start time; a pid alone cannot be checked against ps")
	}
	if held.Holder.Program == "" {
		t.Error("the holder record does not say which binary is running")
	}
	if held.Path != filepath.Join(root, lock.StateDir, lock.FileName) {
		t.Errorf("lock path = %q, want %s", held.Path,
			filepath.Join(root, lock.StateDir, lock.FileName))
	}
}

// TestTheRefusalNamesTheHolder holds the *message* to the requirement, because
// "names the holder" is a property of what a user reads and not of a struct
// field they never see.
func TestTheRefusalNamesTheHolder(t *testing.T) {
	requireExclusion(t)

	root := t.TempDir()
	first, err := lock.Acquire(root, lock.Holder{Session: "20260817T090000Z-abcd1234", Record: "/w/.kopicode/sessions/x"})
	if err != nil {
		t.Fatalf("the first Acquire failed: %v", err)
	}
	t.Cleanup(func() { _ = first.Release() })

	_, err = lock.Acquire(root, lock.Holder{})
	if err == nil {
		t.Fatal("the second Acquire succeeded")
	}

	msg := err.Error()
	for _, want := range []string{
		root,
		"pid ",
		"20260817T090000Z-abcd1234",
		"/w/.kopicode/sessions/x",
		first.Path(),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
}

func TestReleaseFreesTheTree(t *testing.T) {
	requireExclusion(t)

	root := t.TempDir()
	first, err := lock.Acquire(root, lock.Holder{Session: "s1"})
	if err != nil {
		t.Fatalf("the first Acquire failed: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Twice, because a deferred release and an explicit one both happen in the
	// engine's teardown and must not disagree.
	if err := first.Release(); err != nil {
		t.Fatalf("the second Release reported an error: %v", err)
	}

	second, err := lock.Acquire(root, lock.Holder{Session: "s2"})
	if err != nil {
		t.Fatalf("the tree was not free after Release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestADeadHoldersLockDoesNotBrickTheTree is the reason there is no staleness
// heuristic in this package.
//
// A holder that exits without releasing leaves the lock file on disk with its
// description still in it. If that were enough to keep the tree locked, a
// crashed session would brick a repository until someone found and deleted a
// file they have never heard of. flock is held by the open file description, so
// the kernel drops it when the process dies — and this test is what turns that
// sentence into a fact about the shipped binary.
func TestADeadHoldersLockDoesNotBrickTheTree(t *testing.T) {
	requireExclusion(t)

	root := t.TempDir()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	cmd := exec.Command(self)
	cmd.Dir = root
	cmd.Env = helperEnv(root)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the helper holder failed: %v\n%s", err, out)
	}

	path := strings.TrimSpace(string(out))
	if path == "" {
		t.Fatal("the helper did not report the lock path, so it may never have taken the lock")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the lock file the dead holder left: %v", err)
	}
	if !strings.Contains(string(body), "helper") {
		t.Fatalf("the dead holder left no description behind, so this test is not "+
			"exercising the case it claims to:\n%s", body)
	}

	// The file is still there, still describing a process that no longer
	// exists. Acquiring must work anyway.
	l, err := lock.Acquire(root, lock.Holder{Session: "after-the-crash"})
	if err != nil {
		t.Fatalf("a dead holder's lock file blocked a new session; a crashed session "+
			"must not brick the working tree: %v", err)
	}
	t.Cleanup(func() { _ = l.Release() })

	fresh, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the lock file: %v", err)
	}
	if strings.Contains(string(fresh), "helper") {
		t.Errorf("the new holder did not replace the dead one's description:\n%s", fresh)
	}
	if !strings.Contains(string(fresh), "after-the-crash") {
		t.Errorf("the lock file does not describe the live holder:\n%s", fresh)
	}
}

// helperEnv is the environment the re-executed test binary runs under.
//
// Built rather than inherited, per CLAUDE.md and internal/arch/subprocess_test.go:
// this process is a test binary, so an inherited GOFLAGS or TMPDIR changes what
// the child does while the command still reads correctly. The helper needs
// exactly one variable to know what it is, plus PATH so that the operating
// system can start it at all.
func helperEnv(root string) []string {
	return []string{
		helperEnvVar + "=" + root,
		"PATH=" + os.Getenv("PATH"),
	}
}

// TestTheLockFileIsNotRemovedOnRelease pins the decision in the package
// comment. Unlinking it is the classic race — a process that has opened the
// file and is about to lock it ends up holding a lock on an inode nobody else
// can reach — so a future cleanup that "tidies up after itself" fails here
// rather than in production.
func TestTheLockFileIsNotRemovedOnRelease(t *testing.T) {
	root := t.TempDir()
	l, err := lock.Acquire(root, lock.Holder{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(l.Path()); err != nil {
		t.Fatalf("Release removed the lock file, which reopens the unlink race "+
			"the package comment describes: %v", err)
	}
}

// TestTheStateDirIsTheSameEverywhere holds the three copies of ".kopicode" to
// each other.
//
// Each package declares it rather than importing a neighbour, so that the
// import arrows keep pointing one way; the cost of that is three constants that
// could drift, and this is what stops them. A test can import all three because
// it is not part of anyone's dependency graph.
func TestTheStateDirIsTheSameEverywhere(t *testing.T) {
	if lock.StateDir != journal.StateDir {
		t.Errorf("lock.StateDir = %q, journal.StateDir = %q", lock.StateDir, journal.StateDir)
	}
	if lock.StateDir != repo.StateDir {
		t.Errorf("lock.StateDir = %q, repo.StateDir = %q", lock.StateDir, repo.StateDir)
	}
}

// requireExclusion skips a case that only means something where the lock
// excludes anything.
//
// Windows gets a documented no-op (lock_other.go, docs/SLICE-1.md §8), so
// asserting a refusal there would assert the degradation away. The skip names
// it so a Windows run reports the shortfall rather than a clean suite.
func requireExclusion(t *testing.T) {
	t.Helper()
	if !lock.Supported {
		t.Skip("the session lock is a no-op on this platform; two sessions in one working " +
			"tree are not excluded (docs/SLICE-1.md §8)")
	}
}
