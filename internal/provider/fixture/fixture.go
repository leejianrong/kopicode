// Package fixture holds recorded — for now, hand-authored — provider traffic
// and the loader that reads it.
//
// It is data plus the minimum code to read and check that data. It is not the
// mock provider: replay semantics, turn keying and the provider interface
// belong to KAN-773, and nothing here decides them.
//
// # These fixtures are synthesised, and that is a known, temporary violation
//
// CLAUDE.md's test-seam rule is explicit: "The mock provider replays recorded
// traffic rather than synthesising it. If it drifts from real provider
// behaviour, green plumbing tests will mask real breakage." Every fixture in
// this package's data directory today is hand-written, because the build order
// puts the mock provider at docs/SLICE-1.md §Build Plan step 3 and the real
// OpenRouter client at step 8 — there is no recorded traffic yet to replay.
//
// So the drift risk is real and it is the most dangerous thing about this
// package: a hand-authored body that does not match what OpenRouter actually
// sends produces a green suite over a loop that would fail on the first live
// request. Three things bound it, none of which eliminate it:
//
//   - Every fixture declares its [Origin] and the loader refuses a file that
//     does not. A synthetic fixture is mechanically distinguishable from a
//     recorded one, so KAN-774's recorder can replace them file by file and
//     nobody downstream can mistake one for evidence about the provider.
//   - [Validate] checks each fixture against itself: the SSE frames must
//     assemble to the same completion the non-streaming body carries, the
//     declared finish reason and usage must match the body, and the pin must be
//     a pin. A fixture that is internally inconsistent is a fixture somebody
//     hand-edited half of.
//   - The wire shapes in wire.go name where each field was verified against
//     OpenRouter's published documentation, and say so plainly where it could
//     not be.
//
// What none of that can do is prove the shape is right. Only a recording can.
// When one lands, it wins: a recorded fixture replaces the hand-authored one of
// the same name, and the synthetic file goes rather than being kept as a
// second opinion.
//
// # Determinism
//
// A replayed session must produce a byte-identical journal (docs/SLICE-1.md
// §Test Plan), so nothing in a fixture may depend on the clock, on a random
// value or on the machine. Every id, every `created` stamp and every token
// count in the data directory is a fixed literal. They look like values a
// provider produced; they are values a human typed.
//
// # No credentials, by construction
//
// A recorded fixture is real HTTP and real request headers carry
// OPENROUTER_API_KEY, which is why the recorder scrubs at write time through a
// header allowlist rather than a denylist (docs/SLICE-1.md §Build Plan step 3).
// Fixtures here carry no request headers at all and the response header map is
// bounded by [allowedResponseHeaders]. There is nothing to scrub because there
// is nowhere to put it.
package fixture

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Version is the fixture file format version, carried by every file.
//
// It is separate from journal.SchemaVersion and from the anchor version on
// purpose: this is the shape of the *container*, and it changes when the
// container changes, not when the provider's wire format or the journal does.
const Version = 1

// dataDir is the directory the built-in fixtures live in, relative to this
// package.
const dataDir = "data"

// builtin embeds the fixtures that ship with the binary.
//
// They are embedded rather than read from disk because `make bench-smoke` runs
// the corpus against the mock provider from a built kopibench, which has no
// source tree to read from. [FS] hands the same tree to a caller that wants to
// walk it, and [Load] takes an [fs.FS] so a test or a future recorder can point
// at a directory instead.
//
//go:embed data/*.json
var builtin embed.FS

// Origin says where a fixture's content came from. It is the marker that keeps
// synthesised traffic from being mistaken for evidence — see the package doc.
type Origin string

const (
	// OriginHandAuthored means a human wrote this file. It is a plausible
	// shape, not a record of anything the provider did.
	OriginHandAuthored Origin = "hand_authored"

	// OriginRecorded means the file was written by the fixture recorder from
	// real provider traffic, scrubbed at write time.
	OriginRecorded Origin = "recorded"
)

