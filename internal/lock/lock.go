// Package lock is the one-session-per-working-tree advisory lock that
// docs/SLICE-1.md §8 requires.
//
// # Why a lock at all
//
// Two kopicode sessions in one working tree interleave edits and then write two
// session records each claiming to describe one filesystem. That is the drift
// ADR-0002 exists to prevent, arriving through the back door: the journal is
// the record of what the agent decided, git is the record of the tree, and
// neither can be read honestly if a second agent was editing underneath. So the
// second session is refused rather than merged.
//
// # Keyed on the working tree, not on the repository
//
// The distinction decides whether `make bench-smoke` runs or deadlocks. A
// linked worktree shares .git/config, the object store and the ref store with
// the repository it came from — only HEAD and the index are private — so "one
// session per repo" is genuinely ambiguous, and the reading that keys on the
// shared git directory would serialise the ten concurrent task worktrees the
// bench runner creates on purpose. What actually collides is the *filesystem*
// two sessions edit, which is the working tree. So the lock file lives at
// <work tree root>/.kopicode/lock, and callers resolve that root with
// [github.com/leejianrong/kopicode/internal/repo.WorkTreeRoot] rather than
// passing whatever directory they happen to be in — a session started in a
// subdirectory must collide with one started at the root, because they share
// the tree that snapshots are taken of.
//
// # flock, and what it buys
//
// The mechanism is flock(2) on unix and a documented no-op elsewhere; see
// lock_unix.go and lock_other.go. The property that matters most is one nobody
// has to implement: an advisory lock lives as long as the open file
// description, so the kernel releases it when the process exits — on a clean
// return, on an unrecovered panic, on SIGTERM, on SIGKILL, and on the machine
// losing power. A crashed session therefore cannot brick a repository, and the
// lock file being left behind on disk is not staleness: it is an empty
// mailbox that the next Acquire truncates and rewrites.
//
// That is why there is deliberately **no** staleness heuristic here — no
// timeout, no liveness probe on the recorded pid, no "the lock looks old so
// take it anyway". Every one of those is a way to break the exclusion the
// package exists to provide, in exchange for a property flock already gives.
//
// # The lock file is never unlinked
//
// [Lock.Release] closes the file and leaves it in place. Removing it would
// introduce the classic unlink race: a process that opened the file, and is
// about to lock it, ends up holding a lock on an inode nobody else can reach,
// and two sessions run believing they are alone. The file is a few hundred
// bytes inside .kopicode/, which is already excluded from git.
package lock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// StateDir is kopicode's per-working-tree state directory.
	//
	// It duplicates journal.StateDir and repo.StateDir deliberately, for the
	// reason repo.StateDir states: this package is a leaf that shells out to
	// nothing and imports nothing from the module, and importing the journal
	// to share a ten-character constant would invert the import arrow. The
	// duplication is not left to drift — TestTheStateDirIsTheSameEverywhere
	// holds all three to each other.
	StateDir = ".kopicode"

	// FileName is the lock file, inside [StateDir].
	FileName = "lock"

	dirPerm  = 0o700
	filePerm = 0o600
)

// ErrHeld reports that another live process holds the working tree's lock. It
// is the cause every refusal wraps, so a front end can recognise one with
// errors.Is without matching on prose.
var ErrHeld = errors.New("another kopicode session is already running in this working tree")

// Supported reports whether this platform actually excludes a second session.
//
// It is false on Windows, where [Acquire] always succeeds. That is the
// degradation docs/SLICE-1.md §8 chose explicitly, matching satay-runtime's
// posture; see lock_other.go for what it costs and what would fix it. It is
// exported so a caller can say so in a diagnostic rather than a reader having
// to infer it from a lock that never refuses.
const Supported = supported

