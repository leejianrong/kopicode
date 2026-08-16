// Package journal defines kopicode's one session record.
//
// Everything a user or a reviewer sees — REPL output, `run --print`, a bench
// classification — is derived from these events. There is no second transcript
// (docs/adr/0002-no-durable-runtime-own-journal.md decision 2).
//
// # The tagged union
//
// An Event is an envelope (schema version, session, seq, turn, timestamp)
// carrying exactly one Payload. The payload's Go type *is* the discriminator:
// Event deliberately has no separate type field, so the `type` on the wire is
// always Payload.Type() and the two cannot drift apart.
//
// # Compatibility
//
// Event types are a compatibility surface from the first commit. Decoding an
// event whose type this build does not know yields an UnknownPayload holding
// the payload bytes verbatim, and unknown *envelope* fields are kept the same
// way in Event.Extra — so an old kopicode can read, and rewrite, a journal
// written by a newer one without dropping anything.
//
// The bound worth stating: a new field added to an *existing* payload type is
// not preserved through a decode/encode cycle by a build that predates the
// field. Adding a field to a known payload is compatible for readers, not for
// rewriters. Adding a whole event type is compatible for both.
//
// # This file writes no file
//
// event.go and payload.go are the types and their JSON, and nothing here
// touches a disk. FileJournal — append, fsync, crash-truncated-line detection
// and secret redaction — is in file.go; the blob spill that keeps an oversized
// field whole without putting it in the line is in spill.go and blob.go.
package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// SchemaVersion is stamped on every event this build writes. Bump it when the
// envelope changes shape; payload types are added without a bump, because an
// unknown type already survives a round trip.
const SchemaVersion = 1

// Type is an event's wire discriminator. The string matches the Go payload
// type name, so a journal line names a type a reader can grep for.
type Type string

// The slice-1 event set (docs/SLICE-1.md §1).
const (
	TypeSessionStarted      Type = "SessionStarted"
	TypeUserMessage         Type = "UserMessage"
	TypeAssistantMessage    Type = "AssistantMessage"
	TypeThinkingBlock       Type = "ThinkingBlock"
	TypeProviderRequest     Type = "ProviderRequest"
	TypeProviderResponse    Type = "ProviderResponse"
	TypeToolCallRequested   Type = "ToolCallRequested"
	TypeToolCallParsed      Type = "ToolCallParsed"
	TypeToolCallRepaired    Type = "ToolCallRepaired"
	TypeToolCallFailed      Type = "ToolCallFailed"
	TypeToolResult          Type = "ToolResult"
	TypeEditApplied         Type = "EditApplied"
	TypeEditRejected        Type = "EditRejected"
	TypeSyntaxGateRun       Type = "SyntaxGateRun"
	TypePermissionRequested Type = "PermissionRequested"
	TypePermissionDecided   Type = "PermissionDecided"
	TypeTurnSnapshot        Type = "TurnSnapshot"
	TypeVerificationRun     Type = "VerificationRun"
	TypeTurnCancelled       Type = "TurnCancelled"
	TypeSessionEnded        Type = "SessionEnded"
)

// Payload is one event's typed body.
//
// Implementations are value types: a Payload is a record of something that
// already happened, and nothing should be holding a pointer to it expecting to
// change it after the fact.
type Payload interface {
	// Type reports the discriminator this payload is written under.
	Type() Type
}

// Event is the journal envelope.
//
// Seq is per-session monotonic and 1-based; Turn is the turn the event belongs
// to, and 0 for events outside any turn (session start and end). Time is
// written as RFC 3339 in UTC.
type Event struct {
	SchemaVersion int
	SessionID     string
	Seq           uint64
	Turn          int
	Time          time.Time
	Payload       Payload

	// Extra holds envelope fields written by a newer schema version than this
	// build knows, kept verbatim so re-encoding is lossless. It is populated by
	// decoding and should not be filled in by hand; keys colliding with known
	// envelope fields are dropped on encode.
	Extra map[string]json.RawMessage
}

// Type reports the event's discriminator, derived from its payload. A zero
// Event reports the empty type rather than panicking, so error paths can name
// it safely.
func (e Event) Type() Type {
	if e.Payload == nil {
		return ""
	}
	return e.Payload.Type()
}

