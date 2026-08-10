package journal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/journal"
)

const testSessionID = "01JQ0000000000000000000000"

// clock is the injected clock. It advances a fixed step per read so every
// event gets a distinct, predictable timestamp, and it is mutex-guarded
// because Append is called concurrently and the suite runs with -race.
type clock struct {
	mu   sync.Mutex
	at   time.Time
	step time.Duration
}

func newClock() *clock {
	return &clock{at: time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC), step: time.Second}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.at
	c.at = c.at.Add(c.step)
	return t
}

// open creates a journal under a temp root with the clock injected and
// redaction disabled, which is the shape most of these tests want. Close runs
// on failure paths too, so a test that fails mid-way still releases the file.
func open(t *testing.T, root string, opts ...journal.Option) *journal.FileJournal {
	t.Helper()
	base := []journal.Option{journal.WithClock(newClock().now), journal.WithSecretEnv()}
	j, err := journal.Open(root, testSessionID, append(base, opts...)...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return j
}

func eventsPath(root string) string {
	return filepath.Join(journal.SessionDir(root, testSessionID), journal.EventsFile)
}

func readRaw(t *testing.T, root string) []byte {
	t.Helper()
	b, err := os.ReadFile(eventsPath(root))
	if err != nil {
		t.Fatalf("reading the events file: %v", err)
	}
	return b
}

// collect drains Read, returning the events yielded before any error and the
// error that stopped it. Both halves matter: a truncated tail must not cost a
// reader the intact prefix.
func collect(t *testing.T, j *journal.FileJournal) ([]journal.Event, error) {
	t.Helper()
	var out []journal.Event
	for ev, err := range j.Read(context.Background()) {
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
	return out, nil
}

func msg(s string) journal.Payload { return journal.UserMessage{Text: journal.InlineText(s)} }

// TestAppendAssignsMonotonicSeq is the card's first acceptance criterion: seq
// is the journal's to assign, 1-based, and increasing by one.
func TestAppendAssignsMonotonicSeq(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)

	for i := 1; i <= 5; i++ {
		ev, err := j.Append(context.Background(), i, msg(fmt.Sprintf("message %d", i)))
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if ev.Seq != uint64(i) {
			t.Errorf("Append %d returned seq %d, want %d", i, ev.Seq, i)
		}
		if ev.SessionID != testSessionID {
			t.Errorf("Append %d returned session %q, want %q", i, ev.SessionID, testSessionID)
		}
		if ev.SchemaVersion != journal.SchemaVersion {
			t.Errorf("Append %d returned schema_version %d, want %d", i, ev.SchemaVersion, journal.SchemaVersion)
		}
		if ev.Turn != i {
			t.Errorf("Append %d returned turn %d, want %d", i, ev.Turn, i)
		}
	}

	got, err := collect(t, j)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("read %d events, want 5", len(got))
	}
	for i, ev := range got {
		if ev.Seq != uint64(i+1) {
			t.Errorf("event %d on disk has seq %d, want %d", i, ev.Seq, i+1)
		}
	}
}

// TestSeqContinuesAcrossReopen: "monotonic across a session" has to survive the
// journal being reopened, or a restarted session writes a second seq 1.
func TestSeqContinuesAcrossReopen(t *testing.T) {
	root := t.TempDir()

	first, err := journal.Open(root, testSessionID, journal.WithClock(newClock().now), journal.WithSecretEnv())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := first.Append(context.Background(), 1, msg("before")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := open(t, root)
	ev, err := second.Append(context.Background(), 2, msg("after"))
	if err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	if ev.Seq != 4 {
		t.Fatalf("seq after reopen = %d, want 4 — a reopened session must not restart numbering", ev.Seq)
	}

	got, err := collect(t, second)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("read %d events, want 4", len(got))
	}
}

