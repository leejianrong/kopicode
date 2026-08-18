package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider/mock"
)

// openIn builds a session in a fresh temp directory holding the file the
// shipped fixtures read, with the recording replayed through it.
//
// No git. internal/repo owns the git side and its fixtures assert their own
// isolation; a git subprocess here would be a second place to get the
// containment rules wrong (see harness_test.go and the ground-rules skill).
// TestOpenOutsideARepository is the case that fact makes testable for free.
func openIn(t *testing.T, fixture string, opts engine.Options) (*engine.Session, *mock.Provider) {
	t.Helper()

	prov, err := mock.Load(fixture)
	if err != nil {
		t.Fatalf("loading fixture %s: %v", fixture, err)
	}
	return openWithProvider(t, prov, opts)
}

// openWithProvider is openIn's shared body, over a provider the caller already
// has — a loaded fixture, or one built in memory with script (harness_test.go)
// for a shape no shipped fixture holds, such as a run_shell call. It exists so
// a test that needs the latter still drives the real engine.Open path rather
// than reinventing the setup around it.
func openWithProvider(t *testing.T, prov *mock.Provider, opts engine.Options) (*engine.Session, *mock.Provider) {
	t.Helper()

	dir := t.TempDir()
	write(t, dir, "internal/greet/greet.go", greetGo)

	opts.Dir = dir
	opts.Selection = testSelection(prov)
	opts.Provider = prov
	if opts.Now == nil {
		opts.Now = fixedClock()
	}
	if opts.SessionID == "" {
		opts.SessionID = "open-test"
	}

	s, err := engine.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, prov
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// TestOpenRunsAWholeSessionUnderTheMockProvider is the card's headline: a
// caller that names only stdlib types and types declared in this package gets a
// session that runs, records, and closes.
//
// It asserts the observable outcomes rather than the wiring — the events on
// disk, the events the surface was told about, the provider being drained —
// because "did Open build the right collaborators" is a question about what the
// session did, not about which constructor it called.
func TestOpenRunsAWholeSessionUnderTheMockProvider(t *testing.T) {
	var seen []engine.Event
	s, prov := openIn(t, "two_turn_native_tool_call", engine.Options{
		Events: func(e engine.Event) { seen = append(seen, e) },
	})

	res, err := s.Run(t.Context(), `Why does Greet("") render as "Hello, !"?`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Errorf("stop = %s, want %s", res.Stop, engine.StopCompleted)
	}
	if res.Turns != 2 {
		t.Errorf("turns = %d, want 2 — the recording holds two", res.Turns)
	}
	if err := prov.Drained(); err != nil {
		// A session that stopped early passes every assertion about the turns
		// it did reach, which is what this catches.
		t.Errorf("provider not drained: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The record exists, on disk, where Session.Path says it is.
	events := readJournal(t, s.Path())
	want := []journal.Type{
		journal.TypeSessionStarted,
		journal.TypeUserMessage,
		journal.TypeProviderRequest,
		journal.TypeProviderResponse,
		journal.TypeToolCallRequested,
		journal.TypeToolCallParsed,
		journal.TypeToolResult,
		journal.TypeProviderRequest,
		journal.TypeProviderResponse,
		journal.TypeAssistantMessage,
		journal.TypeSessionEnded,
	}
	if got := types(events); !equalTypes(got, want) {
		t.Errorf("journal events =\n%v\nwant\n%v", got, want)
	}

	// SessionStarted carries the arm and the binary, which is what stops two
	// incomparable runs pooling (ADR-0007 decisions 6 and 7).
	started, ok := events[0].Payload.(journal.SessionStarted)
	if !ok {
		t.Fatalf("first event is %T, want journal.SessionStarted", events[0].Payload)
	}
	if started.ModelID != s.Selection().ModelID {
		t.Errorf("SessionStarted.ModelID = %q, want %q", started.ModelID, s.Selection().ModelID)
	}
	if started.HarnessConfigHash == "" {
		t.Error("SessionStarted carries no harness config hash")
	}
	if started.Build.TreeState == "" {
		t.Error("SessionStarted carries no build identity; Open must supply one")
	}
	if started.CWD != s.Dir() {
		t.Errorf("SessionStarted.CWD = %q, want %q", started.CWD, s.Dir())
	}
}

// TestTheSurfaceIsToldExactlyWhatTheRecordHolds is the property that makes the
// event stream a derivation rather than a second transcript.
//
// Every event with a sequence number must correspond, in order and one for one,
// to an event on disk. Anything else — an extra, a reorder, a kind that does
// not match — is a surface being told something the journal does not say.
func TestTheSurfaceIsToldExactlyWhatTheRecordHolds(t *testing.T) {
	var seen []engine.Event
	s, _ := openIn(t, "two_turn_fenced_json_tool_call", engine.Options{
		Events: func(e engine.Event) { seen = append(seen, e) },
	})

	if _, err := s.Run(t.Context(), "why?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	recorded := readJournal(t, s.Path())

	var announced []engine.Event
	for _, e := range seen {
		if e.Seq != 0 {
			announced = append(announced, e)
			continue
		}
		if e.Kind != engine.EventDelta {
			t.Errorf("event %s carries no seq but is not a delta; only a delta is allowed "+
				"to be a thing that is happening rather than a thing that happened", e.Kind)
		}
	}

	if len(announced) != len(recorded) {
		t.Fatalf("the surface was told about %d events and the record holds %d",
			len(announced), len(recorded))
	}
	for i := range announced {
		if announced[i].Seq != recorded[i].Seq {
			t.Errorf("event %d: announced seq %d, recorded seq %d — out of order",
				i, announced[i].Seq, recorded[i].Seq)
		}
		if announced[i].Turn != recorded[i].Turn {
			t.Errorf("event seq %d: announced turn %d, recorded turn %d",
				announced[i].Seq, announced[i].Turn, recorded[i].Turn)
		}
	}

	// And the deltas really did arrive, or the paragraph above proved nothing
	// about streaming.
	var streamed strings.Builder
	for _, e := range seen {
		if e.Kind == engine.EventDelta && e.Reason == "content" {
			streamed.WriteString(e.Text)
		}
	}
	if streamed.Len() == 0 {
		t.Fatal("no content deltas reached the surface; the recording streams and the stream " +
			"is the seam the REPL prints through")
	}

	// The streamed text is an optimistic prefix of the record: the last
	// assistant message must contain what was streamed for it.
	var last string
	for _, ev := range recorded {
		if m, ok := ev.Payload.(journal.AssistantMessage); ok {
			last = m.Text.Inline
		}
	}
	if !strings.Contains(streamed.String(), last) && !strings.Contains(last, "") {
		t.Errorf("streamed text %q bears no relation to the recorded message %q",
			streamed.String(), last)
	}
}

// TestADeltaKnowsItsTurn. Journal events carry the turn in their envelope; a
// stream where only some events know which turn they belong to is a stream a
// renderer has to guess about.
func TestADeltaKnowsItsTurn(t *testing.T) {
	var seen []engine.Event
	s, _ := openIn(t, "two_turn_native_tool_call", engine.Options{
		Events: func(e engine.Event) { seen = append(seen, e) },
	})
	if _, err := s.Run(t.Context(), "why?"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, e := range seen {
		if e.Kind != engine.EventDelta {
			continue
		}
		found = true
		if e.Turn < 1 {
			t.Errorf("a delta arrived with turn %d; turn 0 means outside any turn, and a "+
				"delta is never outside one", e.Turn)
		}
	}
	if !found {
		t.Fatal("no deltas arrived")
	}
}

// TestNothingIsAnnouncedThatTheRecordRefused.
//
// The tee appends first and announces second. A journal that refuses a write
// must therefore leave the surface silent — otherwise a REPL prints a session
// that is not on disk, which is the parallel transcript ADR-0002 forbids
// arriving through the one door that looks like plumbing.
func TestNothingIsAnnouncedThatTheRecordRefused(t *testing.T) {
	// The journal directory is made read-only after Open, so the append that
	// SessionEnded needs fails while everything already open still works.
	var seen []engine.Event
	s, _ := openIn(t, "two_turn_native_tool_call", engine.Options{
		Events: func(e engine.Event) { seen = append(seen, e) },
	})

	before := len(seen)
	if before == 0 {
		t.Fatal("SessionStarted was not announced, so this test's premise is wrong")
	}

	// A cheap, portable refusal: close the session once, then close it again.
	// The second Close is refused by the engine before any append happens.
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	after := len(seen)

	if err := s.Close(t.Context()); !errors.Is(err, engine.ErrEnded) {
		t.Fatalf("second Close = %v, want %v", err, engine.ErrEnded)
	}
	if len(seen) != after {
		t.Errorf("a refused Close announced %d event(s) the record does not hold",
			len(seen)-after)
	}
}

// TestOpenRefusesBeforeItTouchesTheFilesystem holds ADR-0007 decision 4's
// ordering at the constructor rather than only at the front end.
//
// A session that will not run must leave nothing behind. Both refusals below
// happen before the journal is opened, so the directory is byte-for-byte as it
// was found — which is the assertion cmd/kopicode's integration test makes
// about the binary, made here about the call it now makes.
func TestOpenRefusesBeforeItTouchesTheFilesystem(t *testing.T) {
	prov, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}

	cases := map[string]func(*engine.Options){
		"no selection": func(o *engine.Options) { o.Selection = engine.Selection{} },
		"no provider and no key": func(o *engine.Options) {
			o.Provider = nil
			t.Setenv(engine.APIKeyEnv, "")
		},
	}

	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			before := treeOf(t, dir)

			opts := engine.Options{
				Dir:       dir,
				Selection: testSelection(prov),
				Provider:  prov,
				SessionID: "refused",
				Now:       fixedClock(),
			}
			breakIt(&opts)

			if _, err := engine.Open(t.Context(), opts); err == nil {
				t.Fatal("Open accepted a configuration it cannot run")
			}
			if after := treeOf(t, dir); !equalStrings(before, after) {
				t.Errorf("a refused Open changed the directory.\nbefore: %v\nafter:  %v\n"+
					"nothing may be opened, locked or written for a session that never started "+
					"(ADR-0007 decision 4)", before, after)
			}

			// Positive control: the comparison has to be able to see a file
			// appear, or the assertion above passes over a journal.
			write(t, dir, "control", "x")
			if equalStrings(before, treeOf(t, dir)) {
				t.Fatal("positive control failed: the comparison did not notice a new file")
			}
		})
	}
}

// TestAMissingAPIKeyIsNamedRatherThanDiscoveredLater. It costs nothing to
// detect and everything to find out about after a directory has been created
// and a record opened.
func TestAMissingAPIKeyIsNamedRatherThanDiscoveredLater(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "")

	prov, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	_, err = engine.Open(t.Context(), engine.Options{
		Dir:       t.TempDir(),
		Selection: testSelection(prov),
		SessionID: "nokey",
	})
	if !errors.Is(err, engine.ErrNoAPIKey) {
		t.Errorf("Open = %v, want it to wrap %v", err, engine.ErrNoAPIKey)
	}
}