// wireEvent is the on-disk shape. Field order here is the byte order of an
// encoded event, which the byte-identical-replay acceptance criterion relies
// on.
type wireEvent struct {
	SchemaVersion int             `json:"schema_version"`
	SessionID     string          `json:"session_id"`
	Seq           uint64          `json:"seq"`
	Turn          int             `json:"turn"`
	Type          Type            `json:"type"`
	TS            string          `json:"ts"`
	Payload       json.RawMessage `json:"payload"`
}

// envelopeField reports whether name is an envelope field this build knows.
// Used to keep Extra from shadowing a real field.
func envelopeField(name string) bool {
	switch name {
	case "schema_version", "session_id", "seq", "turn", "type", "ts", "payload":
		return true
	default:
		return false
	}
}

// Sentinel causes, for errors.Is. Every decode and encode failure wraps one of
// these in a *FieldError naming the field and the event it came from.
var (
	// ErrMissingField reports a required envelope field that was absent or empty.
	ErrMissingField = errors.New("missing")
	// ErrInvalidField reports a field that was present but not usable.
	ErrInvalidField = errors.New("invalid")
	// ErrUnknownType reports a discriminator this build has no payload for.
	// Event.UnmarshalJSON does not return it — see UnknownPayload.
	ErrUnknownType = errors.New("unknown event type")
)

// FieldError says which field of which event went wrong, because an error a
// reader cannot act on gets deleted by whoever hits it at 2am.
type FieldError struct {
	Seq   uint64 // 0 when the sequence number itself could not be read
	Type  Type   // "" when the discriminator is what is missing
	Field string // the JSON field name, not the Go one
	Hint  string // optional: what the reader should do about it
	Err   error  // one of the sentinels above
}

func (e *FieldError) Error() string {
	where := "event"
	if e.Seq != 0 {
		where = fmt.Sprintf("event seq %d", e.Seq)
	}
	if e.Type != "" {
		where += fmt.Sprintf(" (type %q)", e.Type)
	}
	msg := fmt.Sprintf("journal: %s: field %q: %v", where, e.Field, e.Err)
	if e.Hint != "" {
		msg += " — " + e.Hint
	}
	return msg
}

func (e *FieldError) Unwrap() error { return e.Err }

// UnknownPayload carries an event whose type this build does not know.
//
// It keeps the payload bytes exactly as they were read, so writing the event
// back out loses nothing. Preserving only the type string would still lose the
// event, which is the failure mode this type exists to prevent.
type UnknownPayload struct {
	// EventType is the discriminator read off the wire.
	EventType Type
	// Raw is the payload object, verbatim (JSON-compacted on re-encode).
	Raw json.RawMessage
}

// Type reports the discriminator the event was read under, so an unknown event
// re-encodes under its own name rather than a placeholder.
func (u UnknownPayload) Type() Type { return u.EventType }

// MarshalJSON returns the payload bytes untouched.
func (u UnknownPayload) MarshalJSON() ([]byte, error) {
	if len(u.Raw) == 0 {
		return []byte("null"), nil
	}
	return u.Raw, nil
}

// UnmarshalJSON keeps the bytes without interpreting them. EventType is set by
// Event.UnmarshalJSON, which is the only thing that knows it.
func (u *UnknownPayload) UnmarshalJSON(data []byte) error {
	u.Raw = append(json.RawMessage(nil), data...)
	return nil
}

// decodeInto decodes a payload of a known type.
func decodeInto[T Payload](raw []byte) (Payload, error) {
	var p T
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return p, nil
}

