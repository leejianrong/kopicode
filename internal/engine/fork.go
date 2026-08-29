package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/repo"
)

// This file is KAN-940's engine-level half: branching a brand-new session
// from a copy of an existing one's history up to a chosen turn, so a caller
// can try a different continuation without touching the session it came
// from. [Fork] in open.go is the entry point; this file is what it calls once
// the new session's journal and engine both exist.
//
// # Why Fork is not Open with another bit set
//
// [Options.Resume] is one caller-supplied bit because resuming genuinely is
// one fact with three consequences on an otherwise identical session: same
// id, same journal file, same ref namespace. Forking shares none of those —
// a new session id, a brand-new journal file, a snapshot chain seeded from a
// *different* session's ref rather than its own — so bolting it onto Open as
// a second bool would mean Open's every internal step re-deciding, field by
// field, which of two unrelated operations it is mid-way through. [Fork] is a
// second entry point instead, sharing Open's plumbing where the two
// legitimately agree (the credential check, the working-tree lock, the tool
// set and consent gate, the provider and the final New/Start) and diverging
// where they do not (which journal supplies history, whether the working
// tree moves, how the snapshot chain is seeded).
//
// # Journal duplication versus a reference
//
// See [journal.SessionForked]'s own doc comment for the argument in full;
// the short version is that CLAUDE.md's "one session record, and it is the
// journal" reads as a promise about *this* session's own file, and a fork
// that only recorded "ask session X about turn Y" would make good on that
// promise only conditionally, on X still existing and being reachable from
// wherever the reader is. [recordFork] duplicates instead.
//
// # Why the tree is restored automatically
//
// [Options.Resume]'s own doc comment argues, at length, why *resuming* must
// not restore the tree on the caller's behalf: the ordinary case is an
// interrupted process whose tree is already where it was left, and
// force-restoring in the case that is not ordinary — the user touched the
// tree by hand in between — would silently discard that work with no
// consent asked.
//
// None of that argument survives contact with forking. Forking's *entire
// point* is to try a different continuation from turn N, which is a claim
// about the tree as much as about the conversation: the copied history's
// tool results, anchors and diffs all describe the tree as it stood at turn
// N, and if the actual working tree has since moved past turn N — because
// the source session kept running, or because the fork's own destination
// directory already holds some other state — every one of those anchors is
// now describing a file that may not read the way the copied ToolResult
// events say it does. That is not a UX rough edge, it is ADR-0006's hard
// failure mode reappearing one layer up: an edit tool that is shown one tree
// and told it is looking at another, except here nothing rejects the
// mismatch before the model acts on it, because the mismatch is baked into
// the very history the model is about to be handed. So [Fork] calls
// [repo.Repo.Restore] itself, using the source session's own recorded tree at
// the fork point, before anything else touches the destination directory —
// there is no "ordinary case" here where skipping it is safe, which is
// exactly the asymmetry with Resume that makes automatic restore the right
// default for one and the wrong one for the other.
//
// The corollary CLAUDE.md's own "never touch the user's git state" promise
// still holds even though this file rewrites files: Restore never resets
// HEAD, never touches the index, never moves a branch or a stash — it
// extracts a tree's blobs onto disk the same way `git archive | tar -x`
// would, and [repo.Repo.Restore]'s own doc comment is the source of truth
// for exactly what that does and does not disturb.
//
// # Turn numbering after a fork
//
// The forked session's own turn counter does **not** restart at 1 while the
// copied history's events still carry turns 1..sourceTurn — that would put
// two unrelated things on the record under the same turn number the moment
// the new session ran its own first turn. [New] seeds [Engine.turn] from the
// highest Turn value in [Config.ResumeHistory] (which this file populates
// with the copied events for a fork, exactly as [Options.Resume] populates it
// with a resumed session's own prior events), so the forked session's first
// genuinely new turn is numbered sourceTurn+1 and turn numbers strictly
// increase through the whole file, copied prefix and new suffix alike.
//
// This is also what makes forking safe at the *repository* level without
// [repo.NewForkingSnapshotter] needing to know anything about turn numbers at
// all: the forked session's ref namespace (refs/kopicode/<new-id>/...) never
// collides with the source's regardless of which numbers either side uses,
// and the source's own record is never written to — see [recordFork] and
// [repo.NewForkingSnapshotter]'s own doc comment for the git-level half of
// that argument.

// ForkSource names the session and turn [Fork] branches a new session from.
//
// SessionID is the existing session being read from — never written to; see
// [recordFork]. Turn is the last turn (inclusive) of SessionID's own record
// the new session's history is seeded with; 0 forks before SessionID ran any
// turn at all, which is a legal, if degenerate, request (an empty history and
// nothing to restore).
type ForkSource struct {
	SessionID string
	Turn      int
}

