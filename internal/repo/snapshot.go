package repo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The identity every snapshot commit is written under.
//
// It is fixed rather than taken from the user's git configuration, for three
// reasons. A machine-made snapshot attributed to the human reads, in any tool
// that shows it, as something they wrote. A commit whose author varies by
// machine cannot be compared byte for byte across a replay, which SLICE-1's
// determinism criterion needs. And `git commit-tree` *fails* when no identity
// is configured — "Author identity unknown" — so a session on a freshly
// installed machine would die at the end of its first modifying turn, which is
// a miserable way to meet a new tool.
//
// The domain is .invalid: reserved by RFC 2606 and guaranteed never to resolve,
// so the address cannot silently be a real person's.
const (
	identityName  = "kopicode"
	identityEmail = "kopicode@kopicode.invalid"
)

// indexSubdir holds throwaway indexes, one per session, under StateDir. Git
// writes a sibling .lock file while it works, which is another reason the whole
// of StateDir — not just the index file — is what goes in info/exclude.
const indexSubdir = "index"

// maxSessionIDLen bounds a session id. It becomes a path component and a git
// ref component; neither has an interesting limit at this size, and the cap
// exists so a malformed id fails as a validation error rather than as a
// filesystem one.
const maxSessionIDLen = 128

// A Snapshot is one recorded state of the working tree.
//
// The fields mirror journal.TurnSnapshot exactly, and deliberately: the engine
// takes one of these and journals it. They are duplicated rather than shared
// because this package does not import the journal — the engine journals, this
// package snapshots, and that arrow points one way. Three landed cards made the
// same call.
type Snapshot struct {
	// Ref is the shadow ref, under RefPrefix.
	Ref string
	// Commit is the commit-tree object, Tree its tree, Parent the previous
	// snapshot's commit or "" for the first in a session.
	Commit string
	Tree   string
	Parent string
}

// options carries what NewSnapshotter injects.
type options struct {
	now func() time.Time
}

// An Option configures [NewSnapshotter].
type Option func(*options)

// WithClock injects the clock the commit timestamps come from.
//
// git otherwise reads the wall clock inside commit-tree, which puts the one
// input that changes on every run somewhere a test cannot reach. With a fixed
// clock the same tree, parent and message produce the same commit sha every
// time — verified — which is what SLICE-1's byte-identical-replay criterion
// rests on.
func WithClock(now func() time.Time) Option {
	return func(o *options) {
		if now != nil {
			o.now = now
		}
	}
}

// A Snapshotter writes one session's chain of turn snapshots.
//
// It is safe for concurrent use, though the chain it maintains is inherently
// ordered: the mutex is held across the whole four-command sequence so two
// turns cannot interleave and end up sharing a parent.
//
// It holds no context. Cancellation belongs to the call.
type Snapshotter struct {
	repo      *Repo
	sessionID string
	indexPath string
	now       func() time.Time

	mu       sync.Mutex
	parent   string
	lastTurn int
	snapped  bool
}

// NewSnapshotter prepares a session to be snapshotted.
//
// It validates the session id, ensures StateDir is excluded from git (see
// [Repo.ExcludeStateDir] — without which the throwaway index stages itself into
// the snapshot), creates the directory the index will live in, and refuses a
// session id that already has shadow refs.
//
// That last refusal is deliberate. Continuing a session whose refs exist would
// start a second chain from no parent while the ref names collide with the
// first, orphaning commits and leaving a snapshot history that describes a
// sequence of turns that never happened. Picking up an existing chain is
// resume, and resume is slice 2.
func NewSnapshotter(ctx context.Context, r *Repo, sessionID string, opts ...Option) (*Snapshotter, error) {
	if r == nil {
		return nil, fmt.Errorf("repo: NewSnapshotter needs a repository")
	}
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}

	o := options{now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}

	if err := r.ExcludeStateDir(); err != nil {
		return nil, err
	}

	indexDir := filepath.Join(r.StatePath(), indexSubdir)
	// 0700: the index names every path in the working tree, which on a private
	// repository is itself information. StateDir is created with the same
	// posture by the journal.
	if err := os.MkdirAll(indexDir, 0o700); err != nil {
		return nil, fmt.Errorf("repo: creating %s: %w", indexDir, err)
	}

	s := &Snapshotter{
		repo:      r,
		sessionID: sessionID,
		indexPath: filepath.Join(indexDir, sessionID),
		now:       o.now,
	}

	existing, err := r.git(ctx, "for-each-ref", "--count=1", "--format=%(refname)", s.refPrefix())
	if err != nil {
		return nil, fmt.Errorf("repo: checking for existing snapshots of session %s: %w", sessionID, err)
	}
	if strings.TrimSpace(existing) != "" {
		return nil, fmt.Errorf("repo: %s: %w: %s exists; resuming a chain is slice 2",
			sessionID, ErrSessionExists, strings.TrimSpace(existing))
	}

	return s, nil
}

