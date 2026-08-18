package parse

import "fmt"

// Route names the extraction route that produced a tool call.
//
// Which route a model actually reaches for is a finding worth having — a model
// that never uses its native tool_calls channel and always falls back to text
// is telling us something about the harness — so the route is a first-class
// result rather than a debug detail, and the engine journals it.
//
// It is a typed value, not a string, so a route cannot be invented at a call
// site, and its zero value is deliberately invalid: an extraction that does not
// say which route produced it does not exist (see [Extraction]).
type Route uint8

const (
	// RouteUnknown is the zero value and never names a successful extraction.
	RouteUnknown Route = iota

	// RouteNative is the OpenAI-style tool_calls array on the response itself.
	RouteNative

	// RouteFencedJSON is a ```tool fenced block in the assistant's text.
	RouteFencedJSON

	// RouteXMLTag is a <tool_call>…</tool_call> block in the assistant's text.
	RouteXMLTag
)

// routeOrder is the order [Extract] itself tries routes in, first success wins
// (docs/SLICE-1.md §3). It is what a harness configuration's ParseRoutes names
// when left at its shipped value (internal/harness's "default" configuration),
// and [DefaultRouteOrder] is the copy of it a caller gets to build on.
//
// This is deliberately not the only order in the binary any more (KAN-855): a
// [Repairer] runs on whatever order [NewRepairer] was given, which is how
// Config.ParseRoutes reaches actual behaviour instead of only the hash
// preimage. This var is Extract's own default and the zero-value fallback
// NewRepairer uses when it is not told to vary — see both doc comments.
var routeOrder = [...]Route{RouteNative, RouteFencedJSON, RouteXMLTag}

// DefaultRouteOrder returns the order [Extract] tries routes in, as a fresh
// slice a caller may hold onto and modify.
//
// It exists so that "the ordinary order" has a name outside this file: a
// harness configuration's ParseRoutes decodes into a slice built by repeated
// [Route.UnmarshalText], and the shipped configuration's three strings decode
// to exactly what this returns, but nothing enforces that arithmetic — this
// function is the one place both a caller and a test can name "the default"
// without retyping the three constants.
func DefaultRouteOrder() []Route {
	return append([]Route(nil), routeOrder[:]...)
}

// routeText is the wire form of each route. These strings reach the journal, so
// they are a compatibility surface: add to this map, never rename within it.
//
// A route added without an entry here fails wirename_internal_test.go, which
// derives the constant set from this file rather than from a hand-written list.
var routeText = map[Route]string{
	RouteUnknown:    "unknown",
	RouteNative:     "native",
	RouteFencedJSON: "fenced_json",
	RouteXMLTag:     "xml_tag",
}

// Valid reports whether r names a real extraction route.
func (r Route) Valid() bool {
	return r != RouteUnknown && routeText[r] != ""
}

// String returns the wire form of the route.
func (r Route) String() string {
	if s, ok := routeText[r]; ok {
		return s
	}
	return fmt.Sprintf("route(%d)", uint8(r))
}

// MarshalText encodes the route as its wire form.
//
// The journal serialises this, so it must not become an integer that a future
// reordering of the constants silently redefines.
func (r Route) MarshalText() ([]byte, error) {
	s, ok := routeText[r]
	if !ok {
		return nil, fmt.Errorf("parse: cannot marshal unknown route %d", uint8(r))
	}
	return []byte(s), nil
}

// UnmarshalText decodes the wire form produced by [Route.MarshalText].
func (r *Route) UnmarshalText(b []byte) error {
	for route, s := range routeText {
		if s == string(b) {
			*r = route
			return nil
		}
	}
	return fmt.Errorf("parse: unknown route %q", b)
}
