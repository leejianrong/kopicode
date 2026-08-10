package journal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// On-disk layout of a session (docs/SLICE-1.md §1).
//
// .kopicode/ is kept out of git through .git/info/exclude rather than
// .gitignore, so no tracked file changes. That is session startup's job, not
// this package's: resolving the git directory has to cope with .git being a
// file inside a worktree, which is internal/repo's concern, and a journal has
// to work outside a repository at all.
const (
	// StateDir is kopicode's per-repository state directory.
	StateDir = ".kopicode"
	// SessionsSubdir holds one directory per session, under StateDir.
	SessionsSubdir = "sessions"
	// EventsFile is the JSONL event log inside a session directory.
	EventsFile = "events.jsonl"

	// The record is a private one. A journal carries what the model read and
	// what the user typed, so it is not group- or world-readable.
	dirPerm  = 0o700
	filePerm = 0o600
)

// SessionDir returns the directory a session's files live in, so callers agree
// on the layout instead of each re-deriving it.
func SessionDir(root, id string) string {
	return filepath.Join(root, StateDir, SessionsSubdir, id)
}

// Sentinel causes for the file implementation, for errors.Is.
var (
	// ErrTruncatedLine reports a final line with no newline terminator — a
	// crash caught a write in flight. It is never parsed; see ReadError.
	ErrTruncatedLine = errors.New("truncated final line: no newline terminator")
	// ErrMalformedLine reports a terminated line that is not a journal event.
	ErrMalformedLine = errors.New("malformed line")
	// ErrSeqNotMonotonic reports a seq that did not increase, which means the
	// file is not one session's record read in order.
	ErrSeqNotMonotonic = errors.New("seq is not monotonic")
	// ErrClosed reports an Append to a journal that is closed, or that a write
	// error already took out of service.
	ErrClosed = errors.New("journal is closed")
	// ErrSessionMismatch reports an existing events.jsonl written by a
	// different session id than the one being opened.
	ErrSessionMismatch = errors.New("events belong to a different session")
)

// ReadError names the line a read gave up on.
//
// A reader that is told only "the journal is corrupt" cannot act; one that is
// told the file, the line number and the byte offset can look at it. Err is one
// of the sentinels above, or the decode error underneath, so errors.Is and
// errors.As both reach through.
type ReadError struct {
	// Path is the events file.
	Path string
	// Line is 1-based, counting newline-terminated lines.
	Line int
	// Offset is the byte offset of the line's first byte.
	Offset int64
	// Detail says what was wrong, in words a human can act on.
	Detail string
	// Err is the sentinel or the underlying decode failure.
	Err error
}

func (e *ReadError) Error() string {
	msg := fmt.Sprintf("journal: %s:%d (byte %d): %v", e.Path, e.Line, e.Offset, e.Err)
	if e.Detail != "" {
		msg += " — " + e.Detail
	}
	return msg
}

func (e *ReadError) Unwrap() error { return e.Err }

// options carries what Open injects. Everything here exists because the
// alternative is unreproducible: a clock read inside Append cannot be asserted
// on, and a secret discovered by pattern-matching is a guess.
type options struct {
	now       func() time.Time
	secretEnv []string
}

// An Option configures Open.
type Option func(*options)

// WithClock injects the clock every event is stamped from.
//
// time.Now() inside the append path is untestable by construction, and
// SLICE-1's byte-identical-replay criterion needs the timestamps to come from
// somewhere a replay controls.
func WithClock(now func() time.Time) Option {
	return func(o *options) {
		if now != nil {
			o.now = now
		}
	}
}

// WithSecretEnv replaces the environment variables whose values are redacted
// out of every line written.
//
// Pass no names to disable redaction — appropriate only where nothing in the
// process holds a credential, which in practice means a test that is asserting
// on unredacted bytes.
func WithSecretEnv(names ...string) Option {
	return func(o *options) { o.secretEnv = append([]string(nil), names...) }
}

// FileJournal is the JSONL implementation of Journal: one event per line,
// appended and fsynced, at .kopicode/sessions/<id>/events.jsonl.
//
// It is safe for concurrent use. Append holds a mutex across seq assignment,
// encoding and the write, so two goroutines can neither share a seq nor
// interleave halves of a line. Cross-*process* exclusion is a different
// concern and belongs to the .kopicode/lock advisory lock (docs/SLICE-1.md §8);
// nothing here defends against a second kopicode opening the same file.
type FileJournal struct {
	sess Session
	path string

	mu     sync.Mutex
	f      *os.File
	seq    uint64
	closed bool
	// broken records the write or fsync error that took this journal out of
	// service. A partial line on disk must never be appended to: the next
	// event would splice onto it and produce one unreadable line out of two
	// good events.
	broken error

	now      func() time.Time
	redactor *redactor
}

var _ Journal = (*FileJournal)(nil)