// refPrefix is the namespace this session's refs live under, with the trailing
// slash for-each-ref needs to treat it as a directory.
func (s *Snapshotter) refPrefix() string { return RefPrefix + s.sessionID + "/" }

// IndexPath is the throwaway index this session stages into. Exposed so a
// caller diagnosing a session can see it, and so a test can assert it is never
// the real one.
func (s *Snapshotter) IndexPath() string { return s.indexPath }

// Snapshot records the working tree as it stands and publishes it at
// refs/kopicode/<session>/<turn>.
//
// The mechanism is docs/SLICE-1.md §2, in four commands:
//
//  1. GIT_INDEX_FILE points at the throwaway index, verified before git runs.
//  2. `git add -A` stages everything, *including untracked files* — the model
//     creates new files, and a snapshot that omitted them would describe a tree
//     that never existed.
//  3. `git write-tree`, then `git commit-tree` with the previous snapshot as
//     parent. The first snapshot in a session has no parent: the chain is
//     kopicode's, and rooting it on the user's HEAD would tie it to a commit
//     the user may go on to rewrite.
//  4. `git update-ref`, with an empty expected old value so an unexpected ref
//     collision fails the call rather than silently replacing a snapshot.
//
// turn must increase between calls. Files ignored by .gitignore or info/exclude
// are not captured, which is the same tree git itself would describe; whether
// the turn touched anything is the caller's judgement, and a snapshot of an
// unchanged tree is recorded rather than skipped.
//
// Nothing here reads or writes HEAD, the user's index, their branch or their
// stashes. The only thing added to the repository is objects and a ref under
// refs/kopicode/.
func (s *Snapshotter) Snapshot(ctx context.Context, turn int) (Snapshot, error) {
	if turn < 0 {
		return Snapshot{}, fmt.Errorf("repo: turn %d: %w: turns are numbered from 0", turn, ErrTurnNotIncreasing)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.snapped && turn <= s.lastTurn {
		return Snapshot{}, fmt.Errorf("repo: turn %d: %w: turn %d is already snapshotted",
			turn, ErrTurnNotIncreasing, s.lastTurn)
	}

	env, err := s.env()
	if err != nil {
		return Snapshot{}, err
	}

	// -A rather than -u: -u stages modifications and deletions of files git
	// already knows about, and would silently drop every file the model
	// created. That is the "the snapshot is a lie" case.
	if _, err := runGit(ctx, s.repo.root, env, "add", "-A"); err != nil {
		return Snapshot{}, fmt.Errorf("repo: staging the working tree into %s: %w", s.indexPath, err)
	}

	tree, err := s.object(ctx, env, "write-tree")
	if err != nil {
		return Snapshot{}, fmt.Errorf("repo: writing the tree for turn %d: %w", turn, err)
	}

	args := []string{"commit-tree", tree}
	if s.parent != "" {
		args = append(args, "-p", s.parent)
	}
	args = append(args, "-m", s.message(turn))

	commit, err := s.object(ctx, env, args...)
	if err != nil {
		return Snapshot{}, fmt.Errorf("repo: committing the tree for turn %d: %w", turn, err)
	}

	ref := s.refPrefix() + strconv.Itoa(turn)
	// The trailing "" is the expected old value: git requires the ref not to
	// exist. NewSnapshotter already refuses a session with refs, so reaching
	// this is a bug — and a bug that overwrote a snapshot would be invisible.
	if _, err := runGit(ctx, s.repo.root, env, "update-ref", ref, commit, ""); err != nil {
		return Snapshot{}, fmt.Errorf("repo: publishing turn %d at %s: %w", turn, ref, err)
	}

	snap := Snapshot{Ref: ref, Commit: commit, Tree: tree, Parent: s.parent}
	// Advanced only now: a failure part-way leaves the chain where it was,
	// so a retry of the same turn is still a legal call.
	s.parent = commit
	s.lastTurn = turn
	s.snapped = true
	return snap, nil
}

// object runs a git command whose whole output is one object id, and checks
// that it looks like one. A truncated or empty read here would otherwise be
// recorded as a snapshot, and the lie would only surface when someone tried to
// use it.
func (s *Snapshotter) object(ctx context.Context, env []string, args ...string) (string, error) {
	out, err := runGit(ctx, s.repo.root, env, args...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if !isObjectID(id) {
		return "", fmt.Errorf("repo: git %s returned %q, which is not an object id",
			strings.Join(args, " "), id)
	}
	return id, nil
}

// isObjectID accepts SHA-1 and SHA-256 object ids, since a repository may be
// either.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// message is the commit message. It carries no timestamp and no host, so the
// commit is a pure function of the tree, the parent and the injected clock.
func (s *Snapshotter) message(turn int) string {
	return fmt.Sprintf("kopicode snapshot: session %s, turn %d", s.sessionID, turn)
}

// env builds the environment for one snapshot: the isolated index, the fixed
// identity, and the injected clock as both dates.
//
// It verifies the result rather than trusting that it built it correctly. The
// check looks like it cannot fail — the value was just appended — and that is
// the point: it is what stands between a refactor that drops the override and a
// `git add -A` into the user's real index, and the failure mode it guards has
// no other symptom until someone loses their staged work.
func (s *Snapshotter) env() ([]string, error) {
	// Git's raw date format: seconds since the epoch and a UTC offset.
	stamp := strconv.FormatInt(s.now().UTC().Unix(), 10) + " +0000"

	env := append(baseEnv(),
		"GIT_INDEX_FILE="+s.indexPath,
		"GIT_AUTHOR_NAME="+identityName,
		"GIT_AUTHOR_EMAIL="+identityEmail,
		"GIT_AUTHOR_DATE="+stamp,
		"GIT_COMMITTER_NAME="+identityName,
		"GIT_COMMITTER_EMAIL="+identityEmail,
		"GIT_COMMITTER_DATE="+stamp,
	)

	if err := verifyIndexIsolated(env, s.indexPath); err != nil {
		return nil, err
	}
	return env, nil
}

// verifyIndexIsolated confirms that the environment about to be handed to git
// really does redirect the index, and redirects it where this session intends.
//
// An unset or empty GIT_INDEX_FILE does not mean "no index"; it means the
// repository's real one. So absence, emptiness and a value that is not ours are
// all the same failure, and all of them fail the operation.
func verifyIndexIsolated(env []string, want string) error {
	got, ok := envValue(env, "GIT_INDEX_FILE")
	switch {
	case !ok:
		return fmt.Errorf("repo: %w: GIT_INDEX_FILE is unset, so git would write the repository's own index",
			ErrIndexNotIsolated)
	case got == "":
		return fmt.Errorf("repo: %w: GIT_INDEX_FILE is empty, which git reads as the repository's own index",
			ErrIndexNotIsolated)
	case got != want:
		return fmt.Errorf("repo: %w: GIT_INDEX_FILE is %q, want the session's throwaway index %q",
			ErrIndexNotIsolated, got, want)
	case !filepath.IsAbs(got):
		// Git resolves a relative GIT_INDEX_FILE against its own working
		// directory, which is not necessarily the one this package chose.
		return fmt.Errorf("repo: %w: GIT_INDEX_FILE %q is relative", ErrIndexNotIsolated, got)
	}
	return nil
}

// Close removes the throwaway index.
//
// It is optional — the index is inside StateDir, excluded from git, and
// harmless if left — but a session that ends tidily should not leave a file
// naming every path in the tree behind.
func (s *Snapshotter) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.indexPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("repo: removing the throwaway index %s: %w", s.indexPath, err)
	}
	return nil
}