// registry maps each known discriminator to its decoder. A type absent here is
// not an error on read — it becomes an UnknownPayload.
var registry = map[Type]func([]byte) (Payload, error){
	TypeSessionStarted:      decodeInto[SessionStarted],
	TypeUserMessage:         decodeInto[UserMessage],
	TypeAssistantMessage:    decodeInto[AssistantMessage],
	TypeThinkingBlock:       decodeInto[ThinkingBlock],
	TypeProviderRequest:     decodeInto[ProviderRequest],
	TypeProviderResponse:    decodeInto[ProviderResponse],
	TypeToolCallRequested:   decodeInto[ToolCallRequested],
	TypeToolCallParsed:      decodeInto[ToolCallParsed],
	TypeToolCallRepaired:    decodeInto[ToolCallRepaired],
	TypeToolCallFailed:      decodeInto[ToolCallFailed],
	TypeToolResult:          decodeInto[ToolResult],
	TypeEditApplied:         decodeInto[EditApplied],
	TypeEditRejected:        decodeInto[EditRejected],
	TypeSyntaxGateRun:       decodeInto[SyntaxGateRun],
	TypePermissionRequested: decodeInto[PermissionRequested],
	TypePermissionDecided:   decodeInto[PermissionDecided],
	TypeTurnSnapshot:        decodeInto[TurnSnapshot],
	TypeVerificationRun:     decodeInto[VerificationRun],
	TypeTurnCancelled:       decodeInto[TurnCancelled],
	TypeSessionEnded:        decodeInto[SessionEnded],
}

// KnownTypes lists every discriminator this build can decode into a typed
// payload, sorted. Anything else round-trips as an UnknownPayload.
func KnownTypes() []Type {
	out := make([]Type, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ParsePayload decodes raw into the payload for t.
//
// It returns ErrUnknownType for a discriminator this build does not know.
// Event.UnmarshalJSON deliberately does not propagate that: reading a journal
// must tolerate a future event type. Use this directly only where an unknown
// type is genuinely a fault — a strict validator, or a test.
func ParsePayload(t Type, raw []byte) (Payload, error) {
	if t == "" {
		return nil, &FieldError{
			Field: "type",
			Err:   ErrMissingField,
			Hint:  "every journal event names its payload type; a line without one cannot be interpreted",
		}
	}
	decode, ok := registry[t]
	if !ok {
		return nil, &FieldError{
			Type:  t,
			Field: "type",
			Err:   ErrUnknownType,
			Hint: fmt.Sprintf(
				"this build knows %d types; if the journal came from a newer kopicode, decode with Event.UnmarshalJSON, which preserves unknown types",
				len(registry),
			),
		}
	}
	p, err := decode(raw)
	if err != nil {
		return nil, &FieldError{Type: t, Field: "payload", Err: fmt.Errorf("%w: %w", ErrInvalidField, err)}
	}
	return p, nil
}

// validate checks the envelope invariants that both directions share, so a
// value that decodes can always be encoded again.
func (e Event) validate() error {
	if e.Payload == nil {
		return &FieldError{Seq: e.Seq, Field: "payload", Err: ErrMissingField,
			Hint: "an event with no payload records nothing"}
	}
	if e.Payload.Type() == "" {
		return &FieldError{Seq: e.Seq, Field: "type", Err: ErrMissingField,
			Hint: "the payload reported an empty discriminator"}
	}
	if e.SchemaVersion < 1 {
		return &FieldError{Seq: e.Seq, Type: e.Type(), Field: "schema_version", Err: ErrMissingField,
			Hint: fmt.Sprintf("events written by this build carry %d", SchemaVersion)}
	}
	if e.SessionID == "" {
		return &FieldError{Seq: e.Seq, Type: e.Type(), Field: "session_id", Err: ErrMissingField}
	}
	if e.Seq < 1 {
		return &FieldError{Type: e.Type(), Field: "seq", Err: ErrInvalidField,
			Hint: "seq is per-session monotonic and 1-based"}
	}
	if e.Turn < 0 {
		return &FieldError{Seq: e.Seq, Type: e.Type(), Field: "turn", Err: ErrInvalidField,
			Hint: "turn is 1-based, or 0 for events outside a turn"}
	}
	if e.Time.IsZero() {
		return &FieldError{Seq: e.Seq, Type: e.Type(), Field: "ts", Err: ErrMissingField,
			Hint: "the engine stamps every event from its injected clock"}
	}
	return nil
}

// encode marshals v without HTML escaping.
//
// The default encoder rewrites angle brackets and ampersands as unicode
// escapes, which turns every shell "&&" and most diff lines in the record into
// escape soup. The journal is meant to be read by a human auditing a session,
// so it is written unescaped.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Marshal encodes one journal line.
//
// Prefer this over json.Marshal(e): encoding/json re-escapes HTML in whatever a
// MarshalJSON method returns, so json.Marshal produces the same event with
// unicode-escape noise through every diff and shell command. Both are valid and
// both decode identically; only one is readable.
func Marshal(e Event) ([]byte, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}

	payload, err := encode(e.Payload)
	if err != nil {
		return nil, &FieldError{Seq: e.Seq, Type: e.Type(), Field: "payload",
			Err: fmt.Errorf("%w: %w", ErrInvalidField, err)}
	}

	w := wireEvent{
		SchemaVersion: e.SchemaVersion,
		SessionID:     e.SessionID,
		Seq:           e.Seq,
		Turn:          e.Turn,
		Type:          e.Payload.Type(),
		TS:            e.Time.UTC().Format(time.RFC3339Nano),
		Payload:       payload,
	}

	out, err := encode(w)
	if err != nil {
		return nil, fmt.Errorf("journal: encoding event seq %d: %w", e.Seq, err)
	}
	if len(e.Extra) == 0 {
		return out, nil
	}
	return mergeExtra(out, e.Extra)
}

// mergeExtra folds preserved future envelope fields back into an encoded
// event. Key order becomes sorted rather than declaration order, which only
// happens for events this build did not write.
func mergeExtra(encoded []byte, extra map[string]json.RawMessage) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, fmt.Errorf("journal: re-reading encoded event: %w", err)
	}
	for k, v := range extra {
		if envelopeField(k) {
			continue // a known field always wins over a preserved copy
		}
		fields[k] = v
	}
	out, err := encode(fields)
	if err != nil {
		return nil, fmt.Errorf("journal: encoding preserved envelope fields: %w", err)
	}
	return out, nil
}