// Open prepares the session directory and opens its event log for appending.
//
// root is the repository (or working) root; the layout under it is fixed. An
// existing events.jsonl is read to recover the last seq, so a reopened session
// continues its numbering rather than restarting at 1 — and so a file whose
// final line a crash truncated is refused here rather than appended to.
//
// The clock is injected (WithClock) and defaults to time.Now. Redaction takes
// its values from the environment at construction (WithSecretEnv), because
// kopicode knows the actual secret and a denylist of credential-looking shapes
// would miss the next provider's format.
func Open(root, id string, opts ...Option) (*FileJournal, error) {
	if id == "" {
		return nil, &FieldError{
			Field: "session_id",
			Err:   ErrMissingField,
			Hint:  "the session id is injected by the caller so a replay can reproduce a journal byte for byte",
		}
	}

	o := options{now: time.Now, secretEnv: DefaultSecretEnv()}
	for _, opt := range opts {
		opt(&o)
	}

	dir := SessionDir(root, id)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("journal: creating session directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, EventsFile)

	// O_APPEND is what makes a write land at the end of the file regardless of
	// what else has been written since, which is the property a log needs.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return nil, fmt.Errorf("journal: opening %s: %w", path, err)
	}

	// Creating a file is a directory modification, and it is not durable until
	// the directory itself is fsynced. Without this the events can survive a
	// power loss while the name pointing at them does not.
	if err := syncDir(dir); err != nil {
		_ = f.Close()
		return nil, err
	}

	last, err := resumeSeq(path, id)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	return &FileJournal{
		sess:     Session{ID: id, StartedAt: o.now().UTC(), Dir: dir},
		path:     path,
		f:        f,
		seq:      last,
		now:      o.now,
		redactor: newRedactor(o.secretEnv, os.LookupEnv),
	}, nil
}

// Session reports which session this journal records.
func (j *FileJournal) Session() Session { return j.sess }

// Path is the events file, for a surface that wants to tell the user where the
// record went.
func (j *FileJournal) Path() string { return j.path }

// String identifies the journal without its contents. FileJournal holds the
// values it redacts, and a %+v of the struct would print them; a Stringer is
// what stops an incidental log line from doing exactly what redaction exists to
// prevent.
func (j *FileJournal) String() string {
	return fmt.Sprintf("FileJournal(session %s, %s)", j.sess.ID, j.path)
}

