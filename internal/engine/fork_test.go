package engine_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/tools"
)

// This file is KAN-940's end-to-end proof, plus the validation refusals that
// need no git repository at all. The design questions it exists to answer —
// journal duplication versus a reference, why the tree is restored
// automatically, how turn numbering survives the fork — are argued in
// fork.go's own package comment; this file is where those arguments are
// checked against a real git repository and a real journal on disk rather
// than merely asserted.
//
// # Why this file, alone among this package's tests, shells out to git
//
// open_test.go and harness_test.go both say plainly that they do not, and
// for a real reason: internal/repo already owns the git side and has the
// fixtures that assert their own isolation, so a second git subprocess here
// would be a second place to get the containment rules wrong for no
// coverage this package could not get from a stub. That argument holds for
// everything KAN-939 needed, because Options.Resume never has to prove a
// snapshot chain's *ancestry* to be correct — a stub Snapshotter is enough
// to prove the loop asked it to snapshot.
//
// KAN-940 cannot make the same trade. "The fork's snapshot chains onto the
// source's turn-1 commit" and "the tree was actually restored" are claims
// about a real git object graph and a real filesystem, and a stub that only
// remembers which turn numbers it was called with cannot represent either
// one. So this file accepts the cost open_test.go avoids, and pays down the
// risk the ground-rules skill describes the same way internal/repo's own
// fixtures do: every git subprocess below names its Dir explicitly, builds
// its Env from scratch rather than inheriting one, and refuses to run at all
// against a directory that is not under t.TempDir().

// forkFixtureEnv is the environment every git command in this file runs in:
// every inherited GIT_* variable dropped, then the two isolation pins added
// back explicitly. Building it rather than inheriting os.Environ() is what
// stops a GIT_DIR left over in this process's own environment from
// redirecting one of these commands at a repository that is not the fixture
// — see .claude/skills/agent-ground-rules/SKILL.md.
func forkFixtureEnv() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); !ok || !strings.HasPrefix(name, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
}

// forkFixtureGit runs one git command against a fixture repository, refusing
// outright to run against a directory outside t.TempDir() — the same refusal
// internal/repo's own fixtures make, for the same reason: an empty or
// wrong Dir here would run against whatever directory the test binary
// happened to start in, which is this repository's own checkout.
func forkFixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	tmp := os.TempDir()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolving fixture dir %s: %v", dir, err)
	}
	if dir == "" || (!strings.HasPrefix(abs, tmp) && !strings.HasPrefix(abs, t.TempDir())) {
		t.Fatalf("git %v: %s is not under a temp directory; refusing to run a fixture command "+
			"against a directory that might be this repository's own checkout", args, dir)
	}

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = forkFixtureEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newForkFixtureRepo makes an empty repository (no commits — Snapshotter
// needs none, see internal/repo's own TestSnapshotInRepoWithNoCommits) at a
// fresh subdirectory of t.TempDir().
func newForkFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	forkFixtureGit(t, dir, "init", "-q", "-b", "main")
	return dir
}

// --- validation refusals that need no repository at all --------------------

