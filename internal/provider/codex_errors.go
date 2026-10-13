package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CodexFailure names why the Codex backend refused a request, as far as the
// status and body say (KOP-199).
type CodexFailure string

const (
	// CodexUnauthorized is a 401: the login's access token was not accepted.
	CodexUnauthorized CodexFailure = "unauthorized"
	// CodexForbidden is a 403: the account, plan or model is not allowed, or a
	// challenge page answered instead of the API.
	CodexForbidden CodexFailure = "forbidden"
	// CodexQuotaSpent is a 429 that names the plan's usage limit.
	CodexQuotaSpent CodexFailure = "quota_spent"
	// CodexRateLimited is any other 429: slow down, not spent.
	CodexRateLimited CodexFailure = "rate_limited"
)

// CodexError is a refused Codex request with what to do about it. It wraps the
// [APIError], which still carries the status and the whole body, so a caller
// that only knows APIError (errors.As) sees what it always did.
//
// The shapes behind it are partly inferred. The 401, 403 and 429 statuses are
// the HTTP conventions; the 429 body's "usage_limit_reached" type and its
// resets_* fields follow Codex CLI's open-source client; the reset time also
// comes from the x-codex-*-reset-after-seconds headers, which were observed live
// (docs/research/2026-10-codex-backend-verification.md). No real quota or expiry
// response has been recorded yet (KOP-199 still wants one), so the tests use
// hand-written bodies and say so. Anything not recognised keeps the plain
// [APIError] and its body.
//
// Nothing here ever holds the access token: the request is not retained, and
// the message is built from the status, the response's own fields and fixed
// text.
type CodexError struct {
	API     *APIError
	Failure CodexFailure
	// ResetsIn is when the limit lifts, when the response said; zero when not.
	ResetsIn time.Duration
	// Plan is the plan the response named (plus, pro, ...), or "".
	Plan string
}

func (e *CodexError) Unwrap() error { return e.API }

func (e *CodexError) Error() string {
	var msg string
	switch e.Failure {
	case CodexUnauthorized:
		msg = "the Codex login was not accepted; run Codex CLI once to sign in again, then retry"
	case CodexForbidden:
		msg = "the Codex backend refused this account or model (or a challenge page answered); " +
			"check the model is one your plan serves, and that Codex CLI itself still works"
	case CodexQuotaSpent:
		msg = "the Codex usage limit is spent"
		if e.Plan != "" {
			msg += " for the " + e.Plan + " plan"
		}
		if e.ResetsIn > 0 {
			msg += "; it resets in " + e.ResetsIn.Round(time.Minute).String()
		}
		msg += "; wait for the reset, or run without --provider codex"
	case CodexRateLimited:
		msg = "the Codex backend is rate limiting this login"
		if e.ResetsIn > 0 {
			msg += "; retry in " + e.ResetsIn.Round(time.Second).String()
		}
	}
	return fmt.Sprintf("provider: %s (http %d): %s", msg, e.API.Status, e.API.Body)
}

// codexErrorBody is the part of a refused request's JSON this reads.
type codexErrorBody struct {
	Error struct {
		Type            string  `json:"type"`
		Code            string  `json:"code"`
		PlanType        string  `json:"plan_type"`
		ResetsInSeconds float64 `json:"resets_in_seconds"`
	} `json:"error"`
}

// classifyCodexError turns a failed response into a [*CodexError] when its
// status is one this client knows how to explain, and returns err untouched
// otherwise. header is the failed response's header, read for the reset times.
func classifyCodexError(err error, header http.Header) error {
	api, ok := err.(*APIError) //nolint:errorlint // drain returns the concrete type directly.
	if !ok {
		return err
	}
	ce := &CodexError{API: api}
	switch api.Status {
	case http.StatusUnauthorized:
		ce.Failure = CodexUnauthorized
	case http.StatusForbidden:
		ce.Failure = CodexForbidden
	case http.StatusTooManyRequests:
		ce.Failure = CodexRateLimited
		var b codexErrorBody
		if json.Unmarshal([]byte(api.Body), &b) == nil {
			ce.Plan = b.Error.PlanType
			if strings.Contains(b.Error.Type, "usage_limit") || strings.Contains(b.Error.Code, "usage_limit") {
				ce.Failure = CodexQuotaSpent
			}
			if b.Error.ResetsInSeconds > 0 {
				ce.ResetsIn = time.Duration(b.Error.ResetsInSeconds * float64(time.Second))
			}
		}
		if ce.ResetsIn == 0 {
			ce.ResetsIn = codexResetFromHeaders(header, ce.Failure == CodexQuotaSpent)
		}
	default:
		return err
	}
	return ce
}

// codexResetFromHeaders reads the reset times the backend sends on every
// response. A spent quota lifts when the window that is spent resets, which the
// response does not name, so the longer of the two is reported only when the
// failure is a spent quota and the shorter otherwise (a rate limit is the short
// window). Retry-After, when present, wins for a plain rate limit.
func codexResetFromHeaders(h http.Header, spent bool) time.Duration {
	if !spent {
		if v, err := strconv.ParseFloat(h.Get("Retry-After"), 64); err == nil && v > 0 {
			return time.Duration(v * float64(time.Second))
		}
	}
	var secs []float64
	for _, name := range []string{"x-codex-primary-reset-after-seconds", "x-codex-secondary-reset-after-seconds"} {
		if v, err := strconv.ParseFloat(h.Get(name), 64); err == nil && v > 0 {
			secs = append(secs, v)
		}
	}
	if len(secs) == 0 {
		return 0
	}
	pick := secs[0]
	for _, s := range secs[1:] {
		if (spent && s > pick) || (!spent && s < pick) {
			pick = s
		}
	}
	return time.Duration(pick * float64(time.Second))
}
