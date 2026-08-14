package parse_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/leejianrong/kopicode/internal/parse"
)

// The wire form of every Route, Kind and ArgEncoding constant — the exact
// string, the round trip, the distinctness, and the fact that a constant with no
// wire name refuses to marshal — is guarded in wirename_internal_test.go, over a
// table whose completeness is derived from the source with go/ast (KAN-841).
// Enumerating them here as well was the hand-written half that let a new
// constant land unchecked, so what remains in this file is the behaviour that is
// not per-constant.

// TestUnknownWireNamesAreRefused covers the input side of the compatibility
// surface: a name this build does not know is an error, never a silent zero
// value. RouteUnknown and KindUnspecified are the zero values, so a lenient
// decode would turn an unreadable session into a plausible-looking one.
func TestUnknownWireNamesAreRefused(t *testing.T) {
	var r parse.Route
	if err := r.UnmarshalText([]byte("smoke_signals")); err == nil {
		t.Error("Route.UnmarshalText accepted an unknown route")
	}
	if r != parse.RouteUnknown {
		t.Errorf("a refused decode left the route as %v; it must not touch the value", r)
	}

	var k parse.Kind
	if err := k.UnmarshalText([]byte("vibes")); err == nil {
		t.Error("Kind.UnmarshalText accepted an unknown kind")
	}

	var a parse.ArgEncoding
	if err := a.UnmarshalText([]byte("interpretive_dance")); err == nil {
		t.Error("ArgEncoding.UnmarshalText accepted an unknown encoding")
	}
}

// TestRouteUnknownIsNotValid states the zero-value rule at the exported
// boundary: an extraction that does not say which route produced it does not
// exist.
func TestRouteUnknownIsNotValid(t *testing.T) {
	if parse.RouteUnknown.Valid() {
		t.Error("RouteUnknown reports itself valid")
	}
}

// TestJSONShape checks each type serialises as its name inside a struct, which
// is how a caller encoding one reaches the wire form, and decodes back from it.
// A type that marshals through TextMarshaler and cannot be unmarshalled is a
// one-way trip — the asymmetry KAN-841 closed on ArgEncoding.
func TestJSONShape(t *testing.T) {
	type envelope struct {
		Route       parse.Route       `json:"route"`
		Kind        parse.Kind        `json:"kind"`
		ArgEncoding parse.ArgEncoding `json:"arg_encoding"`
	}

	want := envelope{parse.RouteFencedJSON, parse.KindInvalidJSON, parse.ArgsJSONString}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	const wantJSON = `{"route":"fenced_json","kind":"invalid_json","arg_encoding":"json_string"}`
	if string(b) != wantJSON {
		t.Errorf("Marshal() = %s, want %s", b, wantJSON)
	}

	var got envelope
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal(%s) = %v", b, err)
	}
	if got != want {
		t.Errorf("round trip produced %+v, want %+v", got, want)
	}
}

// TestErrorNamesRouteAndCause checks the error text carries what a human
// debugging a bad session needs, and that the cause survives for errors.Is.
func TestErrorNamesRouteAndCause(t *testing.T) {
	_, err := parse.Extract(parse.Message{Content: "```tool\n{'tool': 'read_file'}\n```"})
	if err == nil {
		t.Fatal("Extract() accepted single-quoted JSON")
	}

	var perr *parse.Error
	if !errors.As(err, &perr) {
		t.Fatalf("error %v is not a *parse.Error", err)
	}
	if perr.Route != parse.RouteFencedJSON {
		t.Errorf("route = %s, want fenced_json", perr.Route)
	}
	if perr.Snippet == "" {
		t.Error("the error carries no snippet of the offending text")
	}
	if errors.Unwrap(err) == nil {
		t.Error("the JSON decode error was not wrapped; %w is how the cause survives")
	}
	if errors.Is(err, parse.ErrNoToolCall) {
		t.Error("a malformed call reported itself as no call at all")
	}
}