// Append stamps the envelope and writes payload durably as one line.
//
// seq is assigned here, never by the caller: a caller-supplied sequence number
// is a caller-supplied bug. It is 1-based, per session, and increases by one
// per successful append; a rejected event does not consume one.
//
// The line is written whole — encode, redact, append the newline, one Write —
// and fsynced before returning, so a caller that has been told an event was
// recorded is being told the truth. Nothing on this path clips: the 64 KiB blob
// spill (KAN-770) hooks between the payload arriving and Marshal, and until it
// lands an oversized payload is written whole rather than shortened.
//
// ctx is deliberately not checked for cancellation. The journal is the record,
// and the events worth having most are the ones explaining why a turn stopped;
// a cancelled context that suppressed a SessionEnded would delete the evidence
// at exactly the moment it was needed.
func (j *FileJournal) Append(_ context.Context, turn int, payload Payload) (Event, error) {
	if payload == nil {
		return Event{}, &FieldError{Field: "payload", Err: ErrMissingField,
			Hint: "an event with no payload records nothing"}
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	if j.broken != nil {
		return Event{}, fmt.Errorf("journal: %s: %w: %w", j.path, ErrClosed, j.broken)
	}
	if j.closed {
		return Event{}, fmt.Errorf("journal: %s: %w", j.path, ErrClosed)
	}

	ev := Event{
		SchemaVersion: SchemaVersion,
		SessionID:     j.sess.ID,
		Seq:           j.seq + 1,
		Turn:          turn,
		Time:          j.now().UTC(),
		Payload:       payload,
	}

	// Marshal, not json.Marshal: encoding/json re-escapes HTML in whatever a
	// MarshalJSON method returns, so every "&&" in a shell command and every
	// "<" in a diff would land in the record as escape soup.
	line, err := Marshal(ev)
	if err != nil {
		return Event{}, err
	}

	scrubbed, changed := j.redactor.scrub(line)
	if changed {
		// Append promises the event as written. Once redaction has changed the
		// bytes, the only honest way to keep that promise is to hand back what
		// the line now decodes to.
		var written Event
		if err := json.Unmarshal(scrubbed, &written); err != nil {
			return Event{}, fmt.Errorf("journal: re-reading the redacted line for event seq %d: %w", ev.Seq, err)
		}
		ev = written
	}

	if _, err := j.f.Write(append(scrubbed, '\n')); err != nil {
		j.broken = err
		return Event{}, fmt.Errorf("journal: writing event seq %d to %s: %w", ev.Seq, j.path, err)
	}
	// Per event. A record that exists to survive a crash is worth an fsync per
	// event; batching would trade the guarantee for throughput this workload —
	// a handful of events per model turn — does not need.
	if err := j.f.Sync(); err != nil {
		j.broken = err
		return Event{}, fmt.Errorf("journal: fsync after event seq %d to %s: %w", ev.Seq, j.path, err)
	}

	j.seq++
	return ev, nil
}

// Read yields every event on disk in seq order.
//
// It opens its own descriptor, so reading a journal that is still being
// appended to is safe and yields everything fsynced so far.
func (j *FileJournal) Read(ctx context.Context) iter.Seq2[Event, error] {
	return readEvents(ctx, j.path)
}

// Close releases the descriptor. Every event is already fsynced, so closing
// loses nothing; a journal that was never closed is missing no data.
func (j *FileJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	if err := j.f.Close(); err != nil {
		return fmt.Errorf("journal: closing %s: %w", j.path, err)
	}
	return nil
}

// readEvents is the one reader. Open uses it too, so the truncated-line guard
// cannot hold on the read path while quietly not holding on the append path.
//
// # The truncated tail
//
// A crash mid-write leaves a line with no terminator. The dangerous case is the
// partial line that is still valid JSON — a write cut after the closing brace
// but before the newline parses perfectly and is silently wrong. So the
// terminator, not the parse, is what decides: an unterminated final chunk is
// reported as ErrTruncatedLine and never handed to the decoder.
//
// The events before it are yielded first, then the error, then iteration stops.
// A caller gets the intact prefix and is told exactly where the record ends;
// dropping the tail silently and parsing it silently are both wrong.
func readEvents(ctx context.Context, path string) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		f, err := os.Open(path)
		if err != nil {
			yield(Event{}, fmt.Errorf("journal: opening %s for read: %w", path, err))
			return
		}
		defer func() { _ = f.Close() }()

		br := bufio.NewReader(f)
		var offset int64
		var lastSeq uint64

		for line := 1; ; line++ {
			if err := ctx.Err(); err != nil {
				yield(Event{}, fmt.Errorf("journal: reading %s: %w", path, err))
				return
			}

			chunk, err := br.ReadBytes('\n')
			start := offset
			offset += int64(len(chunk))

			switch {
			case err != nil && !errors.Is(err, io.EOF):
				yield(Event{}, &ReadError{Path: path, Line: line, Offset: start,
					Detail: "read failed", Err: err})
				return
			case errors.Is(err, io.EOF) && len(chunk) == 0:
				return // clean end: the last line was terminated
			case errors.Is(err, io.EOF):
				yield(Event{}, &ReadError{Path: path, Line: line, Offset: start,
					Detail: fmt.Sprintf("%d bytes, unparsed: a crash cut this write in flight", len(chunk)),
					Err:    ErrTruncatedLine})
				return
			}

			body := bytes.TrimSuffix(chunk, []byte("\n"))
			if len(bytes.TrimSpace(body)) == 0 {
				yield(Event{}, &ReadError{Path: path, Line: line, Offset: start,
					Detail: "blank line: the log is append-only and every line is one event", Err: ErrMalformedLine})
				return
			}

			var ev Event
			if err := json.Unmarshal(body, &ev); err != nil {
				yield(Event{}, &ReadError{Path: path, Line: line, Offset: start,
					Detail: "line did not decode as an event", Err: err})
				return
			}
			if ev.Seq <= lastSeq {
				yield(Event{}, &ReadError{Path: path, Line: line, Offset: start,
					Detail: fmt.Sprintf("seq %d follows seq %d", ev.Seq, lastSeq), Err: ErrSeqNotMonotonic})
				return
			}
			lastSeq = ev.Seq

			if !yield(ev, nil) {
				return
			}
		}
	}
}

// resumeSeq returns the last seq in an existing events file, so a reopened
// session keeps counting instead of restarting at 1 and writing a second event
// under a seq that is already taken.
//
// It reads the whole file, which is what recovering the last seq honestly
// costs; the alternative — seeking to the tail — would skip the very integrity
// checks that make appending safe. Any error is fatal to the open: appending
// after a line that was cut mid-write splices the next event onto it.
func resumeSeq(path, id string) (uint64, error) {
	var last uint64
	for ev, err := range readEvents(context.Background(), path) {
		if err != nil {
			return 0, fmt.Errorf("journal: %s exists but cannot be appended to safely: %w", path, err)
		}
		if ev.SessionID != id {
			return 0, fmt.Errorf("journal: %s: %w: found %q, opening %q",
				path, ErrSessionMismatch, ev.SessionID, id)
		}
		last = ev.Seq
	}
	return last, nil
}

// syncDir fsyncs a directory so a file created in it survives a power loss.
// Without it the file's contents can be durable while the name pointing at them
// is not, which is a record that vanishes rather than one that is short.
//
// Windows has no equivalent — a directory handle cannot be flushed, and the
// call fails with access denied — so it is skipped there rather than failing
// the open. That is a real durability gap on Windows, not a portability
// nicety.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("journal: opening %s to fsync it: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("journal: fsync of %s: %w", dir, err)
	}
	return nil
}
