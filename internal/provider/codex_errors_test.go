package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These bodies are hand-written, not recorded: no real quota or expiry response
// has been captured yet (KOP-199 still wants one). They follow the shape Codex
// CLI's open-source client reads, and the headers are the ones the live
// recording showed. The tests pin what this client does with them, and that an
// unrecognised response degrades to the plain APIError.

func codexFailure(t *testing.T, status int, header http.Header, body string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c, err := NewCodexClient(fakeCreds(t), WithCodexBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Complete(context.Background(), Request{ModelID: "m"})
	if err == nil {
		t.Fatal("a refused request returned no error")
	}
	return err
}

func TestCodexQuotaSpentSaysWhenItResets(t *testing.T) {
	body := `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":7980}}`
	err := codexFailure(t, 429, nil, body)
	var ce *CodexError
	if !errors.As(err, &ce) || ce.Failure != CodexQuotaSpent || ce.Plan != "plus" || ce.ResetsIn != 7980*time.Second {
		t.Fatalf("error = %#v, want quota spent on plus resetting in 7980s", err)
	}
	for _, want := range []string{"usage limit is spent", "plus plan", "2h13m0s", body} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err.Error(), want)
		}
	}
	var api *APIError
	if !errors.As(err, &api) || api.Status != 429 || api.Body != body {
		t.Errorf("the APIError underneath was lost or its body clipped: %+v", api)
	}
}

func TestCodexQuotaResetFallsBackToTheLongerHeaderWindow(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-reset-after-seconds", "600")
	h.Set("x-codex-secondary-reset-after-seconds", "86400")
	err := codexFailure(t, 429, h, `{"error":{"type":"usage_limit_reached"}}`)
	var ce *CodexError
	if !errors.As(err, &ce) || ce.ResetsIn != 86400*time.Second {
		t.Fatalf("ResetsIn = %v, want the spent (longer) window, 24h", ce)
	}
}

func TestCodexPlainRateLimitUsesRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")
	err := codexFailure(t, 429, h, `{"error":{"type":"rate_limit_exceeded"}}`)
	var ce *CodexError
	if !errors.As(err, &ce) || ce.Failure != CodexRateLimited || ce.ResetsIn != 30*time.Second {
		t.Fatalf("error = %#v, want a rate limit retrying in 30s", err)
	}
}

func TestCodexUnauthorizedAndForbiddenSayWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   CodexFailure
		hint   string
	}{
		{401, CodexUnauthorized, "run Codex CLI once"},
		{403, CodexForbidden, "challenge page"},
	} {
		err := codexFailure(t, tc.status, nil, "<html>nope</html>")
		var ce *CodexError
		if !errors.As(err, &ce) || ce.Failure != tc.want || !strings.Contains(err.Error(), tc.hint) {
			t.Errorf("status %d: error = %v, want %s mentioning %q", tc.status, err, tc.want, tc.hint)
		}
		if !strings.Contains(err.Error(), "<html>nope</html>") {
			t.Errorf("status %d: the body was not kept whole: %v", tc.status, err)
		}
	}
}

func TestCodexErrorsNeverCarryTheCredential(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		err := codexFailure(t, status, nil, `{"error":{"type":"x"}}`)
		if s := err.Error(); strings.Contains(s, fakeToken) || strings.Contains(s, fakeAccount) {
			t.Errorf("status %d: the error carries a credential: %s", status, s)
		}
	}
}

func TestCodexOtherFailuresStayPlainAPIErrors(t *testing.T) {
	err := codexFailure(t, 400, nil, `{"detail":"Unsupported parameter: temperature"}`)
	var ce *CodexError
	if errors.As(err, &ce) {
		t.Fatalf("a 400 was classified: %v", ce)
	}
	var api *APIError
	if !errors.As(err, &api) || api.Status != 400 {
		t.Errorf("error = %v, want the plain APIError", err)
	}
}
