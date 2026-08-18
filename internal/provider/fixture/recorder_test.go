package fixture_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
)

// This is KAN-774's done-when: a recording of a live call contains no
// credential, proven by a test that looks for one.
//
// The technique mirrors internal/engine/keyleak_test.go: a low-entropy,
// self-describing fake credential goes out on a real request, and the test
// scans the *bytes actually written to disk* rather than trusting the
// recorder's own bookkeeping. A positive control (the model id, expected to
// survive) proves the scan finds real content and is not silently matching
// nothing; a second positive control writes a header the allowlist should
// keep and asserts it structurally, through the very loader the mock
// provider uses, so the written file is proven to be a *fixture* and not
// merely a JSON blob that happens to have no credential in it.

// recorderFakeKey is the credential sent on the wire, low-entropy and
// self-describing for the same reason internal/provider's and
// internal/engine's tests give theirs that shape.
const recorderFakeKey = "kopicode-fake-key-not-a-credential-0774"

// recorderExtraSecret stands in for a header the allowlist has never heard
// of — the case the card is actually worried about: not "did we remember to
// exclude Authorization" but "does a header nobody thought to name still get
// dropped." A denylist would need updating for this one; an allowlist never
// does.
const recorderExtraSecret = "kopicode-should-not-survive-either-0774"

const recorderModelID = "test/model"

