package provider

import (
	"errors"
	"log/slog"
	"os"
)

// KeyEnv is the environment variable the API key is read from.
//
// It is the one input to this binary that must be ambient. ADR-0007 decision 3
// bans an environment variable for the model id and gives the reason —
// provenance: a result has to be attributable, and an ambient value is invisible
// in the shell history and in the diff. A credential is the opposite kind of
// fact. It must *not* be written to a file anyone can commit, so the asymmetry
// is deliberate and points the other way.
const KeyEnv = "OPENROUTER_API_KEY"

// ErrNoAPIKey reports that [KeyEnv] was unset or empty.
//
// It is a startup failure rather than a request failure: a client built without
// a credential will fail every request with a 401 that reads like a provider
// problem, which is the same misreporting ADR-0007 decision 4 refuses for an
// unknown model id.
var ErrNoAPIKey = errors.New("provider: " + KeyEnv + " is unset")

// APIKey is the provider credential.
//
// It is a type rather than a string because the failure this card most invites
// is not a deliberate leak, it is an incidental one: a `%v` on a struct that
// happens to hold the key, an error that wraps the request, a debug log of the
// client's configuration. Any of those puts the credential in the journal, in a
// blob or on a terminal, and the rule is that it appears in none of them
// (CLAUDE.md, "Commands").
//
// So the value is unexported and every way the standard library has of turning
// a value into text is overridden to return [Redacted]:
//
//   - [APIKey.String] covers fmt's %v, %s and %q — fmt consults a Stringer for
//     all of them, and for %x and %X too.
//   - [APIKey.GoString] covers %#v, which would otherwise print the struct's
//     fields including unexported ones.
//   - [APIKey.MarshalText] and [APIKey.MarshalJSON] cover encoding/json, so a
//     type that embeds a key cannot serialise one into a record.
//   - [APIKey.LogValue] covers log/slog, which is the one output path in this
//     package that a human deliberately points at a terminal.
//
// The value itself is reachable only from inside this package, through
// [APIKey.reveal], and the only caller is the function that sets the
// Authorization header.
type APIKey struct{ v string }

// Redacted is what an [APIKey] renders as, everywhere.
//
// It is a fixed literal rather than a masked prefix of the real value. A "first
// four characters" mask is a smaller leak, not the absence of one, and this
// project's rule has no size threshold in it.
const Redacted = "[redacted]"

// NewAPIKey wraps a credential the caller already holds.
//
// It exists for tests and for a future config path. Production reads the
// environment through [APIKeyFromEnv].
func NewAPIKey(v string) APIKey { return APIKey{v: v} }

// APIKeyFromEnv reads the credential from [KeyEnv].
//
// It returns [ErrNoAPIKey] rather than an empty key, so a missing credential is
// refused where it can still be reported as configuration.
func APIKeyFromEnv() (APIKey, error) {
	v, ok := os.LookupEnv(KeyEnv)
	if !ok || v == "" {
		return APIKey{}, ErrNoAPIKey
	}
	return APIKey{v: v}, nil
}

// IsZero reports whether the key is empty.
func (k APIKey) IsZero() bool { return k.v == "" }

// String renders the key as [Redacted]. fmt consults it for %v, %s, %q, %x and
// %X.
func (k APIKey) String() string { return Redacted }

// GoString renders the key as [Redacted] under %#v, which would otherwise reach
// past the unexported field.
func (k APIKey) GoString() string { return `provider.APIKey("` + Redacted + `")` }

// MarshalText renders the key as [Redacted]. It covers encoding/json for a key
// used as a map key, and every other TextMarshaler consumer.
func (k APIKey) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// MarshalJSON renders the key as a [Redacted] JSON string.
//
// MarshalText alone would be enough for encoding/json, but only for as long as
// nobody adds a MarshalJSON to a type that embeds this one. Declaring both makes
// the redaction independent of which interface a given encoder happens to
// prefer.
func (k APIKey) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// LogValue renders the key as [Redacted] to log/slog.
func (k APIKey) LogValue() slog.Value { return slog.StringValue(Redacted) }

// reveal returns the credential itself.
//
// Unexported on purpose: the only correct use is building the Authorization
// header, which happens in this package, and a caller outside it that wants the
// bytes wants them for something the journal will see.
func (k APIKey) reveal() string { return k.v }
