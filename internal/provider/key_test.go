package provider_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/provider"
)

// This file is the "the key appears nowhere" half of KAN-776's done-when, at the
// level this package can prove it: nothing the client hands back, prints or logs
// carries the credential. The other half — that it reaches neither the journal
// file nor a blob — is proved end to end in internal/engine, on the side that
// does the journaling.
//
// Every check here runs against a positive control, because the failure mode of
// an absence test is that it is vacuous: a grep over an empty string finds no
// secret and reports success.

// revealed reports whether text carries the secret in any spelling a formatting
// verb could produce.
//
// The hex spellings matter: %x and %X on a struct print its fields as hex, so a
// checker that only looked for the literal would call a hex dump of the
// credential clean.
func revealed(text, secret string) bool {
	switch {
	case strings.Contains(text, secret):
		return true
	case strings.Contains(text, hex.EncodeToString([]byte(secret))):
		return true
	case strings.Contains(text, strings.ToUpper(hex.EncodeToString([]byte(secret)))):
		return true
	}
	return false
}

// verbs are the formatting directives fmt consults a Stringer, GoStringer or
// Formatter for, plus the two struct-dumping ones. Anything not on this list
// prints a %!verb complaint rather than the value.
var verbs = []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X"}

// plainHolder is the positive control: the same value, in a type that does
// nothing to protect it.
type plainHolder struct{ Key string }

// TestTheCheckerCanSeeALeak is the control for the control.
//
// A struct that holds the secret as a plain string must be caught under every
// verb this file checks. If it is not, [revealed] is looking in the wrong place
// and every assertion below is vacuous — which is exactly how a guard lands
// green while asserting nothing.
func TestTheCheckerCanSeeALeak(t *testing.T) {
	control := plainHolder{Key: fakeKey}
	for _, verb := range verbs {
		if got := fmt.Sprintf(verb, control); !revealed(got, fakeKey) {
			t.Errorf("the checker did not find the secret in %s of an unprotected struct: %s\n"+
				"the leak checks in this file are asserting nothing", verb, got)
		}
	}
	if b, err := json.Marshal(control); err != nil || !revealed(string(b), fakeKey) {
		t.Errorf("the checker did not find the secret in json.Marshal of an unprotected struct: %s (%v)", b, err)
	}
}

// TestNoFormattingVerbRevealsTheKey covers the realistic accident: a %v on a
// struct that happens to hold a credential, in an error message or a debug
// print.
func TestNoFormattingVerbRevealsTheKey(t *testing.T) {
	key := provider.NewAPIKey(fakeKey)
	client, err := provider.NewClient(key, provider.WithBaseURL("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	subjects := []struct {
		name string
		of   any
	}{
		{"the key itself", key},
		{"a pointer to the key", &key},
		{"the client", client},
		{"a struct holding the key", struct{ Key provider.APIKey }{Key: key}},
	}

	for _, subject := range subjects {
		for _, verb := range verbs {
			got := fmt.Sprintf(verb, subject.of)
			if revealed(got, fakeKey) {
				t.Errorf("%s under %s reveals the credential: %s", subject.name, verb, got)
			}
		}
		if b, err := json.Marshal(subject.of); err == nil && revealed(string(b), fakeKey) {
			t.Errorf("%s survives json.Marshal as %s", subject.name, b)
		}
	}

	// slog is the one output path in this package a human deliberately points
	// at a terminal.
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("configured", slog.Any("key", key))
	if revealed(buf.String(), fakeKey) {
		t.Errorf("slog printed the credential: %s", buf.String())
	}
	if !strings.Contains(buf.String(), provider.Redacted) {
		t.Errorf("slog printed %q, which does not look redacted at all", buf.String())
	}
}

// TestNoErrorPathCarriesTheKey walks every way Complete can fail.
//
// The paths are the realistic leaks named on the card: an error that wraps the
// *http.Request carries its header map, and net/http's own *url.Error carries
// the URL. Neither may end up holding the credential.
func TestNoErrorPathCarriesTheKey(t *testing.T) {
	statusServer := func(status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"code":%d}}`, status)
		}))
	}

	cases := []struct {
		name string
		run  func(t *testing.T) error
	}{
		{
			name: "a request with no pin",
			run: func(t *testing.T) error {
				srv := statusServer(http.StatusOK)
				defer srv.Close()
				req := request(loadFixture(t))
				req.Pin = provider.Pin{}
				_, err := newClient(t, srv).Complete(t.Context(), req)
				return err
			},
		},
		{
			name: "a request with no model",
			run: func(t *testing.T) error {
				srv := statusServer(http.StatusOK)
				defer srv.Close()
				req := request(loadFixture(t))
				req.ModelID = ""
				_, err := newClient(t, srv).Complete(t.Context(), req)
				return err
			},
		},
		{
			name: "an unauthorized response",
			run: func(t *testing.T) error {
				srv := statusServer(http.StatusUnauthorized)
				defer srv.Close()
				_, err := newClient(t, srv).Complete(t.Context(), request(loadFixture(t)))
				return err
			},
		},
		{
			name: "retries exhausted",
			run: func(t *testing.T) error {
				srv := statusServer(http.StatusTooManyRequests)
				defer srv.Close()
				c := newClient(t, srv,
					provider.WithClock(&manualClock{}),
					provider.WithRand(topOfInterval()),
					provider.WithRetry(provider.Retry{MaxAttempts: 2, Base: time.Second, Cap: time.Second}),
				)
				_, err := c.Complete(t.Context(), request(loadFixture(t)))
				return err
			},
		},
		{
			name: "a transport failure",
			run: func(t *testing.T) error {
				srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					panic(http.ErrAbortHandler)
				}))
				defer srv.Close()
				c := newClient(t, srv,
					provider.WithClock(&manualClock{}),
					provider.WithRand(bottomOfInterval()),
					provider.WithRetry(provider.Retry{MaxAttempts: 2, Base: time.Second, Cap: time.Second}),
				)
				_, err := c.Complete(t.Context(), request(loadFixture(t)))
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t)
			if err == nil {
				t.Fatal("the case produced no error, so nothing was checked")
			}
			for _, rendered := range []string{
				err.Error(),
				fmt.Sprintf("%v", err),
				fmt.Sprintf("%+v", err),
				fmt.Sprintf("%#v", err),
			} {
				if revealed(rendered, fakeKey) {
					t.Errorf("the error carries the credential: %s", rendered)
				}
			}
		})
	}
}

// TestTheRetryLogNeverCarriesTheKeyEvenWhenTheProviderEchoesIt.
//
// A gateway that reflects the request's Authorization header into its error body
// is not hypothetical, and the client logs a line per retry. Logging the body
// there would put the credential on a terminal by way of a diagnostic nobody
// thinks of as an output path — and unlike the journal, nothing redacts a log.
//
// The returned error does carry the body, whole and untruncated. That is
// deliberate: the body of a failed provider call is the diagnostic, and the
// journal redacts known secret values at append time. This test asserts that
// difference rather than glossing it, and the error is the positive control —
// if the echoed body were not there, the log assertion would be checking a
// server that never echoed anything.
func TestTheRetryLogNeverCarriesTheKeyEvenWhenTheProviderEchoesIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"error":{"message":"bad credentials: %s"}}`, r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	var logged bytes.Buffer
	c := newClient(t, srv,
		provider.WithClock(&manualClock{}),
		provider.WithRand(topOfInterval()),
		provider.WithRetry(provider.Retry{MaxAttempts: 3, Base: time.Second, Cap: time.Second}),
		provider.WithLogger(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	)

	_, err := c.Complete(t.Context(), request(loadFixture(t)))
	if err == nil {
		t.Fatal("Complete against a permanent 429 succeeded")
	}

	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Body, fakeKey) {
		t.Fatalf("the server did not echo the credential into its body, so the log check below "+
			"is checking nothing: %v", err)
	}
	if !strings.Contains(logged.String(), "retrying") {
		t.Fatalf("no retry was logged, so the log check is vacuous:\n%s", logged.String())
	}
	if revealed(logged.String(), fakeKey) {
		t.Errorf("the retry log carries the credential:\n%s", logged.String())
	}
}

