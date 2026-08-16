//go:build integration

package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// TestRetriesExhaustedExitsThree is the other route to exit 3, and the one the
// card names: a provider failure that *survives* the client's own retries.
//
// It is tagged out of the fast suite because it waits the real backoff out.
// provider.DefaultRetry is six sends with ceilings of 1s, 2s, 4s, 8s and 8s
// under full jitter, so this costs about eleven seconds on average and
// twenty-three at worst. The clock and the RNG are injected in
// internal/provider and asserted exactly there; what cannot be asserted there is
// that the exhausted error reaches the *front end* as exit 3 rather than as a
// harness error, which is what this runs for.
//
// A 503 is used rather than a 429 so the case is "the provider is broken"
// rather than "the provider is throttling us"; both are retried, and neither is
// a fact about the request.
func TestRetriesExhaustedExitsThree(t *testing.T) {
	srv := refusingProvider(t, http.StatusServiceUnavailable, "upstream is down")
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")

	stdout, stderr, code, _ := runHeadless(t, "fix it", engine.Options{ProviderBaseURL: srv.URL})

	if code != exitProvider {
		t.Fatalf("exit code = %d, want %d. stderr:\n%s\nstdout:\n%s",
			code, exitProvider, stderr, stdout)
	}
	if !strings.Contains(stderr, "attempt") {
		t.Errorf("stderr does not say the call was retried:\n%s", stderr)
	}

	end := last(t, stream(t, stdout))
	if got := number(t, end, "exit_code"); got != exitProvider {
		t.Errorf("the record says exit %d, want %d", got, exitProvider)
	}
}