// TestConcurrentAppend: the loop is concurrent, so two goroutines must be
// unable to share a seq or interleave halves of a line.
func TestConcurrentAppend(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)

	const writers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := j.Append(context.Background(), w, msg(fmt.Sprintf("w%d-%d", w, i))); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Append: %v", err)
	}

	got, err := collect(t, j)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != writers*each {
		t.Fatalf("read %d events, want %d", len(got), writers*each)
	}
	seen := map[uint64]bool{}
	for i, ev := range got {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d, want %d — seqs are not dense and ordered", i, ev.Seq, i+1)
		}
		if seen[ev.Seq] {
			t.Fatalf("seq %d was reused", ev.Seq)
		}
		seen[ev.Seq] = true
	}
}

// TestTruncatedFinalLineIsDetected is the card's second acceptance criterion.
//
// The subtests are the two shapes a crash leaves behind, and the first is the
// dangerous one: bytes that parse perfectly as an event but were never
// terminated. Detection keys on the missing newline, so a parseable partial
// line is caught exactly like an unparseable one.
func TestTruncatedFinalLineIsDetected(t *testing.T) {
	cases := map[string]struct {
		// cut is how many bytes to remove from the end of the file.
		cut   int
		wantJ bool // the remaining partial line is still valid JSON
	}{
		"valid JSON, no terminator": {cut: 1, wantJ: true},
		"cut mid-object":            {cut: 25},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			j := open(t, root)
			for i := 1; i <= 3; i++ {
				if _, err := j.Append(context.Background(), i, msg(fmt.Sprintf("message %d", i))); err != nil {
					t.Fatalf("Append: %v", err)
				}
			}

			path := eventsPath(root)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if err := os.Truncate(path, info.Size()-int64(tc.cut)); err != nil {
				t.Fatalf("Truncate: %v", err)
			}

			// Establish that the partial line really is the dangerous case, or
			// the test proves less than it claims.
			raw := readRaw(t, root)
			tail := raw[bytes.LastIndexByte(raw[:len(raw)-1], '\n')+1:]
			if valid := json.Valid(tail); valid != tc.wantJ {
				t.Fatalf("partial tail json.Valid = %v, want %v — the fixture does not test what it says", valid, tc.wantJ)
			}

			got, err := collect(t, j)
			if err == nil {
				t.Fatalf("a truncated final line read clean: got %d events, no error", len(got))
			}
			if !errors.Is(err, journal.ErrTruncatedLine) {
				t.Fatalf("errors.Is(err, ErrTruncatedLine) = false: %v", err)
			}

			var re *journal.ReadError
			if !errors.As(err, &re) {
				t.Fatalf("errors.As(*ReadError) = false: %v", err)
			}
			if re.Line != 3 {
				t.Errorf("ReadError.Line = %d, want 3 — the error must name the line", re.Line)
			}
			if re.Path != path {
				t.Errorf("ReadError.Path = %q, want %q", re.Path, path)
			}
			if re.Offset <= 0 {
				t.Errorf("ReadError.Offset = %d, want the byte offset of the partial line", re.Offset)
			}

			// The intact prefix is still delivered.
			if len(got) != 2 {
				t.Fatalf("got %d events before the error, want the 2 intact ones", len(got))
			}
			if got[0].Seq != 1 || got[1].Seq != 2 {
				t.Errorf("prefix seqs = %d,%d, want 1,2", got[0].Seq, got[1].Seq)
			}
		})
	}
}

// TestOpenRefusesToAppendAfterATruncatedLine: appending after a partial line
// would splice the next event onto it and turn one crash into two lost events.
func TestOpenRefusesToAppendAfterATruncatedLine(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)
	if _, err := j.Append(context.Background(), 1, msg("only message")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	path := eventsPath(root)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if err := os.Truncate(path, info.Size()-1); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	reopened, err := journal.Open(root, testSessionID, journal.WithClock(newClock().now), journal.WithSecretEnv())
	if err == nil {
		_ = reopened.Close()
		t.Fatal("Open accepted a journal whose final line was truncated")
	}
	if !errors.Is(err, journal.ErrTruncatedLine) {
		t.Errorf("errors.Is(err, ErrTruncatedLine) = false: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file: %v", err)
	}
}

