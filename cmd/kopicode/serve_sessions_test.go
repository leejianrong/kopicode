package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
)

// sessionsOf asks server.sessions and returns its rows keyed by session id.
func sessionsOf(t *testing.T, h *serveHarness, id int) map[string]map[string]any {
	t.Helper()
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": methodServerSessions})
	resp := h.awaitResponse(id)
	if resp["error"] != nil {
		t.Fatalf("server.sessions: %v", resp["error"])
	}
	res, _ := resp["result"].(map[string]any)
	list, ok := res["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions is %T, want a list even when empty: %v", res["sessions"], res)
	}
	rows := map[string]map[string]any{}
	for _, r := range list {
		row, _ := r.(map[string]any)
		rows[row["session"].(string)] = row
	}
	return rows
}

func TestServeSessionsIsAnEmptyListBeforeAnySessionStarts(t *testing.T) {
	h := startServe(t, engine.Options{})
	if rows := sessionsOf(t, h, 1); len(rows) != 0 {
		t.Errorf("rows = %v, want none", rows)
	}
	h.close()
}

// TestServeSessionsSaysWhatEachSessionIsDoing walks one session through every
// state the table reports: blocked on a consent.request (with the request's id),
// idle once it is answered and the turn has finished, and ended after a close.
func TestServeSessionsSaysWhatEachSessionIsDoing(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "run_shell", `{"command":"echo hi"}`),
		sseBody("done"),
	)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})
	dir := t.TempDir()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": dir, "prompt": "run a command",
			"consent_mode": consentModeRemoteInteractive}})

	req := h.awaitRequest(methodConsentRequest)
	row := sessionsOf(t, h, 2)["s1"]
	if row == nil {
		t.Fatal("s1 is not listed while it waits on consent")
	}
	if row["state"] != "awaiting_consent" || row["pending_request"] != req["id"] {
		t.Errorf("while blocked: state %v pending %v, want awaiting_consent and %v", row["state"], row["pending_request"], req["id"])
	}
	if got, _ := filepath.EvalSymlinks(row["dir"].(string)); got != mustEval(t, dir) {
		t.Errorf("dir = %v, want %v", row["dir"], dir)
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"answer": "allow"}})
	if resp := h.awaitResponse(1); resp["error"] != nil {
		t.Fatalf("session.start: %v", resp["error"])
	}

	row = sessionsOf(t, h, 3)["s1"]
	if row["state"] != "idle" || row["last_stop"] != "completed" || row["pending_request"] != nil {
		t.Errorf("after the turn: %v", row)
	}
	if turn, _ := row["turn"].(float64); turn < 1 {
		t.Errorf("turn = %v, want at least 1", row["turn"])
	}
	if usage, _ := row["usage"].(map[string]any); usage == nil || usage["requests"] == nil {
		t.Errorf("usage = %v, want session.usage's shape", row["usage"])
	}
	at, _ := row["last_event_at"].(string)
	if when, err := time.Parse(time.RFC3339Nano, at); err != nil || time.Since(when) > time.Minute {
		t.Errorf("last_event_at = %q (%v), want a recent RFC 3339 time", at, err)
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": methodSessionClose, "params": map[string]any{"session": "s1"}})
	if resp := h.awaitResponse(4); resp["error"] != nil {
		t.Fatalf("session.close: %v", resp["error"])
	}
	row = sessionsOf(t, h, 5)["s1"]
	if row == nil || row["state"] != "ended" || row["last_stop"] != "completed" {
		t.Errorf("after close: %v, want s1 listed as ended", row)
	}
	h.close()
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	got, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestServeSessionsReportsAwaitingAnswer: an ask.request the client has not
// answered is its own state, apart from a consent one.
func TestServeSessionsReportsAwaitingAnswer(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "ask", `{"question":"tabs or spaces?","context":"x"}`),
		sseBody("done"),
	)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": t.TempDir(), "prompt": "style it",
			"consent_mode": consentModeRemoteInteractive, "ask_mode": "remote"}})

	req := h.awaitRequest(methodAskRequest)
	row := sessionsOf(t, h, 2)["s1"]
	if row["state"] != "awaiting_answer" || row["pending_request"] != req["id"] {
		t.Errorf("while asked: %v", row)
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"text": "tabs"}})
	h.awaitResponse(1)
	if row := sessionsOf(t, h, 3)["s1"]; row["state"] != "idle" {
		t.Errorf("after the answer: %v", row)
	}
	h.close()
}

// replay asks session.events and returns the result.
func replay(t *testing.T, h *serveHarness, id int, params map[string]any) map[string]any {
	t.Helper()
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": methodSessionEvents, "params": params})
	resp := h.awaitResponse(id)
	if resp["error"] != nil {
		t.Fatalf("session.events: %v", resp["error"])
	}
	res, _ := resp["result"].(map[string]any)
	return res
}

func seqsOf(t *testing.T, events any) []float64 {
	t.Helper()
	list, _ := events.([]any)
	var seqs []float64
	for _, e := range list {
		seq, _ := e.(map[string]any)["seq"].(float64)
		seqs = append(seqs, seq)
	}
	return seqs
}

