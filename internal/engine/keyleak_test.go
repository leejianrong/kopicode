package engine_test

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
)

// This is the other half of KAN-776's done-when: the API key appears nowhere in
// the journal and nowhere in the blobs.
//
// It lives here rather than in internal/provider because internal/provider must
// not import the journal — the engine journals, packages return data — and
// internal/arch enforces that for the product code. The engine is the side that
// writes the record, so the claim belongs on this side of the seam, standing in
// for the loop that KAN-789 will build.
//
// The care that matters is in what is switched off. FileJournal redacts the
// value of OPENROUTER_API_KEY out of every line at append time, so a leak test
// run with redaction on passes whatever the client does — the journal would
// scrub a credential the client handed it, and the test would report that as the
// client being clean. So the client's own hygiene is asserted with redaction
// **off**, and the redactor is asserted separately, on a provider that echoes
// the credential back.

// leakKey is the credential these tests send. Low entropy and self-describing,
// for the reason internal/provider's tests give.
const leakKey = "kopicode-fake-key-not-a-credential-0002"

// scan reports every file under dir whose bytes contain needle.
//
// It walks rather than reading known filenames: the journal's layout is
// .kopicode/sessions/<id>/events.jsonl plus .kopicode/blobs/<sha>, and a test
// that named those two would stop covering whatever the third file turns out to
// be.
func scan(t *testing.T, dir, needle string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), needle) {
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				rel = path
			}
			hits = append(hits, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return hits
}

// countFiles reports how many files the walk can see, so an empty tree cannot
// masquerade as a clean one.
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	var n int
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return n
}