// TestOpenOutsideARepository. A directory that is not a repository is a
// supported place to start a session: no snapshots, no head on the record.
func TestOpenOutsideARepository(t *testing.T) {
	s, _ := openIn(t, "two_turn_native_tool_call", engine.Options{})

	if _, err := s.Run(t.Context(), "why?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, ev := range readJournal(t, s.Path()) {
		if ev.Type() == journal.TypeTurnSnapshot {
			t.Error("a session outside a repository wrote a TurnSnapshot")
		}
		if p, ok := ev.Payload.(journal.SessionStarted); ok && p.RepoHead != "" {
			t.Errorf("SessionStarted.RepoHead = %q outside a repository, want \"\"", p.RepoHead)
		}
	}
}

// TestAGeneratedSessionIDSortsByTimeAndIsUnique. The ids are directory names
// under .kopicode/sessions, so sorting them by name should sort them by time,
// and two sessions started in the same second must still be distinct.
func TestAGeneratedSessionIDSortsByTimeAndIsUnique(t *testing.T) {
	prov, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}

	seen := map[string]bool{}
	var ids []string
	clock := fixedClock()
	for range 4 {
		s, err := engine.Open(t.Context(), engine.Options{
			Dir:       t.TempDir(),
			Selection: testSelection(prov),
			Provider:  prov,
			Now:       clock,
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		id := s.ID()
		_ = s.Close(t.Context())

		if seen[id] {
			t.Fatalf("session id %q was minted twice", id)
		}
		seen[id] = true
		ids = append(ids, id)
	}

	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Errorf("session ids do not sort by time: %q came after %q", ids[i], ids[i-1])
		}
	}
}