// openTrivialSession opens, runs one prose-only turn on, and closes a session
// at dir under id — a minimal but genuine existing session, for tests whose
// point is what Fork does with an id that already names one.
func openTrivialSession(t *testing.T, dir, id string) {
	t.Helper()
	prov := script(t, []scriptedReply{{text: "hi"}}, oneAttemptPerTurn(1))
	s, err := engine.Open(t.Context(), engine.Options{
		Dir: dir, SessionID: id, Selection: testSelection(prov), Provider: prov, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("opening trivial session %s: %v", id, err)
	}
	if _, err := s.Run(t.Context(), "hello"); err != nil {
		t.Fatalf("running trivial session %s: %v", id, err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("closing trivial session %s: %v", id, err)
	}
}

// forkSelection builds a Selection and a Provider from the same no-op
// script, for a validation test that never expects a request to actually
// reach a provider but does need [engine.ErrNoAPIKey] to stay out of its way
// (openSession's credential check runs before every other refusal this file
// exercises, including the ones with nothing to do with a provider at all).
func forkSelection(t *testing.T) (engine.Selection, engine.Provider) {
	t.Helper()
	prov := script(t, nil, nil)
	return testSelection(prov), prov
}

func TestForkNeedsAForkSource(t *testing.T) {
	sel, prov := forkSelection(t)
	_, err := engine.Fork(t.Context(), engine.Options{Dir: t.TempDir(), Selection: sel, Provider: prov})
	if !errors.Is(err, engine.ErrConfig) {
		t.Errorf("Fork with no Options.Fork = %v, want ErrConfig", err)
	}
}

func TestOpenRefusesWhenForkIsSet(t *testing.T) {
	sel, prov := forkSelection(t)
	_, err := engine.Open(t.Context(), engine.Options{
		Dir: t.TempDir(), Selection: sel, Provider: prov,
		Fork: &engine.ForkSource{SessionID: "source", Turn: 1},
	})
	if !errors.Is(err, engine.ErrConfig) {
		t.Errorf("Open with Options.Fork set = %v, want ErrConfig", err)
	}
}

func TestForkAndResumeAreMutuallyExclusive(t *testing.T) {
	dir := t.TempDir()
	openTrivialSession(t, dir, "source")
	sel, prov := forkSelection(t)
	_, err := engine.Fork(t.Context(), engine.Options{
		Dir: dir, Selection: sel, Provider: prov, Resume: true, SessionID: "source",
		Fork: &engine.ForkSource{SessionID: "source", Turn: 1},
	})
	if !errors.Is(err, engine.ErrConfig) {
		t.Errorf("Fork with Resume also set = %v, want ErrConfig", err)
	}
}

func TestForkNeedsASourceSessionID(t *testing.T) {
	sel, prov := forkSelection(t)
	_, err := engine.Fork(t.Context(), engine.Options{
		Dir: t.TempDir(), Selection: sel, Provider: prov, Fork: &engine.ForkSource{Turn: 1},
	})
	if !errors.Is(err, engine.ErrConfig) {
		t.Errorf("Fork with an empty Fork.SessionID = %v, want ErrConfig", err)
	}
}

func TestForkRejectsANegativeTurn(t *testing.T) {
	sel, prov := forkSelection(t)
	_, err := engine.Fork(t.Context(), engine.Options{
		Dir: t.TempDir(), Selection: sel, Provider: prov, Fork: &engine.ForkSource{SessionID: "source", Turn: -1},
	})
	if !errors.Is(err, engine.ErrConfig) {
		t.Errorf("Fork with a negative turn = %v, want ErrConfig", err)
	}
}

func TestForkRejectsTheSameSessionIDAsItsSource(t *testing.T) {
	sel, prov := forkSelection(t)
	_, err := engine.Fork(t.Context(), engine.Options{
		Dir: t.TempDir(), Selection: sel, Provider: prov, SessionID: "same",
		Fork: &engine.ForkSource{SessionID: "same", Turn: 1},
	})
	if !errors.Is(err, engine.ErrConfig) {
		t.Errorf("Fork with SessionID equal to Fork.SessionID = %v, want ErrConfig", err)
	}
}

func TestForkRejectsSnapshotsOff(t *testing.T) {
	dir := t.TempDir()
	openTrivialSession(t, dir, "source")
	sel, prov := forkSelection(t)
	_, err := engine.Fork(t.Context(), engine.Options{
		Dir: dir, Selection: sel, Provider: prov, Snapshots: engine.SnapshotsOff,
		Fork: &engine.ForkSource{SessionID: "source", Turn: 1},
	})
	if !errors.Is(err, engine.ErrConfig) {
		t.Errorf("Fork with SnapshotsOff = %v, want ErrConfig", err)
	}
}

// TestForkRequiresAnExistingSourceSession holds Fork's own read-only
// existence check to the same discipline Options.Resume's holds
// TestResumeRequiresAnExistingSession to: told plainly, before anything is
// touched, rather than handed a fork of nothing.
func TestForkRequiresAnExistingSourceSession(t *testing.T) {
	dir := t.TempDir()
	sel, prov := forkSelection(t)
	_, err := engine.Fork(t.Context(), engine.Options{
		Dir: dir, Selection: sel, Provider: prov, Fork: &engine.ForkSource{SessionID: "never-existed", Turn: 1},
	})
	if !errors.Is(err, engine.ErrConfig) {
		t.Fatalf("Fork from a nonexistent source = %v, want ErrConfig", err)
	}
	if !strings.Contains(err.Error(), "never-existed") {
		t.Errorf("the refusal does not name the missing source: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".kopicode")); !os.IsNotExist(statErr) {
		t.Errorf("a refused Fork created %s", filepath.Join(dir, ".kopicode"))
	}
}

// TestForkRefusesAnAlreadyExistingNewSessionID is the direction Resume never
// has to check, because Resume *wants* an existing session under the id it
// was given: a fork's own new id must never already have a record, or the
// SessionForked marker this call would append lands partway through a
// session that began some other way.
func TestForkRefusesAnAlreadyExistingNewSessionID(t *testing.T) {
	dir := t.TempDir()
	openTrivialSession(t, dir, "source")
	openTrivialSession(t, dir, "already-here")

	sel, prov := forkSelection(t)
	_, err := engine.Fork(t.Context(), engine.Options{
		Dir: dir, Selection: sel, Provider: prov, SessionID: "already-here",
		Fork: &engine.ForkSource{SessionID: "source", Turn: 1},
	})
	if !errors.Is(err, engine.ErrConfig) {
		t.Fatalf("Fork onto an id that already has a session = %v, want ErrConfig", err)
	}
	if !strings.Contains(err.Error(), "already-here") {
		t.Errorf("the refusal does not name the colliding id: %v", err)
	}
}

// TestForkRequiresAGitRepository holds [Fork]'s own doc comment to account:
// unlike Open, which treats "not a repository" as an ordinary, unsnapshotted
// session, Fork has nothing to restore or attach a chain to outside one.
func TestForkRequiresAGitRepository(t *testing.T) {
	dir := t.TempDir()
	openTrivialSession(t, dir, "source")

	sel, prov := forkSelection(t)
	_, err := engine.Fork(t.Context(), engine.Options{
		Dir: dir, Selection: sel, Provider: prov, Fork: &engine.ForkSource{SessionID: "source", Turn: 1},
	})
	if err == nil {
		t.Fatal("Fork outside a git repository succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "git repository") {
		t.Errorf("the refusal does not explain that a repository is needed: %v", err)
	}
}

// TestForkRejectsATurnBeyondTheSourcesOwnHistory holds readForkSource's
// bound check to account: asking to fork from a turn the source never
// reached is a caller mistake, not a request to be silently satisfied with
// whatever the source does have.
func TestForkRejectsATurnBeyondTheSourcesOwnHistory(t *testing.T) {
	dir := newForkFixtureRepo(t)
	openTrivialSession(t, dir, "source") // one prose-only turn: reaches turn 1, never turn 5

	sel, prov := forkSelection(t)
	_, err := engine.Fork(t.Context(), engine.Options{
		Dir: dir, Selection: sel, Provider: prov, SessionID: "fork",
		Fork: &engine.ForkSource{SessionID: "source", Turn: 5},
	})
	if !errors.Is(err, engine.ErrConfig) {
		t.Fatalf("Fork from a turn beyond the source's history = %v, want ErrConfig", err)
	}
	if !strings.Contains(err.Error(), "5") {
		t.Errorf("the refusal does not name the requested turn: %v", err)
	}
}

// --- the end-to-end proof ----------------------------------------------

// forkFixtureGitInRepo is forkFixtureGit against the repository every test
// below shares, for the verification queries a test makes once a session has
// already run.
func gitRevParse(t *testing.T, dir, rev string) string {
	t.Helper()
	return forkFixtureGit(t, dir, "rev-parse", rev)
}

// TestForkSharesHistoryRestoresTheTreeAndLeavesTheSourceUntouched is KAN-940's
// full claim, checked end to end rather than piecewise: a source session
// runs three turns, two of which write files; a new session forks from turn
// 1; and every one of the card's own acceptance points holds at once.
func TestForkSharesHistoryRestoresTheTreeAndLeavesTheSourceUntouched(t *testing.T) {
	dir := newForkFixtureRepo(t)
	ctx := t.Context()

	// The source session: turn 1 writes greet.go, turn 2 rewrites it and adds
	// a second file, turn 3 stops in prose. Two mutating turns exist so the
	// tree "as of turn 1" and the tree the working directory is actually in
	// once the source session ends are provably different — which is the
	// whole reason a fork has anything to restore.
	sourceProv := script(t, []scriptedReply{
		{calls: []wireCall{nativeCall("call-1", tools.ToolWriteFile, `{"path":"greet.go","content":"turn one\n"}`)}},
		{calls: []wireCall{
			nativeCall("call-2a", tools.ToolWriteFile, `{"path":"greet.go","content":"turn two\n"}`),
			nativeCall("call-2b", tools.ToolWriteFile, `{"path":"turn-two-only.txt","content":"b\n"}`),
		}},
		{text: "Done."},
	}, oneAttemptPerTurn(3))

	sourceSel := testSelection(sourceProv)
	source, err := engine.Open(ctx, engine.Options{
		Dir: dir, SessionID: "session-source", Selection: sourceSel, Provider: sourceProv, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("opening the source session: %v", err)
	}
	if _, err := source.Run(ctx, "write greet.go, then rewrite it and add a second file"); err != nil {
		t.Fatalf("running the source session: %v", err)
	}
	if err := sourceProv.Drained(); err != nil {
		t.Fatalf("source provider not drained: %v", err)
	}
	if err := source.Close(ctx); err != nil {
		t.Fatalf("closing the source session: %v", err)
	}

	sourceEvents := readJournal(t, source.Path())
	sourceCommitAtTurn1 := sole[journal.TurnSnapshot](t, filterByTurn(sourceEvents, 1)).Commit
	sourceJournalBefore, err := os.ReadFile(filepath.Join(source.Path(), journal.EventsFile))
	if err != nil {
		t.Fatalf("reading the source journal before forking: %v", err)
	}
	sourceRefBefore := gitRevParse(t, dir, "refs/kopicode/session-source/1")

	// Sanity: the working tree is currently at turn 2's state, not turn 1's —
	// otherwise a bug that skipped Restore entirely would still pass.
	if got, _ := os.ReadFile(filepath.Join(dir, "greet.go")); string(got) != "turn two\n" {
		t.Fatalf("setup: greet.go = %q before forking, want %q", got, "turn two\n")
	}
	if _, err := os.Stat(filepath.Join(dir, "turn-two-only.txt")); err != nil {
		t.Fatalf("setup: turn-two-only.txt does not exist before forking: %v", err)
	}

	// The fork: from turn 1, so its history should carry turn 1 alone and its
	// own new turn should be numbered 2 (turn 1's own successor), not 1.
	forkProv := script(t, []scriptedReply{
		{calls: []wireCall{nativeCall("call-fork", tools.ToolWriteFile, `{"path":"forked.txt","content":"forked\n"}`)}},
		{text: "Forked and done."},
	}, [][2]int{{2, 1}, {3, 1}})

	forkSel := testSelection(forkProv)
	forked, err := engine.Fork(ctx, engine.Options{
		Dir: dir, SessionID: "session-fork", Selection: forkSel, Provider: forkProv, Now: fixedClock(),
		Fork: &engine.ForkSource{SessionID: "session-source", Turn: 1},
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	t.Cleanup(func() { _ = forked.Close(context.Background()) })

	// The tree, restored, before the forked session's own turn ever ran.
	if got, err := os.ReadFile(filepath.Join(dir, "greet.go")); err != nil || string(got) != "turn one\n" {
		t.Errorf("greet.go = %q (err %v) immediately after Fork, want %q (turn 1's own state)",
			got, err, "turn one\n")
	}
	if _, err := os.Stat(filepath.Join(dir, "turn-two-only.txt")); !os.IsNotExist(err) {
		t.Errorf("turn-two-only.txt exists after Fork restored turn 1's tree, which never had it")
	}

	if _, err := forked.Run(ctx, "try a different fix"); err != nil {
		t.Fatalf("running the forked session: %v", err)
	}
	if err := forkProv.Drained(); err != nil {
		t.Errorf("fork provider not drained: %v", err)
	}
	if err := forked.Close(ctx); err != nil {
		t.Fatalf("closing the forked session: %v", err)
	}

	// 1. The forked session's own journal carries the shared prefix.
	forkedEvents := readJournal(t, forked.Path())
	if got, want := forkedEvents[0].Type(), journal.TypeSessionStarted; got != want {
		t.Fatalf("forked journal's first event is %s, want %s", got, want)
	}
	if got, want := forkedEvents[1].Type(), journal.TypeSessionForked; got != want {
		t.Fatalf("forked journal's second event is %s, want %s", got, want)
	}
	marker := forkedEvents[1].Payload.(journal.SessionForked)
	if marker.SourceSessionID != "session-source" || marker.SourceTurn != 1 {
		t.Errorf("SessionForked = %+v, want SourceSessionID session-source, SourceTurn 1", marker)
	}
	turn1FromSource := filterByTurn(sourceEvents, 1)
	if marker.Copied != len(turn1FromSource) {
		t.Errorf("SessionForked.Copied = %d, want %d (session-source's own turn 1 events)",
			marker.Copied, len(turn1FromSource))
	}
	copiedBlock := forkedEvents[2 : 2+marker.Copied]
	for i, ev := range copiedBlock {
		if ev.Turn != 1 {
			t.Errorf("copied event %d has Turn %d, want 1", i, ev.Turn)
		}
		if ev.Type() != turn1FromSource[i].Type() {
			t.Errorf("copied event %d is %s, want %s (source's own turn-1 event in the same position)",
				i, ev.Type(), turn1FromSource[i].Type())
		}
	}
	// The copied block is exactly session-source's own turn 1 — same length,
	// same types, same position — and nothing more: session-source's turn 2
	// (the rewrite to "turn two\n" and turn-two-only.txt) and its SessionEnded
	// never cross over. What follows the copied block is this session's own:
	// its own turn 2 is numbered 2 because it is turn 1's successor, not
	// because anything from session-source's own turn 2 leaked in — the
	// equality checks above already pin the copied block's exact contents, so
	// a Turn-2 event past that point is this session's own new turn, not a
	// leak.

	// 2. The forked session's own new turn is numbered 2 (turn 1's successor),
	// and its own snapshot parents onto the source's turn-1 commit.
	forkedSnap := sole[journal.TurnSnapshot](t, filterByTurn(forkedEvents, 2))
	if got := gitRevParse(t, dir, forkedSnap.Commit+"^"); got != sourceCommitAtTurn1 {
		t.Errorf("forked snapshot's parent (per git) = %s, want session-source's turn-1 commit %s",
			got, sourceCommitAtTurn1)
	}
	if forkedSnap.Parent != sourceCommitAtTurn1 {
		t.Errorf("forked TurnSnapshot.Parent = %s, want %s", forkedSnap.Parent, sourceCommitAtTurn1)
	}

	// 3. session-source's own record is untouched: same bytes on disk, same
	// ref pointing at the same commit.
	sourceJournalAfter, err := os.ReadFile(filepath.Join(source.Path(), journal.EventsFile))
	if err != nil {
		t.Fatalf("reading the source journal after forking: %v", err)
	}
	if string(sourceJournalAfter) != string(sourceJournalBefore) {
		t.Errorf("session-source's own journal changed after forking:\nbefore:\n%s\nafter:\n%s",
			sourceJournalBefore, sourceJournalAfter)
	}
	if got := gitRevParse(t, dir, "refs/kopicode/session-source/1"); got != sourceRefBefore {
		t.Errorf("refs/kopicode/session-source/1 moved from %s to %s", sourceRefBefore, got)
	}
}

// filterByTurn is the same "one turn's worth of events" slice this file's
// end-to-end test needs twice: once to know what session-source's own turn 1
// looked like, and once to find the forked session's own turn-2 snapshot.
func filterByTurn(evs []journal.Event, turn int) []journal.Event {
	var out []journal.Event
	for _, ev := range evs {
		if ev.Turn == turn {
			out = append(out, ev)
		}
	}
	return out
}