// TestNonMonotonicSeqIsDetected: a file whose seqs go backwards is not one
// session's record, and reading it in order would be a lie.
func TestNonMonotonicSeqIsDetected(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)
	for i := 1; i <= 2; i++ {
		if _, err := j.Append(context.Background(), i, msg("m")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// Re-append line 1 by hand, which is what a naive concatenation of two
	// runs would produce.
	raw := readRaw(t, root)
	first := raw[:bytes.IndexByte(raw, '\n')+1]
	f, err := os.OpenFile(eventsPath(root), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.Write(first); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = collect(t, j)
	if !errors.Is(err, journal.ErrSeqNotMonotonic) {
		t.Fatalf("errors.Is(err, ErrSeqNotMonotonic) = false: %v", err)
	}
	var re *journal.ReadError
	if !errors.As(err, &re) || re.Line != 3 {
		t.Errorf("want a ReadError naming line 3, got %v", err)
	}
}

// TestMalformedLineIsReported: a terminated line that is not an event stops the
// read with a typed error rather than being skipped.
func TestMalformedLineIsReported(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)
	if _, err := j.Append(context.Background(), 1, msg("m")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	f, err := os.OpenFile(eventsPath(root), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString("{not json}\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := collect(t, j)
	if err == nil {
		t.Fatal("a malformed line read clean")
	}
	var re *journal.ReadError
	if !errors.As(err, &re) {
		t.Fatalf("errors.As(*ReadError) = false: %v", err)
	}
	if re.Line != 2 {
		t.Errorf("ReadError.Line = %d, want 2", re.Line)
	}
	if len(got) != 1 {
		t.Errorf("got %d events before the error, want 1", len(got))
	}
}

// TestSecretNeverReachesDisk is the redaction guard.
//
// ToolResult.Output is the one credential back door left: no payload field can
// hold the key, but a model that runs `env` in a shell puts it in tool output
// verbatim. Append is the last place that can stop it.
func TestSecretNeverReachesDisk(t *testing.T) {
	// Low-entropy on purpose: this is a fixture, not a key, and it should not
	// look like one to a secret scanner either.
	const fakeKey = "not-a-real-key-not-a-real-key"
	t.Setenv("OPENROUTER_API_KEY", fakeKey)

	root := t.TempDir()
	// No WithSecretEnv: this exercises the default, which is the path
	// production takes.
	j, err := journal.Open(root, testSessionID, journal.WithClock(newClock().now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	output := "PATH=/usr/bin\nOPENROUTER_API_KEY=" + fakeKey + "\nHOME=/home/jian\n"
	ev, err := j.Append(context.Background(), 1, journal.ToolResult{
		CallID: "call_1",
		Tool:   "run_shell",
		Output: journal.InlineText(output),
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	raw := readRaw(t, root)
	if bytes.Contains(raw, []byte(fakeKey)) {
		t.Fatalf("the key is on disk in %s", eventsPath(root))
	}
	if !bytes.Contains(raw, []byte("[redacted:OPENROUTER_API_KEY]")) {
		t.Errorf("the line does not say a value was removed:\n%s", raw)
	}
	// The surrounding output is untouched — redaction removes the secret, not
	// the diagnostic context around it.
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/jian"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("redaction removed more than the secret: %q is gone", want)
		}
	}

	// Append returns the event as written, so the caller is not handed a value
	// that differs from the record.
	result, ok := ev.Payload.(journal.ToolResult)
	if !ok {
		t.Fatalf("returned payload is %T, want journal.ToolResult", ev.Payload)
	}
	if strings.Contains(result.Output.Inline, fakeKey) {
		t.Error("Append returned the unredacted output — the caller and the record disagree")
	}

	// And nothing incidental prints it either.
	if strings.Contains(fmt.Sprintf("%v %+v", j, j), fakeKey) {
		t.Error("printing the journal exposes the secret it redacts")
	}
}

// TestShortEnvValueIsNotRedacted: a two-character variable is not a credential,
// and substituting every occurrence of it would corrupt the record to protect
// nothing.
func TestShortEnvValueIsNotRedacted(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "no")

	root := t.TempDir()
	j, err := journal.Open(root, testSessionID, journal.WithClock(newClock().now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	if _, err := j.Append(context.Background(), 1, msg("there is no problem here")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	raw := readRaw(t, root)
	if bytes.Contains(raw, []byte("[redacted")) {
		t.Errorf("a 2-character value was treated as a secret:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte("there is no problem here")) {
		t.Errorf("the message was mangled:\n%s", raw)
	}
}

// TestLinesAreNotHTMLEscaped: json.Marshal re-escapes HTML in whatever a
// MarshalJSON method returns, which would put unicode-escape noise through
// every shell command and diff line in the record. The journal is meant to be
// read by a human auditing a session.
func TestLinesAreNotHTMLEscaped(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)

	const cmd = "go test ./... && gofmt -l . <src >out"
	if _, err := j.Append(context.Background(), 1, journal.ToolResult{
		CallID: "call_1",
		Tool:   "run_shell",
		Output: journal.InlineText(cmd),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	raw := readRaw(t, root)
	if !bytes.Contains(raw, []byte(cmd)) {
		t.Errorf("the command is not on disk verbatim:\n%s", raw)
	}
	// And the escaped spelling is absent. Deriving it from json.Marshal rather
	// than writing it out means this asserts against the encoder actually being
	// avoided, not against a hand-copied escape sequence.
	quoted, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	escaped := quoted[1 : len(quoted)-1]
	if bytes.Equal(escaped, []byte(cmd)) {
		t.Fatal("json.Marshal did not escape the fixture — the test proves nothing")
	}
	if bytes.Contains(raw, escaped) {
		t.Errorf("line is HTML-escaped — encode with journal.Marshal, not json.Marshal:\n%s", raw)
	}
}

// TestLargePayloadIsNotTruncated: the blob spill is a later card, and until it
// lands an oversized payload is written whole. Clipping it would be the exact
// failure the spill exists to avoid, arriving early.
func TestLargePayloadIsNotTruncated(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)

	big := strings.Repeat("diagnostic output that justifies the fix\n", 6000) // ~240 KiB
	if _, err := j.Append(context.Background(), 1, journal.ToolResult{
		CallID: "call_1",
		Tool:   "run_shell",
		Output: journal.InlineText(big),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := collect(t, j)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d events, want 1", len(got))
	}
	result, ok := got[0].Payload.(journal.ToolResult)
	if !ok {
		t.Fatalf("payload is %T, want journal.ToolResult", got[0].Payload)
	}
	if result.Output.Inline != big {
		t.Errorf("output changed: %d bytes on disk, %d written", len(result.Output.Inline), len(big))
	}
}

// TestEventsAreOnDiskBeforeClose: each Append fsyncs, so a crash before Close
// still leaves the events behind.
func TestEventsAreOnDiskBeforeClose(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)
	if _, err := j.Append(context.Background(), 1, msg("recorded")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	raw := readRaw(t, root)
	if !bytes.Contains(raw, []byte("recorded")) {
		t.Errorf("the event is not on disk before Close:\n%s", raw)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Error("the line has no terminator — a reader would take it for a truncated write")
	}
}

// TestTimestampsComeFromTheInjectedClock: a time.Now() inside Append would make
// byte-identical replay impossible.
func TestTimestampsComeFromTheInjectedClock(t *testing.T) {
	root := t.TempDir()
	c := newClock()
	j, err := journal.Open(root, testSessionID, journal.WithClock(c.now), journal.WithSecretEnv())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	// The first tick is Session.StartedAt, so events start one step later.
	if got := j.Session().StartedAt; !got.Equal(time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("StartedAt = %v, want the clock's first reading", got)
	}
	for i := 1; i <= 2; i++ {
		ev, err := j.Append(context.Background(), i, msg("m"))
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		want := time.Date(2026, 8, 11, 9, 30, i, 0, time.UTC)
		if !ev.Time.Equal(want) {
			t.Errorf("event %d stamped %v, want %v", i, ev.Time, want)
		}
	}
	if !bytes.Contains(readRaw(t, root), []byte(`"ts":"2026-08-11T09:30:01Z"`)) {
		t.Error("the injected time did not reach the line")
	}
}

// TestLayout pins where a session's record lives.
func TestLayout(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)

	wantDir := filepath.Join(root, ".kopicode", "sessions", testSessionID)
	if got := j.Session().Dir; got != wantDir {
		t.Errorf("Session().Dir = %q, want %q", got, wantDir)
	}
	if got, want := j.Path(), filepath.Join(wantDir, "events.jsonl"); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
	if got := j.Session().ID; got != testSessionID {
		t.Errorf("Session().ID = %q, want %q", got, testSessionID)
	}
	if _, err := os.Stat(wantDir); err != nil {
		t.Errorf("session directory was not created: %v", err)
	}
}

// TestAppendAfterCloseFails: a closed journal has no file, and an event that
// looks recorded but is not is worse than an error.
func TestAppendAfterCloseFails(t *testing.T) {
	root := t.TempDir()
	j, err := journal.Open(root, testSessionID, journal.WithClock(newClock().now), journal.WithSecretEnv())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Errorf("a second Close should be a no-op, got %v", err)
	}
	if _, err := j.Append(context.Background(), 1, msg("too late")); !errors.Is(err, journal.ErrClosed) {
		t.Errorf("Append after Close: err = %v, want ErrClosed", err)
	}
}

// TestOpenRejectsAnEmptySessionID: the id is injected, and an empty one would
// put every session in the same directory.
func TestOpenRejectsAnEmptySessionID(t *testing.T) {
	_, err := journal.Open(t.TempDir(), "")
	if !errors.Is(err, journal.ErrMissingField) {
		t.Fatalf("Open with no id: err = %v, want ErrMissingField", err)
	}
	var fe *journal.FieldError
	if !errors.As(err, &fe) || fe.Field != "session_id" {
		t.Errorf("want a FieldError naming session_id, got %v", err)
	}
}

// TestOpenRejectsAnotherSessionsEvents: two sessions writing one file produce
// one record describing two histories.
func TestOpenRejectsAnotherSessionsEvents(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)
	if _, err := j.Append(context.Background(), 1, msg("mine")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Same directory, different id on the events inside it.
	dir := journal.SessionDir(root, "other")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, journal.EventsFile), readRaw(t, root), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	other, err := journal.Open(root, "other", journal.WithClock(newClock().now), journal.WithSecretEnv())
	if err == nil {
		_ = other.Close()
		t.Fatal("Open accepted an events file belonging to another session")
	}
	if !errors.Is(err, journal.ErrSessionMismatch) {
		t.Errorf("errors.Is(err, ErrSessionMismatch) = false: %v", err)
	}
}

// TestReadStopsOnACancelledContext: reading a long record has to be
// interruptible, and the reason has to be visible.
func TestReadStopsOnACancelledContext(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)
	if _, err := j.Append(context.Background(), 1, msg("m")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var got error
	for _, err := range j.Read(ctx) {
		got = err
		break
	}
	if !errors.Is(got, context.Canceled) {
		t.Errorf("Read on a cancelled context: err = %v, want context.Canceled", got)
	}
}