// openJournal opens a session journal under a fresh root.
//
// The blob threshold is zero, so every unbounded field spills and the blob store
// is exercised on every event rather than only on a large one. The clock is
// fixed because a journal that is not reproducible is not comparable.
func openJournal(t *testing.T, opts ...journal.Option) (*journal.FileJournal, string) {
	t.Helper()
	root := t.TempDir()
	base := []journal.Option{
		journal.WithBlobThreshold(0),
		journal.WithClock(func() time.Time { return time.Unix(1767225600, 0).UTC() }),
	}
	j, err := journal.Open(root, "leak-test", append(base, opts...)...)
	if err != nil {
		t.Fatalf("opening journal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j, root
}

// record writes the two events a loop writes around one provider call, from the
// values the client returned and nothing else.
//
// This is the shape KAN-789 will use: the request's pin and sampling on the way
// out, the wire transcript and the reported usage on the way back. Whatever the
// client hands over lands here, which is exactly why "what does the client hand
// over" is a question about credentials.
func record(t *testing.T, j journal.Journal, req provider.Request, stream *provider.Stream) {
	t.Helper()
	ctx := context.Background()

	if _, err := j.Append(ctx, req.Turn, journal.ProviderRequest{
		ModelID: req.ModelID,
		Provider: journal.ProviderPin{
			Order:          req.Pin.Order,
			AllowFallbacks: req.Pin.AllowFallbacks,
			Quantizations:  req.Pin.Quantizations,
		},
		Sampling: journal.Sampling{
			Temperature: req.Sampling.Temperature,
			TopP:        req.Sampling.TopP,
			MaxTokens:   req.Sampling.MaxTokens,
			Seed:        req.Sampling.Seed,
		},
		Attempt: req.Attempt,
	}); err != nil {
		t.Fatalf("appending the request: %v", err)
	}

	for stream.Next() {
	}
	reply, err := stream.Reply()
	if err != nil {
		t.Fatalf("draining the reply: %v", err)
	}
	if _, err := j.Append(ctx, req.Turn, journal.ProviderResponse{
		// A streamed reply has no assembled body; the transcript is the record
		// of what arrived.
		Body: journal.InlineText(string(stream.Transcript())),
		Tokens: journal.TokenCounts{
			Prompt:     reply.Usage.Prompt,
			Completion: reply.Usage.Completion,
			Total:      reply.Usage.Total,
		},
		FinishReason: reply.FinishReason,
		ServedBy:     reply.ServedBy,
	}); err != nil {
		t.Fatalf("appending the response: %v", err)
	}
}

func loadFixture(t *testing.T) fixture.Fixture {
	t.Helper()
	f, err := fixture.Load(fixture.FS(), "two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	return f
}

func pinnedRequest(f fixture.Fixture) provider.Request {
	return provider.Request{
		ModelID: f.ModelID,
		Pin: provider.Pin{
			Order:          f.Pin.Order,
			AllowFallbacks: f.Pin.AllowFallbacks,
			Quantizations:  f.Pin.Quantizations,
		},
		Sampling: provider.Sampling{Temperature: 0.2, TopP: 0.95, MaxTokens: 1024},
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "add a test"}},
		Turn:     1,
		Attempt:  1,
	}
}

// sseServer serves the fixture's frames and reflects the request's headers into
// its own, so a client that kept them would be caught.
func sseServer(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		for _, line := range lines {
			io.WriteString(w, line+"\n") //nolint:errcheck // a short write fails the assertions.
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func clientFor(t *testing.T, srv *httptest.Server) *provider.Client {
	t.Helper()
	c, err := provider.NewClient(provider.NewAPIKey(leakKey),
		provider.WithBaseURL(srv.URL),
		provider.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return c
}

// TestTheCredentialReachesNeitherTheJournalNorABlob is the card's done-when,
// with redaction switched off so that what is being asserted is the client's
// hygiene rather than the journal's mop.
func TestTheCredentialReachesNeitherTheJournalNorABlob(t *testing.T) {
	f := loadFixture(t)
	srv := sseServer(t, f.Exchanges[0].Response.Stream)

	// No secret names: nothing is redacted on the way to disk, so anything the
	// client handed over lands verbatim.
	j, root := openJournal(t, journal.WithSecretEnv())

	stream, err := clientFor(t, srv).Complete(t.Context(), pinnedRequest(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // the record helper drains and checks the stream.
	record(t, j, pinnedRequest(f), stream)

	if n := countFiles(t, root); n < 2 {
		t.Fatalf("the session tree holds %d file(s); a walk that finds nothing reports every "+
			"secret as absent", n)
	}
	if hits := scan(t, root, leakKey); len(hits) > 0 {
		t.Errorf("the credential reached %v under %s", hits, root)
	}

	// The positive control, in the same tree, through the same walk: a value
	// that *is* written must be found. Without it, a scan that silently matched
	// nothing — a mistyped root, a walk that skipped a directory — would report
	// the same clean result.
	if hits := scan(t, root, f.ModelID); len(hits) == 0 {
		t.Errorf("the walk did not find the model id %q anywhere under %s, so it would not have "+
			"found the credential either", f.ModelID, root)
	}
	// And one of those hits has to be a blob. The response body is a Text and
	// the threshold is zero, so it spilled; a scan that could not see into
	// .kopicode/blobs would clear the journal of a credential sitting in one.
	assertBlobWasScanned(t, root, "chat.completion.chunk")
}

// assertBlobWasScanned fails unless needle was found inside the blob store.
//
// It is the control for the specific blindness this scan invites: the record is
// two places, not one, and the oversized half — the half a provider body lands
// in — is the half that is easy to walk straight past.
func assertBlobWasScanned(t *testing.T, root, needle string) {
	t.Helper()
	for _, hit := range scan(t, root, needle) {
		if strings.Contains(hit, journal.BlobsSubdir) {
			return
		}
	}
	t.Errorf("no blob under %s carried %q, so the scan above says nothing about what is in the "+
		"blob store", filepath.Join(root, journal.StateDir, journal.BlobsSubdir), needle)
}

// TestAPlantedCredentialIsFound is the control for the control.
//
// Same journal settings, same walk, but the credential is deliberately appended
// — as it would be if a model ran `env` in a shell and the output landed in a
// tool result. It must be found. If it is not, the assertion above is vacuous.
func TestAPlantedCredentialIsFound(t *testing.T) {
	j, root := openJournal(t, journal.WithSecretEnv())

	if _, err := j.Append(context.Background(), 1, journal.ToolResult{
		CallID: "call-1",
		Tool:   "run_shell",
		Output: journal.InlineText("OPENROUTER_API_KEY=" + leakKey),
	}); err != nil {
		t.Fatalf("appending: %v", err)
	}

	if hits := scan(t, root, leakKey); len(hits) == 0 {
		t.Fatalf("a credential written straight into the record was not found under %s; "+
			"the leak scan is asserting nothing", root)
	}
	assertBlobWasScanned(t, root, leakKey)
}

// TestTheJournalStillScrubsAProviderThatEchoesTheKey is the defence in depth,
// asserted separately because it is a different claim.
//
// The client is clean by construction; a provider that reflects the request's
// Authorization header into its reply is not, and that content is a legitimate
// part of the record. FileJournal's append-time redaction is what stops it
// reaching disk, and it reads the value from the environment — so this is the
// one test here that sets OPENROUTER_API_KEY.
func TestTheJournalStillScrubsAProviderThatEchoesTheKey(t *testing.T) {
	t.Setenv(provider.KeyEnv, leakKey)

	f := loadFixture(t)
	echoed := fmt.Sprintf(`data: {"id":"gen-1","object":"chat.completion.chunk","created":1767225600,`+
		`"model":%q,"provider":"Parasail","choices":[{"index":0,"delta":{"content":"your key is %s"},`+
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		f.ModelID, leakKey)
	srv := sseServer(t, []string{echoed, "", "data: [DONE]"})

	// Default options: DefaultSecretEnv names OPENROUTER_API_KEY.
	j, root := openJournal(t)

	stream, err := clientFor(t, srv).Complete(t.Context(), pinnedRequest(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // the record helper drains and checks the stream.
	record(t, j, pinnedRequest(f), stream)

	// The control: the provider really did echo it, so the absence below is the
	// redactor working rather than the server being polite.
	if hits := scan(t, root, "your key is"); len(hits) == 0 {
		t.Fatalf("the echoed reply is not in the record at all, so the redaction check below " +
			"is checking nothing")
	}
	if hits := scan(t, root, leakKey); len(hits) > 0 {
		t.Errorf("the echoed credential survived into %v", hits)
	}
}