// origins is every value Origin may take. A file naming anything else is
// rejected rather than defaulted, because the one default that is safe here
// ("assume synthetic") would silently downgrade a real recording, and the
// other ("assume recorded") is how a hand-written body gets quoted as evidence.
var origins = map[Origin]bool{
	OriginHandAuthored: true,
	OriginRecorded:     true,
}

// Valid reports whether o names a real origin.
func (o Origin) Valid() bool { return origins[o] }

// Synthetic reports whether the content was written by hand rather than
// recorded. Callers that report on provider behaviour must say so when this is
// true.
func (o Origin) Synthetic() bool { return o == OriginHandAuthored }

// Fixture is one session's worth of provider traffic: the arm it was taken
// under, and the exchanges in order.
type Fixture struct {
	// FormatVersion is the container version — [Version] for anything this
	// build wrote.
	FormatVersion int `json:"format_version"`

	// Origin is hand_authored or recorded, and is required.
	Origin Origin `json:"origin"`

	// OriginNote explains the provenance in one sentence. Required when Origin
	// is hand_authored: a synthetic fixture has to say what it is modelling and
	// on what basis, or the next reader has no way to judge it.
	OriginNote string `json:"origin_note"`

	// Name identifies the fixture. It matches the file's base name, which
	// [Load] checks, so a fixture cannot be renamed on disk into something the
	// mock provider then fails to find by the name inside it.
	Name string `json:"name"`

	// Description says what session this is, in the model's terms.
	Description string `json:"description"`

	// ModelID is the provider's model identifier for the arm this was taken
	// under.
	ModelID string `json:"model_id"`

	// Pin is the routing every request in this session declared
	// (docs/adr/0005-benchmark-and-ab-methodology.md §2). It is carried so a
	// replayed session can be checked against the pin the experiment declares
	// rather than assumed to match it.
	//
	// KAN-775 chose the value the shipped fixtures carry — `parasail/bf16` at
	// `bf16`, from the endpoints OpenRouter served for qwen/qwen3-coder-next on
	// 2026-08-14. The evidence and the reasoning are in docs/provider-pin.md,
	// and that file is where a changed pin gets argued and dated; this package
	// only refuses one that could not have been a pin at all.
	Pin Pin `json:"pin"`

	// Exchanges are the request/response pairs in the order they happened.
	Exchanges []Exchange `json:"exchanges"`
}

// Pin is the provider routing demanded on a request.
//
// It mirrors journal.ProviderPin field for field but is its own declaration:
// this package must not import the journal (the engine journals; packages
// return data), and the journal must not import this. The coupling is a
// documented wire contract, the same way parse.Route's strings are.
type Pin struct {
	// Order is provider.order — a single slug for a benchmark run.
	Order []string `json:"order"`
	// AllowFallbacks is false on every benchmark request. [Validate] refuses a
	// fixture that says otherwise, because a fixture recorded with fallbacks on
	// is not a record of the arm it claims.
	AllowFallbacks bool `json:"allow_fallbacks"`
	// Quantizations is the fixed quantization set requested.
	Quantizations []string `json:"quantizations"`
}

// Provisional reports whether the pin still carries the placeholder that stood
// in before KAN-775 chose a provider.
//
// The marker's meaning inverted when the decision landed. While there was no
// pin, a fixture carrying a plausible slug would have been inventing a project
// decision, so the placeholder was *required* and the suite checked for it. Now
// that docs/provider-pin.md holds a real one, a placeholder is the thing that
// must not survive: a fixture pinned to nothing declares an arm nobody can
// reproduce, and ADR-0005 §2 discards results whose pin does not match the
// declared pin — which a placeholder trivially never does. [Validate] refuses
// it.
func (p Pin) Provisional() bool {
	for _, slug := range p.Order {
		if strings.HasPrefix(slug, provisionalPrefix) {
			return true
		}
	}
	for _, q := range p.Quantizations {
		if strings.HasPrefix(q, provisionalPrefix) {
			return true
		}
	}
	return false
}

// provisionalPrefix marks a pin value nobody has chosen yet. It is deliberately
// not a plausible provider slug: a fixture carrying "together" or "deepinfra"
// would read as a decision this project has not made.
//
// It is kept rather than deleted with the placeholder it named, because the
// same situation recurs on the next model: a fixture may be written before the
// arm it belongs to has been pinned, and the marker is how that fixture says so
// out loud instead of guessing. What changed is that such a fixture no longer
// loads.
const provisionalPrefix = "PROVISIONAL-"

