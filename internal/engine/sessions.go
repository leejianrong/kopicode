package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/leejianrong/kopicode/internal/journal"
)

// This file is KAN-941's engine-level half: a front end cannot import
// internal/journal (ADR-0003's allowlist gives cmd/ three entries, and that
// is not one of them), so "what sessions exist under this directory" has to
// be answered from here or not at all. [Options.Resume] and [Fork] already
// need a caller who already knows a session id and a turn; this is what lets
// a caller find one in the first place, short of reading
// .kopicode/sessions by hand — the gap main.go's own --resume flag doc
// comment named as this card's job.

// SessionSummary is what [ListSessions] reports about one session on disk —
// enough for a human, or a script picking up an interrupted headless run, to
// recognise which one to resume or fork from without replaying it.
type SessionSummary struct {
	// ID is the session's directory name under .kopicode/sessions — what
	// [Options.SessionID], [Options.Resume] and [ForkSource.SessionID] all
	// expect.
	ID string `json:"id"`
	// StartedAt is this session's own SessionStarted timestamp.
	StartedAt time.Time `json:"started_at"`
	// ModelID and HarnessConfigHash identify the arm this session ran,
	// straight off SessionStarted.
	ModelID           string `json:"model_id,omitempty"`
	HarnessConfigHash string `json:"harness_config_hash,omitempty"`
	// FirstMessage is the text of this session's first UserMessage, or ""
	// when there was none yet or it spilled to a blob — see [ListSessions]'s
	// own doc comment for why a spilled message is not fetched here.
	FirstMessage string `json:"first_message,omitempty"`
	// Turns is the highest turn number this session's record reached. 0
	// means the session opened and wrote nothing beyond SessionStarted.
	Turns int `json:"turns"`
	// Ended reports whether this session's record carries a SessionEnded.
	// False does not distinguish "still running" from "the process was
	// killed before it could close" — [journal.SessionEnded] is the only
	// thing that could tell those apart, and a session that never wrote one
	// is exactly the case where nothing can.
	Ended bool `json:"ended"`
	// EndReason is SessionEnded.Reason, "" when Ended is false.
	EndReason string `json:"end_reason,omitempty"`
	// Forked reports whether this session's record carries a
	// journal.SessionForked marker — i.e. it began with [Fork] rather than
	// [Open].
	Forked bool `json:"forked"`
	// ForkedFrom and ForkedTurn are the marker's source session and turn,
	// zero values when Forked is false.
	ForkedFrom string `json:"forked_from,omitempty"`
	ForkedTurn int    `json:"forked_turn,omitempty"`
}

// ListSessions reports every session recorded under dir's
// .kopicode/sessions, oldest first.
//
// # Why a summary and not a replay
//
// A caller deciding whether to resume or fork a session needs enough to
// recognise it — when it started, which model it ran, what the human first
// asked, how far it got, whether it ended and how — and none of that needs
// the full turn-by-turn record [Options.Resume] and [Fork] themselves
// replay. So this reads each session's own events.jsonl once, through
// [journal.ReadSession] — the same reader [Options.Resume] uses, not
// [journal.ReadSessionBlobs] — and keeps a handful of scalars off it rather
// than materialising every event. A session whose first user message
// spilled to a blob reports an empty [SessionSummary.FirstMessage] rather
// than paying to fetch it, the opposite of the asymmetry [readForkSource]'s
// own doc comment argues, because here the caller only needs to recognise
// the session well enough to type its id, not act on its content.
//
// # What "cheap" means here
//
// Cheap relative to a full replay, not free: every event in every session's
// file is decoded once, because the last turn number and whether the
// session ended are only known once the file has been read to its end.
// Thousands of long-running sessions in one directory would make this slow;
// nothing in this project's usage today does, and a caller that needs to
// scale past that has [journal.ReadSession] itself to build a cheaper index
// from — the same primitive this function is built on.
//
// # What a missing directory means
//
// A directory that has never run a kopicode session has no
// .kopicode/sessions at all, and that is reported as zero sessions and no
// error — the same "not a failure" treatment [Open] gives a directory that
// is not a git repository, for the same reason: asking "what is here" should
// not have to distinguish "nothing" from "an error" for the most ordinary
// case.
func ListSessions(ctx context.Context, dir string) ([]SessionSummary, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("engine: resolving %s: %w", dir, err)
	}

	// journal.SessionDir joins root, StateDir, SessionsSubdir and id;
	// filepath.Join collapses a trailing empty element, so this is the
	// sessions directory itself without a second constant naming it here.
	sessionsDir := journal.SessionDir(abs, "")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("engine: listing sessions under %s: %w", sessionsDir, err)
	}

	var out []SessionSummary
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		summary, serr := summarizeSession(ctx, abs, id)
		if serr != nil {
			if errors.Is(serr, os.ErrNotExist) {
				// A directory under sessions/ with no events.jsonl is not a
				// session that ever recorded anything — e.g. one left by
				// something other than journal.Open — so it is skipped
				// rather than reported, the same way a stray file in the
				// directory already is by the entry.IsDir() check above.
				continue
			}
			return nil, fmt.Errorf("engine: reading session %s: %w", id, serr)
		}
		out = append(out, summary)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}

// summarizeSession reads one session's own record once and keeps the fields
// [SessionSummary] reports.
//
// It reads to the end of the file rather than stopping at the first
// SessionStarted, because the last turn number and whether the session ended
// are only knowable once every event has been seen — there is no separate
// index recording either, by design, since maintaining one would be a
// second place a session's own record could be summarised from and drift
// against. A read that hits an error (journal.ErrTruncatedLine or any
// other) after at least one event decoded keeps what came before rather
// than discarding it: a session a crash caught mid-write is still worth
// listing, and the summary just reports it exactly as it would report a
// session that is simply still running — honest, since ReadSession alone
// cannot tell the two apart either. An error on the very first read (most
// often os.ErrNotExist, when id names a directory with no events.jsonl at
// all) is returned rather than swallowed, so [ListSessions] can tell "this
// is not a session" from "this session's record could not be read" and
// treat them differently.
func summarizeSession(ctx context.Context, root, id string) (SessionSummary, error) {
	sum := SessionSummary{ID: id}
	sawFirstMessage := false
	n := 0
	for ev, rerr := range journal.ReadSession(ctx, journal.SessionDir(root, id)) {
		if rerr != nil {
			if n == 0 {
				return SessionSummary{}, rerr
			}
			break
		}
		n++
		if ev.Turn > sum.Turns {
			sum.Turns = ev.Turn
		}
		switch p := ev.Payload.(type) {
		case journal.SessionStarted:
			sum.StartedAt = ev.Time
			sum.ModelID = p.ModelID
			sum.HarnessConfigHash = p.HarnessConfigHash
		case journal.SessionForked:
			sum.Forked = true
			sum.ForkedFrom = p.SourceSessionID
			sum.ForkedTurn = p.SourceTurn
		case journal.UserMessage:
			if !sawFirstMessage {
				sawFirstMessage = true
				sum.FirstMessage = p.Text.Inline
			}
		case journal.SessionEnded:
			sum.Ended = true
			sum.EndReason = p.Reason
		}
	}
	return sum, nil
}