// Holder describes the process that holds the lock, as recorded in the lock
// file and reported back to whoever is refused.
//
// # Why not just the pid
//
// A pid alone is a weak answer to "who has this". It is reused, so a user who
// checks finds either nothing or an unrelated process; and even when it is
// live it says nothing about what is running or where its record is, which is
// what someone locked out actually wants to know. The four facts here answer
// that between them:
//
//   - Started disambiguates a reused pid. If `ps` shows that pid starting at a
//     different time, the number has been recycled and the holder is gone —
//     which flock already told us cannot be true, so this is the field that
//     makes the report checkable rather than merely assertive.
//   - Host matters wherever a pid is not meaningful locally: a bind-mounted
//     working tree seen from a container has a pid namespace of its own, and a
//     tree on a network filesystem may be held from another machine entirely.
//   - Program says which front end it is, so `kopibench` running the corpus
//     does not read as a stray REPL.
//   - Record points at the other session's journal, which is the one thing
//     that lets the user find out what it is doing rather than only that it
//     exists.
//
// PID, Host, Started and Program are facts about the running process and are
// filled in by [Acquire]; a caller cannot supply them, because a lock file that
// could describe someone else would be worse than one that says nothing.
type Holder struct {
	PID     int       `json:"pid"`
	Host    string    `json:"host"`
	Program string    `json:"program"`
	Started time.Time `json:"started"`

	// Session is the holder's session id. Optional: a caller that has not
	// minted one yet leaves it empty.
	Session string `json:"session,omitempty"`
	// Record is the directory the holder's journal lives in, so a user who is
	// refused can go and read what the other session is doing. Optional.
	Record string `json:"record,omitempty"`
}

// String renders the holder for a human, on one line.
func (h Holder) String() string {
	var b strings.Builder
	if h.Program != "" {
		b.WriteString(h.Program + ", ")
	}
	fmt.Fprintf(&b, "pid %d", h.PID)
	if h.Host != "" {
		b.WriteString(" on " + h.Host)
	}
	if !h.Started.IsZero() {
		b.WriteString(", started " + h.Started.UTC().Format(time.RFC3339))
	}
	if h.Session != "" {
		b.WriteString(", session " + h.Session)
	}
	return b.String()
}

// HeldError is the refusal, carrying everything the message needs.
type HeldError struct {
	// Root is the working tree that is already in use.
	Root string
	// Path is the lock file itself.
	Path string
	// Holder is what the lock file said. A zero PID means the file could not
	// be read or was caught mid-write; the refusal still stands, because the
	// fact that the lock is held comes from the kernel and not from the file.
	Holder Holder
}

func (e *HeldError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", ErrHeld.Error(), e.Root)
	if e.Holder.PID > 0 {
		b.WriteString("\n  holder: " + e.Holder.String())
	} else {
		b.WriteString("\n  holder: unknown — " + e.Path + " could not be read")
	}
	if e.Holder.Record != "" {
		b.WriteString("\n  record: " + e.Holder.Record)
	}
	b.WriteString("\n  lock:   " + e.Path +
		" (held for as long as that process lives; it is released automatically if it dies)")
	return b.String()
}

// Unwrap makes errors.Is(err, ErrHeld) true for every refusal.
func (e *HeldError) Unwrap() error { return ErrHeld }

// Lock is a held advisory lock. The zero value is not usable; get one from
// [Acquire].
type Lock struct {
	root string
	path string

	once sync.Once
	err  error
	f    *os.File
}