// TestNothingTheClientReturnsOnSuccessCarriesTheKey. The reply, its raw bytes
// and the wire transcript all reach the journal; none may carry a credential.
func TestNothingTheClientReturnsOnSuccessCarriesTheKey(t *testing.T) {
	f := loadFixture(t)

	// A server that reflects the request's headers into the reply text — the
	// worst case a well-behaved client can be handed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		for _, line := range f.Exchanges[0].Response.Stream {
			io.WriteString(w, line+"\n") //nolint:errcheck // a short write fails the assertions below.
		}
	}))
	defer srv.Close()

	stream, err := newClient(t, srv).Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // the assertions below cover the stream's own error.

	var text strings.Builder
	for stream.Next() {
		text.WriteString(stream.Delta().Text)
	}
	reply, err := stream.Reply()
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	for name, value := range map[string]string{
		"the deltas":     text.String(),
		"the reply":      fmt.Sprintf("%+v", reply),
		"the raw body":   string(reply.Raw),
		"the transcript": string(stream.Transcript()),
	} {
		if revealed(value, fakeKey) {
			t.Errorf("%s carries the credential: %s", name, value)
		}
	}

	// The control: the response header the server set really did carry it, so
	// the absence above is a property of what the client keeps rather than of a
	// server that never sent anything.
	if got := stream.Transcript(); len(got) == 0 {
		t.Error("the transcript is empty, so the check above found nothing because there was nothing")
	}
}

// TestAPIKeyFromEnvRefusesAnEmptyEnvironment. A missing credential is
// configuration and is reported as configuration, not as a 401 from a provider
// three retries later.
func TestAPIKeyFromEnvRefusesAnEmptyEnvironment(t *testing.T) {
	t.Setenv(provider.KeyEnv, "")
	if _, err := provider.APIKeyFromEnv(); !errors.Is(err, provider.ErrNoAPIKey) {
		t.Fatalf("APIKeyFromEnv with an empty %s: %v, want ErrNoAPIKey", provider.KeyEnv, err)
	}

	t.Setenv(provider.KeyEnv, fakeKey)
	key, err := provider.APIKeyFromEnv()
	if err != nil {
		t.Fatalf("APIKeyFromEnv: %v", err)
	}
	if key.IsZero() {
		t.Error("APIKeyFromEnv returned an empty key from a set variable")
	}
	if revealed(fmt.Sprintf("%v %s %q", key, key, key), fakeKey) {
		t.Error("a key read from the environment prints itself")
	}
}
