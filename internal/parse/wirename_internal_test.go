package parse

import (
	"encoding"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// This file is the completeness guard for every wire vocabulary this package
// owns — Route, Kind and ArgEncoding — and it is internal because routeText,
// kindText and argEncodingText are unexported.
//
// The problem is the same for all three, and it is not cosmetic. The engine
// writes these strings into journal payloads (ToolCallParsed.Route,
// .ArgEncoding, ToolCallFailed's classification) as plain strings, precisely so
// internal/parse and internal/journal share a vocabulary rather than a
// dependency edge — internal/arch/imports_test.go enforces that neither imports
// the other. Nothing in the compiler connects a constant to its wire name, so a
// constant added to any of these types without an entry in its text map still
// compiles, still passes every extraction test, and ships "route(4)" or
// "kind(15)" into the session record: a value that looks like a bug report
// rather than a measurement, on a field a bench query groups by.
//
// Until KAN-841 that risk was covered for ArgEncoding only (KAN-838), and Route
// and Kind were checked against hand-written lists in route_test.go — lists that
// a new constant does not appear in, written by the same person who forgot the
// text entry. The set is now derived from the source with go/ast, which is the
// shape cmd/kopicode/lineedit/key_internal_test.go settled on for the same
// problem.
//
// # What the tables below still assert
//
// A completeness check answers "is every constant covered", which is a different
// question from "is each mapping right". The tables keep the second: each row
// pins the exact wire string, so renaming one inside routeText or kindText fails
// here rather than silently making old sessions unreadable. Add to a table,
// never rename within one.
//
// # The positive control
//
// A traversal that quietly stops finding constants reports success having
// checked nothing — that has actually happened in this repo (KAN-837, where a
// deliberately broken AST walk left the main guard green). So the check runs in
// both directions: every constant the walk finds must have a row, *and* every
// row must name a constant the walk found. A broken walk fails the second, and
// the failure names each constant that went missing. declaredConstants also
// refuses outright when it finds nothing at all.

// wireConstant is what these three types have in common: a uint8-based
// enumeration whose wire form is a name, marshalled through encoding.TextMarshaler
// so no JSON encoding of it can render an integer a future reordering redefines.
type wireConstant interface {
	~uint8
	fmt.Stringer
	encoding.TextMarshaler
}

// wireName is one row of a guard table: the value a constant names, and the
// exact string it must serialise to.
type wireName[T wireConstant] struct {
	value T
	text  string
}

// routeWireNames is the wire vocabulary of [Route]. A constant missing from here
// fails TestRouteWireNames.
var routeWireNames = map[string]wireName[Route]{
	"RouteUnknown":    {RouteUnknown, "unknown"},
	"RouteNative":     {RouteNative, "native"},
	"RouteFencedJSON": {RouteFencedJSON, "fenced_json"},
	"RouteXMLTag":     {RouteXMLTag, "xml_tag"},
}

// kindWireNames is the wire vocabulary of [Kind], which the bench classifier
// reads to tell a harness failure from a model failure.
var kindWireNames = map[string]wireName[Kind]{
	"KindUnspecified":       {KindUnspecified, "unspecified"},
	"KindNoCall":            {KindNoCall, "no_call"},
	"KindUnlabelledFence":   {KindUnlabelledFence, "unlabelled_fence"},
	"KindUnfencedCall":      {KindUnfencedCall, "unfenced_call"},
	"KindUnclosedFence":     {KindUnclosedFence, "unclosed_fence"},
	"KindUnclosedTag":       {KindUnclosedTag, "unclosed_tag"},
	"KindEmptyBlock":        {KindEmptyBlock, "empty_block"},
	"KindInvalidJSON":       {KindInvalidJSON, "invalid_json"},
	"KindMissingName":       {KindMissingName, "missing_name"},
	"KindInvalidArguments":  {KindInvalidArguments, "invalid_arguments"},
	"KindAmbiguousCall":     {KindAmbiguousCall, "ambiguous_call"},
	"KindUnknownTool":       {KindUnknownTool, "unknown_tool"},
	"KindMissingArgument":   {KindMissingArgument, "missing_argument"},
	"KindWrongArgumentType": {KindWrongArgumentType, "wrong_argument_type"},
	"KindUnknownEnumValue":  {KindUnknownEnumValue, "unknown_enum_value"},
}

// argEncodingWireNames is the wire vocabulary of [ArgEncoding], which records a
// leniency the harness performed on the model's behalf.
var argEncodingWireNames = map[string]wireName[ArgEncoding]{
	"ArgsObject":     {ArgsObject, "object"},
	"ArgsJSONString": {ArgsJSONString, "json_string"},
}

func TestRouteWireNames(t *testing.T) {
	checkWireNames(t, "Route", "route.go", routeWireNames, routeText)
}

func TestKindWireNames(t *testing.T) {
	checkWireNames(t, "Kind", "errors.go", kindWireNames, kindText)
}

func TestArgEncodingWireNames(t *testing.T) {
	checkWireNames(t, "ArgEncoding", "parse.go", argEncodingWireNames, argEncodingText)
}

// TestOnlyRouteUnknownIsValid covers the one property that is Route's alone: the
// zero value never names a successful extraction, and every other route does.
// It runs over the guarded table, so a fourth route arrives here already
// covered.
func TestOnlyRouteUnknownIsValid(t *testing.T) {
	for _, name := range sortedKeys(routeWireNames) {
		row := routeWireNames[name]
		want := row.value != RouteUnknown
		if got := row.value.Valid(); got != want {
			t.Errorf("%s.Valid() = %v, want %v — the zero Route is deliberately "+
				"invalid so an extraction that does not say which route produced "+
				"it cannot exist", name, got, want)
		}
	}
}

// checkWireNames holds one enumeration to its wire vocabulary.
//
// It is generic over the three types rather than copied three times, because the
// three near-identical copies are what let ArgEncoding's guard land without
// Route's and Kind's. It stays a plain function taking a table and the package's
// own text map: three explicit calls above, no reflection, nothing to discover
// at 2am beyond the table you are reading.
func checkWireNames[T wireConstant, PT interface {
	*T
	encoding.TextUnmarshaler
}](t *testing.T, typeName, sourceFile string, table map[string]wireName[T], text map[T]string) {
	t.Helper()

	declared := declaredConstants(t, sourceFile, typeName)
	found := make(map[string]bool, len(declared))
	for _, name := range declared {
		found[name] = true
	}

	// The guard proper: a constant the source declares but the table does not
	// cover is a constant nothing below checks.
	for _, name := range declared {
		if _, ok := table[name]; !ok {
			t.Errorf("%s constant %s (declared in %s) has no row in the guard table — "+
				"add one with the wire name it must serialise to, and give it an entry "+
				"in the package's text map. Without both it reaches the journal as a "+
				"placeholder, on a field the bench groups by.", typeName, name, sourceFile)
		}
	}

	// The positive control. A traversal that stops finding constants makes the
	// loop above iterate nothing and pass; this direction fails instead, and
	// names every constant that went missing.
	for _, name := range sortedKeys(table) {
		if !found[name] {
			t.Errorf("the guard table has a row for %s, which the walk over %s did not "+
				"find as a %s constant. Either the constant was removed — a wire name is "+
				"a compatibility surface, so removing one needs a deliberate decision — "+
				"or declaredConstants is broken and this whole guard is asserting nothing.",
				name, sourceFile, typeName)
		}
	}

	seen := map[string]string{}
	for _, name := range declared {
		row, ok := table[name]
		if !ok {
			continue // already reported above
		}

		if got := row.value.String(); got != row.text {
			t.Errorf("%s.String() = %q, want %q — the engine writes String()'s result "+
				"into the journal, so a missing text entry ships a placeholder",
				name, got, row.text)
		}

		b, err := row.value.MarshalText()
		switch {
		case err != nil:
			t.Errorf("%s.MarshalText() = %v; it has no wire name", name, err)
		case string(b) != row.text:
			t.Errorf("%s marshals as %q but must be %q. These strings reach the journal "+
				"verbatim: add to the text map, never rename within it, or an old session "+
				"stops being readable.", name, b, row.text)
		}

		var back T
		if err := PT(&back).UnmarshalText([]byte(row.text)); err != nil {
			t.Errorf("%s does not decode from its own wire name %q: %v", name, row.text, err)
		} else if back != row.value {
			t.Errorf("%q decoded to %d, want %s (%d) — the round trip is what makes an "+
				"old session readable", row.text, uint8(back), name, uint8(row.value))
		}

		if other, dup := seen[row.text]; dup {
			t.Errorf("%s and %s share the wire name %q; a collision merges two findings "+
				"into one number without failing anything else", name, other, row.text)
		}
		seen[row.text] = name
	}

	if len(seen) != len(text) {
		t.Errorf("the %s text map has %d entries but the guard accounts for %d distinct "+
			"wire names. An entry with no constant is a name nothing can produce; a "+
			"constant with no entry ships a placeholder.", typeName, len(text), len(seen))
	}

	checkUnknownValueRefuses[T, PT](t, typeName, text)
}

// checkUnknownValueRefuses covers the value with no wire name at all — the state
// a constant added without a text entry actually lands in. String falls back to
// a placeholder so a debug print never panics; MarshalText must fail instead,
// because a value that serialises to "kind(15)" in a session record is worse
// than a record that failed to write, since it pools with real measurements. And
// the placeholder must never decode, or a round trip would launder it into a
// real constant.
func checkUnknownValueRefuses[T wireConstant, PT interface {
	*T
	encoding.TextUnmarshaler
}](t *testing.T, typeName string, text map[T]string) {
	t.Helper()

	unknown, ok := unnamedValue(text)
	if !ok {
		t.Fatalf("every one of the 256 %s values has a wire name, so this check "+
			"cannot run; it needs rewriting rather than deleting", typeName)
	}

	if b, err := unknown.MarshalText(); err == nil {
		t.Errorf("%s(%d).MarshalText() = %q with no wire name; it must refuse, so a "+
			"placeholder never reaches the journal", typeName, uint8(unknown), b)
	}

	placeholder := unknown.String()
	if placeholder == "" {
		t.Errorf("%s(%d).String() is empty; an unnamed value must be recognisable in a "+
			"log line, not blank", typeName, uint8(unknown))
	}

	var back T
	if err := PT(&back).UnmarshalText([]byte(placeholder)); err == nil {
		t.Errorf("%s decoded the placeholder %q into %d; a placeholder that round-trips "+
			"is indistinguishable from a real value", typeName, placeholder, uint8(back))
	}
}

// unnamedValue returns a value of T that the text map does not name. It searches
// downwards so it keeps working as constants are added at the bottom of an iota
// block.
func unnamedValue[T wireConstant](text map[T]string) (T, bool) {
	for i := 255; i >= 0; i-- {
		v := T(uint8(i))
		if _, named := text[v]; !named {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// declaredConstants parses sourceFile and returns the name of every constant
// declared with the named type.
//
// Parsing the source is the whole point: a hand-maintained list of constant
// names goes stale in exactly the way the text map does, and by the same hand.
func declaredConstants(t *testing.T, sourceFile, typeName string) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), sourceFile, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", sourceFile, err)
	}

	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST || len(gen.Specs) == 0 {
			continue
		}
		// The type is written on the first spec of an iota block and implied by
		// the rest, so the block is taken whole once its first spec names it.
		first, ok := gen.Specs[0].(*ast.ValueSpec)
		if !ok {
			continue
		}
		ident, ok := first.Type.(*ast.Ident)
		if !ok || ident.Name != typeName {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				if name.Name != "_" {
					names = append(names, name.Name)
				}
			}
		}
	}

	// The first half of the positive control. If the walk finds nothing —
	// the constants moved to another file, the block grew a form this does not
	// recognise — every assertion built on it would iterate an empty slice and
	// report success.
	if len(names) == 0 {
		t.Fatalf("found no %s constants in %s: the AST walk is broken, not the type. "+
			"Every check built on it would pass having asserted nothing.",
			typeName, sourceFile)
	}
	return names
}

// sortedKeys makes the failure output of a map-driven check deterministic, so
// two runs of a red suite are diffable.
func sortedKeys[T wireConstant](table map[string]wireName[T]) []string {
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