// MarshalJSON implements json.Marshaler so an Event nests correctly inside
// other documents. Marshal is the canonical encoder for a journal line.
func (e Event) MarshalJSON() ([]byte, error) { return Marshal(e) }

// UnmarshalJSON decodes one journal line.
//
// The discriminator is checked first, because it decides how everything else is
// read: an unknown type is preserved, a missing one is a hard error. Envelope
// fields this build does not know are kept in Extra.
func (e *Event) UnmarshalJSON(data []byte) error {
	var w wireEvent
	if err := json.Unmarshal(data, &w); err != nil {
		return fmt.Errorf("journal: decoding event envelope: %w", err)
	}

	if w.Type == "" {
		return &FieldError{
			Seq:   w.Seq,
			Field: "type",
			Err:   ErrMissingField,
			Hint:  "every journal event names its payload type; a line without one cannot be interpreted",
		}
	}

	// An absent payload is normalised to null so the value round-trips to
	// itself rather than flipping between nil and null.
	raw := w.Payload
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}

	payload, err := ParsePayload(w.Type, raw)
	if err != nil {
		if !errors.Is(err, ErrUnknownType) {
			var fe *FieldError
			if errors.As(err, &fe) {
				fe.Seq = w.Seq
			}
			return err
		}
		// The compatibility promise: keep the bytes, keep the name.
		payload = UnknownPayload{EventType: w.Type, Raw: raw}
	}

	ts, err := time.Parse(time.RFC3339, w.TS)
	if err != nil {
		return &FieldError{Seq: w.Seq, Type: w.Type, Field: "ts",
			Err:  fmt.Errorf("%w: %w", ErrInvalidField, err),
			Hint: "expected RFC 3339 in UTC"}
	}

	extra, err := extraFields(data)
	if err != nil {
		return err
	}

	decoded := Event{
		SchemaVersion: w.SchemaVersion,
		SessionID:     w.SessionID,
		Seq:           w.Seq,
		Turn:          w.Turn,
		Time:          ts.UTC(),
		Payload:       payload,
		Extra:         extra,
	}
	if err := decoded.validate(); err != nil {
		return err
	}

	*e = decoded
	return nil
}

// extraFields returns the top-level fields this build has no home for.
func extraFields(data []byte) (map[string]json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("journal: decoding event envelope: %w", err)
	}
	var extra map[string]json.RawMessage
	for k, v := range all {
		if envelopeField(k) {
			continue
		}
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		extra[k] = v
	}
	return extra, nil
}