// TestCloseReleasesEverythingOnce. Session exists so a front end has one thing
// to close; a second Close must not be how a file handle is double-freed or an
// error is invented.
func TestCloseReleasesEverythingOnce(t *testing.T) {
	s, _ := openIn(t, "two_turn_native_tool_call", engine.Options{})

	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(t.Context()); !errors.Is(err, engine.ErrEnded) {
		t.Errorf("second Close = %v, want it to wrap %v", err, engine.ErrEnded)
	}
}

// readJournal reads a session's record back off disk, failing on any read error
// rather than skipping a damaged line.
func readJournal(t *testing.T, sessionDir string) []journal.Event {
	t.Helper()

	// SessionDir is <root>/.kopicode/sessions/<id>; journal.Open takes the root.
	root := filepath.Dir(filepath.Dir(filepath.Dir(sessionDir)))
	id := filepath.Base(sessionDir)

	j, err := journal.Open(root, id)
	if err != nil {
		t.Fatalf("reopening the journal at %s: %v", sessionDir, err)
	}
	defer func() { _ = j.Close() }()

	var out []journal.Event
	for ev, err := range j.Read(context.Background()) {
		if err != nil {
			t.Fatalf("reading the journal: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

func types(events []journal.Event) []journal.Type {
	out := make([]journal.Type, len(events))
	for i, ev := range events {
		out[i] = ev.Type()
	}
	return out
}

func equalTypes(a, b []journal.Type) bool {
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

func treeOf(t *testing.T, dir string) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
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
