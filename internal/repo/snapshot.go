package repo

import (
	"context"
	"fmt"
	"math"
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

// NewSnapshotter prepares a session to be snapshotted from scratch.
//
// It validates the session id, ensures StateDir is excluded from git (see
// [Repo.ExcludeStateDir] — without which the throwaway index stages itself into
// the snapshot), creates the directory the index will live in, and refuses a
// session id that already has shadow refs.
//
// That last refusal is deliberate. Continuing a session whose refs exist would
// start a second chain from no parent while the ref names collide with the
// first, orphaning commits and leaving a snapshot history that describes a
// sequence of turns that never happened. That refusal is exactly right for two
// independent sessions that happen to collide on an id — a bug, or a stale
// leftover directory — and it is also what a genuine resume must *not* trip.
// [NewResumingSnapshotter] is the other case: a caller that knows it is
// continuing a specific, named session asks for that explicitly, and the
// refusal above stays in force for everyone who does not (KAN-939).
func NewSnapshotter(ctx context.Context, r *Repo, sessionID string, opts ...Option) (*Snapshotter, error) {
	return newSnapshotter(ctx, r, sessionID, false, opts...)
}

// NewResumingSnapshotter attaches to sessionID's existing shadow-ref chain
// instead of refusing it, so the next [Snapshotter.Snapshot] call chains onto
// the latest recorded turn rather than starting a second, colliding chain
// (KAN-939).
//
// It is [NewSnapshotter]'s deliberate exception, not a relaxation of it: the
// caller is asserting — because it is resuming a specific session by id, not
// because a ref happened to exist — that sessionID names a session being
// continued rather than two sessions that collided on an id by accident.
// Everything NewSnapshotter does before its refusal (validating the id,
// excluding StateDir, preparing the throwaway index) is identical here; only
// what happens when refs already exist differs.
//
// When no refs exist yet — a resumed session that never got as far as a
// mutating turn before it stopped — this behaves exactly like NewSnapshotter
// on a fresh id: the first [Snapshotter.Snapshot] call has no parent. There is
// nothing to attach to, so there is nothing to distinguish.
//
// When refs do exist, the ref naming the highest turn number is read back —
// not the journal's TurnSnapshot events, which would be a second account of
// the same fact and could in principle disagree with what git actually
// committed — and the snapshotter's internal chain state is seeded from it, so
// the next Snapshot call's Parent is that ref's commit and Snapshot's own
// "turn must increase" check is enforced against the real last turn rather
// than against a fresh chain that has forgotten it.
func NewResumingSnapshotter(ctx context.Context, r *Repo, sessionID string, opts ...Option) (*Snapshotter, error) {
	return newSnapshotter(ctx, r, sessionID, true, opts...)
}

// NewForkingSnapshotter prepares a **new**, independent session (newSessionID)
// whose first snapshot's parent is sourceSessionID's own snapshot at, or at
// the highest turn not exceeding, sourceTurn (KAN-940).
//
// It is a third case, not a third mode of the two above, because it answers a
// different question. [NewSnapshotter] starts a chain with no parent.
// [NewResumingSnapshotter] attaches to a chain that continues under **its own**
// session id: the same numbering, the same ref namespace, picking up where it
// left off. Forking shares neither property: newSessionID has never had a
// ref before (the refusal [NewSnapshotter] enforces applies here exactly as
// it does everywhere else — this is not the resume case, and nothing here
// widens that refusal), and its own turn numbers start at 1 the same way any
// fresh session's do, in its own namespace under RefPrefix+newSessionID — the
// forked session's turn 1 is not sourceTurn+1, it is simply 1, because the
// two sessions' own turn counters are unrelated (a fresh [Engine] always
// starts counting at 0 in-process, and only [engine.Config.ResumeHistory]'s
// replay ever moves that; this package does not know about turn counters at
// all, only about refs).
//
// So the seeded state is deliberately asymmetric with [NewResumingSnapshotter]'s:
// s.parent is set to the source's commit, so the *first* snapshot the new
// session ever writes has correct git ancestry — but s.snapped stays false
// and s.lastTurn stays 0, exactly [NewSnapshotter]'s own starting state,
// because the "turn must increase" check is a promise about *this session's
// own* chain and must not be pre-loaded with a number from a namespace that
// has nothing to do with it. Setting s.lastTurn = sourceTurn here would be
// the bug this comment exists to prevent: it would refuse the new session's
// own turn 1 with ErrTurnNotIncreasing, because 1 <= sourceTurn for almost
// every fork anybody would ever do.
//
// The returned [Snapshot] is sourceSessionID's own snapshot the parent was
// read from — Ref, Commit and Tree populated, Parent left "" because nothing
// here consumes it — so a caller ([engine.Fork]) can pass its Tree straight
// to [Repo.Restore] without a second, duplicate ref lookup. found is false
// exactly when sourceSessionID has no ref at or before sourceTurn: turn 0
// (fork before any turn ran) and a source session that never reached a
// mutating turn both land here honestly, and the caller gets a fresh,
// parentless chain with nothing to restore — the same starting state
// [NewSnapshotter] gives any brand-new session.
//
// sourceSessionID is validated the same way any session id is
// ([validateSessionID]); it may equal newSessionID only in the sense that
// nothing here refuses that (a caller forking a session from its own earlier
// turn under a literally identical id would be resuming, not forking, and
// [engine.Fork] is what refuses that case before this function is ever
// called).
func NewForkingSnapshotter(
	ctx context.Context, r *Repo, newSessionID, sourceSessionID string, sourceTurn int, opts ...Option,
) (*Snapshotter, Snapshot, bool, error) {
	if err := validateSessionID(sourceSessionID); err != nil {
		return nil, Snapshot{}, false, fmt.Errorf("repo: fork source session: %w", err)
	}
	if sourceTurn < 0 {
		return nil, Snapshot{}, false, fmt.Errorf(
			"repo: fork source turn %d: %w: turns are numbered from 0", sourceTurn, ErrTurnNotIncreasing)
	}

	// false: a brand-new session id must never silently attach to an existing
	// chain of its own — see NewSnapshotter's own refusal, which this does not
	// relax.
	s, err := newSnapshotter(ctx, r, newSessionID, false, opts...)
	if err != nil {
		return nil, Snapshot{}, false, err
	}

	foundTurn, commit, tree, found, err := refAtOrBefore(ctx, r, sourceSessionID, sourceTurn)
	if err != nil {
		return nil, Snapshot{}, false, fmt.Errorf(
			"repo: locating session %s's snapshot at or before turn %d: %w", sourceSessionID, sourceTurn, err)
	}
	if !found {
		return s, Snapshot{}, false, nil
	}

	s.parent = commit
	snap := Snapshot{
		Ref:    RefPrefix + sourceSessionID + "/" + strconv.Itoa(foundTurn),
		Commit: commit,
		Tree:   tree,
	}
	return s, snap, true, nil
}

// newSnapshotter is NewSnapshotter and NewResumingSnapshotter's shared body.
// attach selects which of the two the caller asked for; see their doc
// comments for what that means and why the distinction exists.
func newSnapshotter(ctx context.Context, r *Repo, sessionID string, attach bool, opts ...Option) (*Snapshotter, error) {
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

	if attach {
		turn, commit, found, err := s.latest(ctx)
		if err != nil {
			return nil, err
		}
		if found {
			s.parent = commit
			s.lastTurn = turn
			s.snapped = true
		}
		return s, nil
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

// latest reports the highest turn number sessionID has a shadow ref for, and
// that ref's commit, so [NewResumingSnapshotter] can seed the chain state a
// fresh [Snapshotter] would otherwise start from scratch.
//
// found is false when the session has no refs at all — a resume of a session
// that never reached a mutating turn — which is not an error: there is simply
// nothing to attach to yet.
//
// It is refAtOrBefore with no upper bound: NewResumingSnapshotter always
// wants the highest turn there is, never a specific ceiling, which is exactly
// what math.MaxInt as the bound expresses without a second code path.
func (s *Snapshotter) latest(ctx context.Context) (turn int, commit string, found bool, err error) {
	turn, commit, _, found, err = refAtOrBefore(ctx, s.repo, s.sessionID, math.MaxInt)
	return turn, commit, found, err
}

// refAtOrBefore reports sessionID's shadow ref whose turn number is the
// highest not exceeding turn, together with that ref's commit and tree —
// [NewResumingSnapshotter]'s "the highest turn there is" ([Snapshotter.latest],
// which passes math.MaxInt) and [NewForkingSnapshotter]'s "the tree as it
// stood once a specific turn had finished, even when that turn itself left no
// ref of its own because it did not touch the tree" are the same query with a
// different bound, so this is the one for-each-ref parse both read from
// rather than two copies that could drift.
//
// found is false when no ref for sessionID exists at or below turn at all —
// the session has never snapshotted anything yet, or turn is 0 — which is not
// an error: there is simply nothing there to report.
func refAtOrBefore(ctx context.Context, r *Repo, sessionID string, turn int) (
	foundTurn int, commit, tree string, found bool, err error,
) {
	prefix := RefPrefix + sessionID + "/"
	// %(tree) is valid on a commit object (and a tag) per git-for-each-ref(1);
	// every ref under this namespace points at a commit-tree commit, so this
	// one call is enough to answer both "which commit" and "which tree" without
	// a second git invocation per candidate.
	out, gerr := r.git(ctx, "for-each-ref", "--format=%(refname) %(objectname) %(tree)", prefix)
	if gerr != nil {
		return 0, "", "", false, fmt.Errorf("repo: listing existing snapshots of session %s: %w", sessionID, gerr)
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return 0, "", "", false, fmt.Errorf(
				"repo: unexpected for-each-ref output %q while listing session %s's snapshots",
				line, sessionID)
		}
		refname, obj, tr := fields[0], fields[1], fields[2]
		turnPart := strings.TrimPrefix(refname, prefix)
		t, perr := strconv.Atoi(turnPart)
		if perr != nil {
			return 0, "", "", false, fmt.Errorf(
				"repo: ref %q under session %s's namespace does not name a turn number: %w",
				refname, sessionID, perr)
		}
		if t > turn {
			continue
		}
		if !found || t > foundTurn {
			foundTurn, commit, tree, found = t, obj, tr, true
		}
	}
	return foundTurn, commit, tree, found, nil
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