// quantizations is the vocabulary OpenRouter accepts in provider.quantizations,
// verified against its provider-routing documentation on 2026-08-14.
//
// This exists because the failure mode a pin invites is not a typo — it is a
// *plausible invention*. ADR-0005 §2 discards a result whose pin does not match
// the experiment's declared pin, and the comparison is between a declaration and
// itself, so a quantization no provider serves passes every internal check and
// silently makes the whole series unfalsifiable. Holding the field to a closed
// documented set is the cheapest place to catch that.
//
// "unknown" is on the list because OpenRouter really accepts it: some providers
// do not report what they serve. It is a legal filter and a poor pin, and
// docs/provider-pin.md says why that ruled two endpoints out.
var quantizations = map[string]bool{
	"int4":    true,
	"int8":    true,
	"fp4":     true,
	"mxfp4":   true,
	"nvfp4":   true,
	"fp6":     true,
	"fp8":     true,
	"mxfp8":   true,
	"fp16":    true,
	"bf16":    true,
	"fp32":    true,
	"unknown": true,
}

// Exchange is one request and the response it drew.
type Exchange struct {
	// Turn is the 1-based turn this exchange belongs to, matching the journal
	// envelope's turn. Two exchanges may share a turn when a request was
	// retried; Attempt tells them apart.
	Turn int `json:"turn"`

	// Attempt is 1 for the first send of a turn and increments once per repair
	// round trip, matching journal.ProviderRequest.Attempt exactly — the
	// engine's own re-sends, one per call to provider.Provider.Complete. It
	// does not, and cannot, stand in for a client-internal 429/5xx retry
	// *inside* one Complete call: the replay provider never sends anything over
	// a wire, so it has no such retries to represent, and
	// journal.ProviderRetried (KAN-851) is what a live session journals for
	// those instead.
	Attempt int `json:"attempt"`

	// Note says what this exchange is demonstrating, for the reader of a failed
	// test rather than for the machine.
	Note string `json:"note"`

	// Response is what came back.
	Response Response `json:"response"`

	// Expect are the facts a consumer may assert against without decoding the
	// body itself. Every one of them is cross-checked against Response.Body by
	// [Validate], so they are a convenience and never a second source of truth.
	Expect Expect `json:"expect"`
}

// Response is one provider reply, in both the shapes kopicode has to handle.
//
// Both are carried because which one the mock provider serves is KAN-773's
// decision, not this card's. The real client streams (docs/SLICE-1.md §Build
// Plan step 8), so Stream is what a faithful replay at the transport level
// feeds; Body is what the same exchange looks like assembled, which is what
// journal.ProviderResponse.Body holds and what a replay at the provider
// interface returns. [Validate] holds the two to each other so they cannot
// drift apart in a file somebody edited half of.
type Response struct {
	// Status is the HTTP status code.
	Status int `json:"status"`

	// Headers are response headers worth keeping, bounded by
	// [allowedResponseHeaders]. Request headers are not represented at all —
	// that is where the API key travels.
	Headers map[string]string `json:"headers,omitempty"`

	// Body is the assembled chat-completion object, verbatim. Held raw rather
	// than as a struct so that a byte for byte replay stays possible and so
	// that a field this build does not model is not silently dropped on the way
	// through.
	Body json.RawMessage `json:"body"`

	// Stream is the SSE payload as a sequence of raw lines, exactly as they
	// arrive on the wire including the blank separator lines, the
	// `: OPENROUTER PROCESSING` comments and the terminating `data: [DONE]`.
	// Empty for a fixture recorded without streaming.
	Stream []string `json:"stream,omitempty"`
}

// Streamed reports whether this response carries SSE frames.
func (r Response) Streamed() bool { return len(r.Stream) > 0 }