// validateSessionID rejects an id that cannot safely be both a path component
// and a git ref component.
//
// The character set is the intersection of what is safe in both, minus the
// shapes git refuses in a ref (a leading dot, "..", a ".lock" suffix). It is
// deliberately narrower than either would strictly allow: session ids are
// generated by kopicode, so nothing legitimate needs a slash, and an id that
// could contain one is a directory traversal waiting for the first caller who
// passes user input through.
func validateSessionID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("repo: %w: empty", ErrInvalidSessionID)
	case len(id) > maxSessionIDLen:
		return fmt.Errorf("repo: %w: %d characters, limit %d", ErrInvalidSessionID, len(id), maxSessionIDLen)
	case strings.HasPrefix(id, "."), strings.HasPrefix(id, "-"):
		return fmt.Errorf("repo: %w: %q may not begin with %q", ErrInvalidSessionID, id, id[:1])
	case strings.Contains(id, ".."):
		return fmt.Errorf("repo: %w: %q contains %q", ErrInvalidSessionID, id, "..")
	case strings.HasSuffix(id, ".lock"):
		return fmt.Errorf("repo: %w: %q ends with .lock, which git refuses as a ref", ErrInvalidSessionID, id)
	}
	for _, c := range id {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
		if !ok {
			return fmt.Errorf("repo: %w: %q contains %q; allowed: letters, digits, '-', '_', '.'",
				ErrInvalidSessionID, id, string(c))
		}
	}
	return nil
}
