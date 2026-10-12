// Package codexauth reads the login Codex CLI already made, so a ChatGPT
// subscription can power kopicode's model calls (ADR-0035, KOP-193).
//
// # What it does, and what it does not
//
// It reads one file, `auth.json` under CODEX_HOME (default ~/.codex), read-only.
// It does not refresh a token, write the file, run a login or contact any
// server: an expired token is an error that says to run Codex CLI once. Own
// login and refresh are KOP-195 and need OpenAI's endpoints confirmed first.
//
// # The credential never leaves by accident
//
// The access token and the account id are [Secret]s. Every way the standard
// library turns a value into text (fmt, encoding/json, log/slog) renders a
// Secret as [Redacted], the same device as provider.APIKey, so a Credentials
// value handed to a log line, an error or a journal payload cannot carry them.
// [Secret.Reveal] is the one way out and its only legitimate caller builds an
// Authorization header. No error this package returns contains any part of the
// file: a malformed file is reported by path and by what is wrong, never by
// quoting it.
//
// Only what a request needs is kept. The refresh token and the id token are
// parsed past and dropped, so they are never held longer than the read.
package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HomeEnv names the directory Codex CLI keeps its state in.
const HomeEnv = "CODEX_HOME"

// FileName is the credential file inside [HomeEnv]'s directory.
const FileName = "auth.json"

// AuthModeChatGPT is the `auth_mode` of a ChatGPT sign-in, the only mode this
// package serves. Codex also records an API-key mode; that key is not a
// subscription and is not read.
const AuthModeChatGPT = "chatgpt"

// Redacted is what a [Secret] renders as, everywhere.
const Redacted = "[redacted]"

// Errors. None carries file content.
var (
	// ErrNotFound: there is no auth.json, so Codex CLI has not been signed in.
	ErrNotFound = errors.New("codexauth: no Codex login found")
	// ErrInsecure: the file is readable by someone other than its owner, or is
	// not owned by the current user. The path is named; the content never is.
	ErrInsecure = errors.New("codexauth: the credential file is not private to you")
	// ErrMalformed: the file is not the shape Codex writes.
	ErrMalformed = errors.New("codexauth: the credential file is not in the expected format")
	// ErrNotChatGPT: the file holds an API key or another mode, not a ChatGPT sign-in.
	ErrNotChatGPT = errors.New("codexauth: the Codex login is not a ChatGPT sign-in")
	// ErrExpired: the access token's own expiry has passed.
	ErrExpired = errors.New("codexauth: the Codex access token has expired; run Codex CLI once to refresh it")
)

// Secret is a credential value that cannot be printed by accident.
type Secret struct{ v string }

// String renders [Redacted] for fmt's %v, %s, %q, %x and %X.
func (s Secret) String() string { return Redacted }

// GoString renders [Redacted] under %#v, which would otherwise reach the field.
func (s Secret) GoString() string { return `codexauth.Secret("` + Redacted + `")` }

// MarshalText renders [Redacted] for encoding/json and every TextMarshaler consumer.
func (s Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// MarshalJSON renders [Redacted], independent of which interface an encoder prefers.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// LogValue renders [Redacted] to log/slog.
func (s Secret) LogValue() slog.Value { return slog.StringValue(Redacted) }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.v == "" }

// Reveal returns the credential. The only correct use is building the
// Authorization (or account) header of a request to the backend; a caller that
// wants the bytes for anything the journal will see wants the wrong thing.
func (s Secret) Reveal() string { return s.v }

// Credentials is what a request to the subscription backend needs.
type Credentials struct {
	// Path is the file the credentials were read from.
	Path string
	// AccessToken is the bearer token.
	AccessToken Secret
	// AccountID is the ChatGPT account id the backend may require alongside it.
	AccountID Secret
	// Expires is the access token's own expiry, zero when it states none.
	Expires time.Time
	// LastRefresh is when Codex last refreshed the file, zero when it says nothing.
	LastRefresh time.Time
}

// Dir is the directory the login lives in: CODEX_HOME when getenv returns one,
// else ~/.codex. getenv is a parameter so the choice is a function of what the
// caller passes and not of ambient state a test cannot see.
func Dir(getenv func(string) string) (string, error) {
	if d := getenv(HomeEnv); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codexauth: locating the home directory: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

// Load reads the credentials from the file at path. now is the clock the
// expiry is judged against.
func Load(path string, now time.Time) (Credentials, error) {
	if err := checkPrivate(path); err != nil {
		return Credentials{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, fmt.Errorf("%w (looked at %s)", ErrNotFound, path)
		}
		return Credentials{}, fmt.Errorf("codexauth: reading %s: %w", path, pathError(err))
	}

	var f struct {
		AuthMode string `json:"auth_mode"`
		Tokens   *struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	// The decoder's own error can quote a character of the input, so it is
	// dropped: which file and what is wrong is enough, and the content is not.
	if err := json.Unmarshal(data, &f); err != nil {
		return Credentials{}, fmt.Errorf("%w (%s is not valid JSON)", ErrMalformed, path)
	}
	if f.AuthMode != AuthModeChatGPT {
		return Credentials{}, fmt.Errorf("%w (%s)", ErrNotChatGPT, path)
	}
	if f.Tokens == nil || f.Tokens.AccessToken == "" {
		return Credentials{}, fmt.Errorf("%w (%s has no access token)", ErrMalformed, path)
	}

	c := Credentials{
		Path:        path,
		AccessToken: Secret{v: f.Tokens.AccessToken},
		AccountID:   Secret{v: f.Tokens.AccountID},
		Expires:     jwtExpiry(f.Tokens.AccessToken),
	}
	if t, err := time.Parse(time.RFC3339Nano, f.LastRefresh); err == nil {
		c.LastRefresh = t
	}
	if !c.Expires.IsZero() && !now.Before(c.Expires) {
		return Credentials{}, fmt.Errorf("%w (%s)", ErrExpired, path)
	}
	return c, nil
}

// pathError strips an *os.PathError down to its cause, since the caller names
// the path itself.
func pathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// jwtExpiry reads the `exp` claim of a JWT access token without verifying it:
// the backend does that, and this only decides whether to ask it at all. A token
// that is not a JWT, or has no exp, has no expiry here and is sent as it is.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(claims.Exp), 0).UTC()
}