// Expect are the derived facts a fixture states outright.
type Expect struct {
	// FinishReason is the choice's normalised finish_reason, verbatim.
	FinishReason string `json:"finish_reason"`

	// Route is the parse route this reply is meant to exercise: "native",
	// "fenced_json", "xml_tag", or "" when the reply carries no tool call at
	// all. The wire form matches parse.Route's, which the fixture tests assert
	// by running the real extractor over the assembled message.
	Route string `json:"route"`

	// Tools are the tool names extraction should find in this reply, in order,
	// whichever route carried them. Empty for a terminal text reply.
	//
	// Note that this is *not* the same as the body's native tool_calls array:
	// on the fenced and XML routes the reply calls a tool and the native array
	// is empty, which is the entire difficulty those routes exist for.
	// [Validate] can only hold this to the native array when there is one,
	// because checking the text routes means running the extractor and this
	// package does not import it. The package's tests do, and that is where the
	// text routes are checked.
	Tools []string `json:"tools,omitempty"`

	// Usage is the token accounting the body reports.
	Usage Usage `json:"usage"`

	// ServedBy is the upstream provider that answered, from the body's
	// top-level `provider` field. A result served outside the declared pin is
	// discarded rather than adjusted (ADR-0005 §2), so it is stated here.
	ServedBy string `json:"served_by"`
}

// Usage is the token accounting, in the journal's terms rather than the
// provider's. The provider spells these prompt_tokens/completion_tokens/
// total_tokens; journal.TokenCounts spells them prompt/completion/total, and
// this is the fixture's statement of what the mapping should produce.
type Usage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

// allowedResponseHeaders is the allowlist of response headers a fixture may
// carry.
//
// An allowlist rather than a denylist, for the reason the recorder uses one: a
// denylist stays correct only until the provider adds a header, and the header
// it adds is the one nobody thought to exclude. Response headers do not carry
// the request's Authorization, but the recorder will write this file type and
// the rule should already be in place when it does.
var allowedResponseHeaders = map[string]bool{
	"content-type": true,
	"x-request-id": true,
}

// ErrNotFound reports a fixture name with no file behind it.
var ErrNotFound = errors.New("fixture not found")

// FS returns the built-in fixture files, rooted at the data directory.
func FS() fs.FS {
	sub, err := fs.Sub(builtin, dataDir)
	if err != nil {
		// Unreachable: the directory is embedded at compile time by the
		// directive above, so a failure here means the binary was built
		// without it, which `go build` does not permit.
		panic("fixture: embedded data directory missing: " + err.Error())
	}
	return sub
}

// Names lists the fixture names in fsys, sorted.
//
// Discovery is by directory walk rather than by a list kept in code, so a
// fixture added to the data directory is validated by the existing tests
// instead of waiting for somebody to remember to register it.
func Names(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("fixture: reading fixture directory: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

// Load reads one fixture by name and validates it.
//
// An invalid fixture is an error, not a warning: a loader that returns a
// half-checked fixture puts the check at every call site, and the call site
// that skips it is the one that ships.
func Load(fsys fs.FS, name string) (Fixture, error) {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return Fixture{}, fmt.Errorf("fixture: %q is not a fixture name: %w", name, ErrNotFound)
	}
	data, err := fs.ReadFile(fsys, path.Join(name+".json"))
	if err != nil {
		return Fixture{}, fmt.Errorf("fixture %q: %w: %w", name, ErrNotFound, err)
	}

	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var f Fixture
	if err := dec.Decode(&f); err != nil {
		return Fixture{}, fmt.Errorf("fixture %q: decoding: %w", name, err)
	}

	if f.Name != name {
		return Fixture{}, fmt.Errorf(
			"fixture %q: the file declares name %q; the name inside the file and the file's own name must agree, "+
				"or a fixture renamed on disk becomes one the mock provider cannot find",
			name, f.Name)
	}
	if err := Validate(f); err != nil {
		return Fixture{}, err
	}
	return f, nil
}

// LoadAll reads and validates every fixture in fsys, in name order.
func LoadAll(fsys fs.FS) ([]Fixture, error) {
	names, err := Names(fsys)
	if err != nil {
		return nil, err
	}
	out := make([]Fixture, 0, len(names))
	for _, name := range names {
		f, err := Load(fsys, name)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