// Acquire takes the working tree's lock, or refuses.
//
// root is the working tree root — see the package comment on why that and not
// the git directory. self carries the caller's half of the holder description;
// the process facts are filled in here.
//
// A refusal is a *[HeldError] wrapping [ErrHeld]. Every other error is a
// filesystem failure, and both are returned before anything else about a
// session exists: this call creates .kopicode/ and the lock file and writes
// nothing else, so an invocation that is refused has created neither, because
// the only way to be refused is for a holder to have created them already.
func Acquire(root string, self Holder) (*Lock, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("lock: resolving %s: %w", root, err)
	}

	dir := filepath.Join(abs, StateDir)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("lock: creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, FileName)

	// O_TRUNC is deliberately absent. Truncating on open would erase the
	// holder's description *before* asking whether there is a holder, so the
	// refusal this call is about to produce would name nobody. The truncate
	// happens after the lock is ours, below.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, fmt.Errorf("lock: opening %s: %w", path, err)
	}

	ok, err := tryLock(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock: locking %s: %w", path, err)
	}
	if !ok {
		held := &HeldError{Root: abs, Path: path, Holder: readHolder(f)}
		_ = f.Close()
		return nil, held
	}

	l := &Lock{root: abs, path: path, f: f}
	if err := l.describe(self); err != nil {
		// The lock is genuinely ours and the session could run; but a lock
		// nobody can be named from is a lock whose refusal message is useless,
		// and a write failure here means something is wrong with .kopicode/
		// that the journal is about to hit anyway. Fail while nothing has been
		// opened.
		_ = l.Release()
		return nil, err
	}
	return l, nil
}

// Root reports the working tree this lock covers.
func (l *Lock) Root() string { return l.root }

// Path reports the lock file.
func (l *Lock) Path() string { return l.path }

// Release gives the lock up.
//
// It is safe to call more than once and returns the same answer each time, so a
// deferred release and an explicit one cannot disagree. Closing the file is
// what releases the kernel lock; the unlock call before it is not strictly
// needed and is made anyway so that the release is explicit in a stack trace
// rather than a side effect of a close.
//
// The file is not removed. See the package comment: unlinking it is how two
// processes end up holding locks on different inodes.
func (l *Lock) Release() error {
	l.once.Do(func() {
		if l.f == nil {
			return
		}
		unlockErr := unlock(l.f)
		closeErr := l.f.Close()
		l.err = errors.Join(unlockErr, closeErr)
		if l.err != nil {
			l.err = fmt.Errorf("lock: releasing %s: %w", l.path, l.err)
		}
	})
	return l.err
}

// describe writes the holder record, replacing whatever a previous holder left.
func (l *Lock) describe(self Holder) error {
	self.PID = os.Getpid()
	self.Host = hostname()
	self.Program = program()
	self.Started = time.Now().UTC().Truncate(time.Second)

	body, err := json.Marshal(self)
	if err != nil {
		return fmt.Errorf("lock: encoding the holder record: %w", err)
	}
	body = append(body, '\n')

	if err := l.f.Truncate(0); err != nil {
		return fmt.Errorf("lock: truncating %s: %w", l.path, err)
	}
	if _, err := l.f.WriteAt(body, 0); err != nil {
		return fmt.Errorf("lock: writing %s: %w", l.path, err)
	}
	// Fsync so that a reader refused a moment from now sees the description
	// rather than an empty file. It costs one flush per session.
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("lock: syncing %s: %w", l.path, err)
	}
	return nil
}

// readHolder reads the description out of a lock file we could not take.
//
// Every failure yields the zero Holder rather than an error. The file is
// written by another process and can legitimately be caught empty or half
// written — the holder truncates and rewrites it just after taking the lock —
// and none of that changes the answer: the lock is held, because the kernel
// said so. The description is a courtesy, and [HeldError.Error] says plainly
// when it is missing rather than inventing one.
func readHolder(f *os.File) Holder {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Holder{}
	}
	body, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return Holder{}
	}
	var h Holder
	if err := json.Unmarshal(body, &h); err != nil {
		return Holder{}
	}
	return h
}

// hostname is the machine's name, or "" when it cannot be had. An empty string
// is rendered as an absent field rather than as a plausible default: a lock
// report naming the wrong machine is worse than one naming none.
func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

// program is the running binary's name, from os.Args[0].
//
// It is read here rather than supplied by the caller because it is a fact about
// the process, and the field's whole value is that it cannot be a claim. Under
// `go test` it is the test binary's name, which is the honest answer.
func program() string {
	if len(os.Args) == 0 || os.Args[0] == "" {
		return ""
	}
	return filepath.Base(os.Args[0])
}