// readForkSource reads src.SessionID's own record — under root, the same
// directory the forked session is about to open in; see [Options.Fork]'s doc
// comment for why a fork does not name a separate directory for its source —
// and returns the events belonging to turns 1 through src.Turn inclusive,
// together with any [journal.ProjectInstructionsLoaded] the source's own
// bootstrap wrote (see below), in seq order, together with the highest turn
// number src.SessionID's own record actually reached.
//
// The range excludes Turn 0 otherwise, deliberately: that is where
// SessionID's own SessionStarted (and, if it has since closed, its
// SessionEnded) live, and duplicating either into the new session's record
// would misrepresent this session's own start — see [journal.SessionForked]'s
// doc comment.
//
// [journal.ProjectInstructionsLoaded] is the one exception, carved out of
// that exclusion rather than folded into the Turn range: it also lives at
// Turn 0 (openSession's bootstrap writes it immediately after
// SessionStarted, before any turn runs — see instructions.go), but it is
// source-tree content the model was actually shown, not a fact about how the
// source session itself began. [Fork] never re-runs the discovery that
// produced it — loadProjectInstructions is called only for a genuinely new
// session, never a forked one — so if this exception did not exist, a fork's
// history would silently omit the AGENTS.md turn the source session's own
// history carries, the same gap the resume.go replay switch exists to close
// for [Options.Resume]. Found by reading this function's own Turn-0
// exclusion against KAN-1024's design, not assumed.
//
// It uses [journal.ReadSessionBlobs] rather than [journal.ReadSession]
// because [recordFork] is about to duplicate these events' *content* — a
// spilled tool result's actual bytes, not a reference to a blob this new
// session's own journal has never written — into a brand-new journal, and
// only the blob-aware reader rehydrates that.
func readForkSource(ctx context.Context, root string, src ForkSource) (events []journal.Event, sourceMaxTurn int, err error) {
	for ev, rerr := range journal.ReadSessionBlobs(ctx, root, src.SessionID) {
		if rerr != nil {
			return nil, 0, fmt.Errorf(
				"engine: reading session %s's history to fork from: %w", src.SessionID, rerr)
		}
		if ev.Turn > sourceMaxTurn {
			sourceMaxTurn = ev.Turn
		}
		switch {
		case ev.Turn >= 1 && ev.Turn <= src.Turn:
			events = append(events, ev)
		case ev.Turn == 0:
			if _, ok := ev.Payload.(journal.ProjectInstructionsLoaded); ok {
				events = append(events, ev)
			}
		}
	}
	return events, sourceMaxTurn, nil
}

// recordFork writes [journal.SessionForked] and then events, in order, into
// e's own journal — the forked session's own record, never the source's.
//
// It is called once, right after [Engine.Start] has written this session's
// own SessionStarted, so the marker and the copied block that follows it are
// never mistaken for how this session itself began. e.append is what [New]'s
// [Engine.Start] already uses, so this reaches the same journal — the teed
// one Options.Events observes — through the same path, rather than a second
// way to write an event that an observer could miss.
func (e *Engine) recordFork(ctx context.Context, src ForkSource, events []journal.Event) error {
	if _, err := e.append(ctx, 0, journal.SessionForked{
		SourceSessionID: src.SessionID,
		SourceTurn:      src.Turn,
		Copied:          len(events),
	}); err != nil {
		return fmt.Errorf("engine: recording the fork from session %s turn %d: %w", src.SessionID, src.Turn, err)
	}
	for _, ev := range events {
		if _, err := e.append(ctx, ev.Turn, ev.Payload); err != nil {
			return fmt.Errorf("engine: copying session %s's history into the fork: %w", src.SessionID, err)
		}
	}
	return nil
}

// clearWorkingTreeForFork removes everything directly under root except its
// git directory entry and StateDir, so the [repo.Repo.Restore] call that
// follows starts from an empty slate rather than layering the source's turn
// onto whatever later turns (in the source session, or in some other
// session that has since run against this same tree) left behind.
//
// It exists because [repo.Repo.Restore] deliberately does not do this on its
// own: its own doc comment argues that "does dest end up holding nothing the
// tree does not" is a materially bigger promise than "read a tree back out
// safely", and leaves it to whichever caller's operation actually needs an
// exact checkout. Forking is exactly that caller — [Fork]'s own doc comment
// argues why an approximate tree is not an acceptable outcome here the way
// it is for an ordinary resume — so this is that caller making the decision
// Restore leaves open, rather than a gap in Restore.
//
// The two names skipped are the only two directories any [Repo] method in
// this codebase ever writes: the git directory entry (a directory in an
// ordinary checkout, a gitdir-pointer *file* in a linked worktree — skipping
// the name ".git" covers both without this function needing to know which)
// and StateDir, which holds the session's own lock file, journal and
// throwaway snapshot index — files this very call is running underneath.
// Removing either would mean forking corrupts the git state it promises
// never to touch, or deletes the lock the running process is holding.
func clearWorkingTreeForFork(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("engine: listing %s before restoring a fork's tree: %w", root, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" || name == repo.StateDir {
			continue
		}
		path := filepath.Join(root, name)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("engine: clearing %s before restoring a fork's tree: %w", path, err)
		}
	}
	return nil
}