// TestServeEventsReplaysWhatTheClientMissed: a replay holds exactly the events
// the session announced, with the same seq, in order; after_seq resumes where the
// client left off; limit pages, with more and last_seq to carry on.
func TestServeEventsReplaysWhatTheClientMissed(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("first"), sseBody("second"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), "hello")})
	h.awaitResponse(1)
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "again"}})
	h.awaitResponse(2)

	// What the session announced live carries its seq.
	var live []float64
	for _, n := range h.notificationsFor("s1") {
		ev, _ := n["params"].(map[string]any)["event"].(map[string]any)
		seq, _ := ev["seq"].(float64)
		if seq == 0 {
			t.Fatalf("a session.event with no seq: %v", ev)
		}
		live = append(live, seq)
	}

	all := replay(t, h, 3, map[string]any{"session": "s1"})
	got := seqsOf(t, all["events"])
	if len(got) < 6 || got[0] != 1 || all["more"] != false {
		t.Fatalf("replay seqs = %v more %v", got, all["more"])
	}
	if len(got) != len(live) {
		t.Fatalf("replay has %d events, the session announced %d", len(got), len(live))
	}
	for i := range got {
		if got[i] != live[i] {
			t.Fatalf("replay seqs %v differ from the announced %v", got, live)
		}
	}
	if all["last_seq"] != got[len(got)-1] {
		t.Errorf("last_seq = %v, want %v", all["last_seq"], got[len(got)-1])
	}

	tail := replay(t, h, 4, map[string]any{"session": "s1", "after_seq": got[2]})
	if want := got[3:]; len(seqsOf(t, tail["events"])) != len(want) || seqsOf(t, tail["events"])[0] != want[0] {
		t.Errorf("after_seq %v returned %v, want %v", got[2], seqsOf(t, tail["events"]), want)
	}

	// Page through two at a time: the pages join into the whole record.
	var paged []float64
	after := float64(0)
	for i, id := 0, 10; ; i, id = i+1, id+1 {
		if i > len(got) {
			t.Fatal("paging never ended")
		}
		page := replay(t, h, id, map[string]any{"session": "s1", "after_seq": after, "limit": 2})
		paged = append(paged, seqsOf(t, page["events"])...)
		after, _ = page["last_seq"].(float64)
		if page["more"] != true {
			break
		}
	}
	if len(paged) != len(got) {
		t.Errorf("paged %v, want %v", paged, got)
	}

	// Nothing after the last event: an empty page that keeps the client's place.
	none := replay(t, h, 5, map[string]any{"session": "s1", "after_seq": all["last_seq"]})
	if len(seqsOf(t, none["events"])) != 0 || none["last_seq"] != all["last_seq"] || none["more"] != false {
		t.Errorf("past the end: %v", none)
	}
	h.close()
}

// TestServeEventsReadsSpilledTextBack: the event a session announces carries no
// text for a value that spilled to a blob, only its size; the replay carries all
// of it.
func TestServeEventsReadsSpilledTextBack(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("ok"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})
	prompt := strings.Repeat("x", 100<<10)
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), prompt)})
	h.awaitResponse(1)

	var liveText any = "unset"
	for _, n := range h.notificationsFor("s1") {
		ev, _ := n["params"].(map[string]any)["event"].(map[string]any)
		if ev["kind"] == "user_message" {
			liveText = ev["text"]
		}
	}
	if liveText != nil && liveText != "" {
		t.Fatalf("the live user_message carried %d bytes of text; the spill is not what this test needs", len(liveText.(string)))
	}

	res := replay(t, h, 2, map[string]any{"session": "s1"})
	var replayed string
	for _, e := range res["events"].([]any) {
		if ev := e.(map[string]any); ev["kind"] == "user_message" {
			replayed, _ = ev["text"].(string)
		}
	}
	if replayed != prompt {
		t.Errorf("replayed user_message holds %d bytes, want %d", len(replayed), len(prompt))
	}
	if res["problems"] != nil {
		t.Errorf("problems = %v", res["problems"])
	}
	h.close()
}

// TestServeEventsServesAnEndedSessionAndRefusesAnUnknownOne.
func TestServeEventsServesAnEndedSessionAndRefusesAnUnknownOne(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("done"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), "go")})
	h.awaitResponse(1)
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionClose, "params": map[string]any{"session": "s1"}})
	h.awaitResponse(2)

	res := replay(t, h, 3, map[string]any{"session": "s1"})
	kinds := []string{}
	for _, e := range res["events"].([]any) {
		kinds = append(kinds, e.(map[string]any)["kind"].(string))
	}
	if len(kinds) == 0 || kinds[len(kinds)-1] != "session_ended" {
		t.Errorf("an ended session's replay ends %v, want session_ended last", kinds)
	}

	for i, tc := range []struct {
		params map[string]any
		code   int
	}{
		{map[string]any{"session": "nope"}, codeUnknownSession},
		{map[string]any{}, codeInvalidParams},
		{map[string]any{"session": "s1", "limit": -1}, codeInvalidParams},
		{map[string]any{"session": "s1", "after_seq": -1}, codeInvalidParams},
	} {
		id := 10 + i
		h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": methodSessionEvents, "params": tc.params})
		if e, _ := h.awaitResponse(id)["error"].(map[string]any); e == nil || !numEq(e["code"], tc.code) {
			t.Errorf("params %v: error = %v, want code %d", tc.params, e, tc.code)
		}
	}
	h.close()
}

// TestEventsAfterReadsPastAnUnfinishedLine: a writer caught mid-line is the end
// of what can be read now, not a failure of the read.
func TestEventsAfterReadsPastAnUnfinishedLine(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("done"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})
	dir := t.TempDir()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", dir, "go")})
	rec, _ := h.awaitResponse(1)["result"].(map[string]any)["record"].(string)
	// Close first, so nothing else is written after the fragment below.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionClose, "params": map[string]any{"session": "s1"}})
	h.awaitResponse(2)

	before, err := engine.EventsAfter(t.Context(), dir, "s1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(rec, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":999,"ki`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	after, err := engine.EventsAfter(t.Context(), dir, "s1", 0, 0)
	if err != nil || len(after.Events) != len(before.Events) {
		t.Errorf("with a half-written line: %d events (%v), want the %d before it", len(after.Events), err, len(before.Events))
	}
	h.close()
}