// recorderSSELines is one terminal exchange: an assistant reply with no
// tool call, ending in "stop" — the shape [fixture.Validate]'s
// validateSequence requires of a fixture's last exchange.
var recorderSSELines = []string{
	`data: {"id":"gen-rec-0001","object":"chat.completion.chunk","created":1767225600,` +
		`"model":"test/model","provider":"Parasail","choices":[{"index":0,"delta":` +
		`{"role":"assistant","content":"hi"},"finish_reason":null,"native_finish_reason":null}]}`,
	"",
	`data: {"id":"gen-rec-0001","object":"chat.completion.chunk","created":1767225600,` +
		`"model":"test/model","provider":"Parasail","choices":[{"index":0,"delta":` +
		`{"content":null},"finish_reason":"stop","native_finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
	"",
	"data: [DONE]",
	"",
}

// recorderRequestBody is the outgoing request's JSON: only the two fields
// [fixture.Recorder] reads back out of it, model and provider, the way
// internal/provider/client.go's wireRequest sends them.
const recorderRequestBody = `{"model":"test/model","provider":{"order":["parasail/bf16"],` +
	`"allow_fallbacks":false,"quantizations":["bf16"]}}`

// recorderSSEServer serves recorderSSELines verbatim, the same
// line-plus-newline framing internal/provider/client_test.go's writeSSE
// uses, so a request recorded here is a faithful stand-in for one the live
// client would have made.
func recorderSSEServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, line := range recorderSSELines {
			if _, err := io.WriteString(w, line+"\n"); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recordOneExchange drives one real HTTP request — headers, body and all —
// through rec and returns once the response has been read to completion,
// which is what triggers [fixture.Recorder] to finalise the exchange.
func recordOneExchange(t *testing.T, rec *fixture.Recorder, srv *httptest.Server) {
	t.Helper()
	ctx := fixture.WithMeta(context.Background(), 1, 1, "the KAN-774 credential-scrub proof")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader([]byte(recorderRequestBody)))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	// Exactly what internal/provider/client.go's newHTTPRequest sets, plus one
	// header on no allowlist anywhere, to prove the mechanism is "nothing
	// survives unless named" rather than "we happen to have excluded this one
	// name."
	req.Header.Set("Authorization", "Bearer "+recorderFakeKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Kopicode-Test-Secret", recorderExtraSecret)

	client := &http.Client{Transport: rec}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("reading the response body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("closing the response body: %v", err)
	}
}

func TestRecorderScrubsTheCredentialAtWriteTime(t *testing.T) {
	srv := recorderSSEServer(t)
	rec := fixture.NewRecorder(nil)
	recordOneExchange(t, rec, srv)

	f, err := rec.Fixture("kan774_recorder_proof", "proves the recorder scrubs headers before a fixture is ever written")
	if err != nil {
		t.Fatalf("assembling the recorded fixture: %v", err)
	}
	if f.Origin != fixture.OriginRecorded {
		t.Errorf("origin is %q, want %q", f.Origin, fixture.OriginRecorded)
	}

	dir := t.TempDir()
	path, err := fixture.Write(dir, f)
	if err != nil {
		t.Fatalf("writing the recorded fixture: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back %s: %v", path, err)
	}
	body := string(raw)

	// The card's own done-when.
	if strings.Contains(body, recorderFakeKey) {
		t.Errorf("the credential reached the recorded fixture at %s:\n%s", path, body)
	}
	// The header nobody put on any allowlist has to be gone too, or this is a
	// denylist wearing an allowlist's name.
	if strings.Contains(body, recorderExtraSecret) {
		t.Errorf("an unlisted header's value reached the recorded fixture at %s — the allowlist "+
			"let through something it was never told to keep", path)
	}
	if strings.Contains(strings.ToLower(body), "authorization") {
		t.Errorf("the literal word %q appears in the recorded fixture at %s", "authorization", path)
	}

	// The positive control: if the model id — real content that belongs in
	// the file — were not found either, the negative results above would be
	// proving that the scan matches nothing, not that the file is clean.
	if !strings.Contains(body, recorderModelID) {
		t.Fatalf("the model id %q never reached %s either; the credential checks above prove nothing", recorderModelID, path)
	}

	// Load it back through the same loader the mock provider uses. A
	// recorder that writes bytes gitleaks would pass is not the card: the
	// output has to be the fixture format, not a look-alike.
	loaded, err := fixture.Load(os.DirFS(dir), "kan774_recorder_proof")
	if err != nil {
		t.Fatalf("loading the recorded fixture back through fixture.Load: %v", err)
	}
	if len(loaded.Exchanges) != 1 {
		t.Fatalf("loaded fixture carries %d exchange(s), want 1", len(loaded.Exchanges))
	}

	// The second positive control: Content-Type and Accept are on the
	// allowlist and must survive, structurally, not just as a substring
	// somewhere in the file.
	want := map[string]string{"content-type": "application/json", "accept": "text/event-stream"}
	if diff := cmp.Diff(want, loaded.Exchanges[0].RequestHeaders); diff != "" {
		t.Errorf("recorded request headers (-want +got):\n%s", diff)
	}
	if _, ok := loaded.Exchanges[0].RequestHeaders["authorization"]; ok {
		t.Errorf("the loaded fixture's request headers still name authorization")
	}
}

// TestAPlantedHeaderIsRejectedByValidate is the control for the control on
// the request side: [fixture.Validate] already refuses a response header
// outside its allowlist (validate_test.go covers that), and this is the
// request-side twin — proof that a header the recorder should never write
// would, if it somehow got there, fail to load rather than fail silently.
func TestAPlantedHeaderIsRejectedByValidate(t *testing.T) {
	srv := recorderSSEServer(t)
	rec := fixture.NewRecorder(nil)
	recordOneExchange(t, rec, srv)

	f, err := rec.Fixture("kan774_planted_header", "the request-header allowlist, proven red")
	if err != nil {
		t.Fatalf("assembling the recorded fixture: %v", err)
	}
	if len(f.Exchanges) != 1 {
		t.Fatalf("recorded %d exchange(s), want 1", len(f.Exchanges))
	}
	// A header the recorder would never write, planted directly to prove the
	// validator — not merely the recorder — refuses it too.
	f.Exchanges[0].RequestHeaders["authorization"] = "Bearer " + recorderFakeKey

	if err := fixture.Validate(f); err == nil {
		t.Fatal("Validate accepted a fixture whose request headers name authorization")
	} else if !strings.Contains(err.Error(), "authorization") {
		t.Errorf("Validate's error does not name the offending header: %v", err)
	}
}

// TestRecorderIgnoresATransportFailure is the other half of "an error here is
// a request that never produced a reply": nothing should be recorded, and
// Fixture should not fail either, because zero exchanges is a fixture in
// progress rather than a broken one.
func TestRecorderIgnoresATransportFailure(t *testing.T) {
	rec := fixture.NewRecorder(http.DefaultTransport)
	client := &http.Client{Transport: rec}

	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:1/unreachable", strings.NewReader(recorderRequestBody))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := client.Do(req) //nolint:bodyclose // a transport-failure Do returns a nil response; there is nothing to close.
	if err == nil {
		resp.Body.Close() //nolint:errcheck // the fatal below stops the test either way.
		t.Fatal("expected the request to fail to connect")
	}

	f, err := rec.Fixture("kan774_no_exchanges", "a session where every request failed before a reply arrived")
	if err != nil {
		t.Fatalf("Fixture: %v", err)
	}
	if len(f.Exchanges) != 0 {
		t.Errorf("recorded %d exchange(s) from a request that never got a response", len(f.Exchanges))
	}
}
