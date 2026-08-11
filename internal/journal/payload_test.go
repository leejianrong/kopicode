package journal_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/build"
	"github.com/leejianrong/kopicode/internal/journal"
)

// forbidden are substrings no payload field name may contain. The API key must
// be structurally unable to reach the journal: not scrubbed on the way in, not
// redacted on the way out, but with nowhere to go.
//
// "token" is absent from this list on purpose — token *counts* are the point of
// ProviderRequest — so the names here are the ones that only ever describe a
// credential.
var forbidden = []string{
	"apikey", "api_key",
	"secret", "credential", "password", "passwd",
	"authorization", "bearer",
	"header", // where the key actually travels
	"env",    // an environment dump carries it wholesale
}

// TestNoPayloadFieldCanHoldACredential walks every field of every registered
// payload, including nested structs.
//
// It is a name check, which is a weak guarantee on its own; the field-shape
// check below is the load-bearing half.
func TestNoPayloadFieldCanHoldACredential(t *testing.T) {
	for typ, payload := range fixtures() {
		walkFields(t, reflect.TypeOf(payload), string(typ), func(path string, f reflect.StructField) {
			name := strings.ToLower(f.Name)
			for _, bad := range forbidden {
				if strings.Contains(name, bad) {
					t.Errorf("%s is named for a credential — the journal has no business holding one", path)
				}
			}
		})
	}
}

// TestNoPayloadFieldIsAFreeFormBag is the check that actually holds the line.
//
// A map[string]string or a map[string]any is how request headers, an
// environment or a config dump enters a record, and OPENROUTER_API_KEY lives in
// all three. Refusing the shape refuses the whole category, where a name
// denylist only refuses the spellings someone thought of.
//
// json.RawMessage is exempt: it is a []byte, it holds tool arguments the model
// wrote, and the model has never seen the key.
func TestNoPayloadFieldIsAFreeFormBag(t *testing.T) {
	for typ, payload := range fixtures() {
		walkFields(t, reflect.TypeOf(payload), string(typ), func(path string, f reflect.StructField) {
			if f.Type.Kind() == reflect.Map {
				t.Errorf("%s is a %s — a free-form map is how a credential reaches a record; "+
					"name the fields you mean to keep", path, f.Type)
			}
		})
	}
}

// walkFields visits every exported field of t and of any struct it contains.
func walkFields(t *testing.T, typ reflect.Type, path string, visit func(string, reflect.StructField)) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		fieldPath := path + "." + f.Name
		visit(fieldPath, f)

		ft := f.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
			if ft == reflect.TypeOf([]byte(nil)) {
				break
			}
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft.PkgPath() != "time" {
			walkFields(t, ft, fieldPath, visit)
		}
	}
}

// TestBuildInfoCanHoldEverythingTheBuildPackageProduces keeps the two
// declarations of a build identity from drifting apart.
//
// journal.BuildInfo is deliberately not build.Info: the journal is a
// compatibility surface and must not change shape because a producer package
// was refactored (the same reason EditApplied.AnchorVersion is a string rather
// than an anchor.Version). The cost of that decoupling is that a field added to
// build.Info would simply never be journaled, and nothing would say so — the
// failure would be a missing fact in a record nobody notices is incomplete.
//
// So the check is directional rather than an equality: every field
// internal/build produces must have somewhere to land here. The journal may
// carry more than the producer does — a field kept for records written by an
// older build is exactly what a compatibility surface is for.
//
// This is a test-only import. internal/journal itself imports nothing from this
// module, and that stays true.
func TestBuildInfoCanHoldEverythingTheBuildPackageProduces(t *testing.T) {
	wire := reflect.TypeOf(journal.BuildInfo{})
	produced := reflect.TypeOf(build.Info{})

	for i := range produced.NumField() {
		f := produced.Field(i)
		if !f.IsExported() {
			continue
		}
		w, ok := wire.FieldByName(f.Name)
		if !ok {
			t.Errorf("build.Info.%s has no journal.BuildInfo field — a build fact that "+
				"never reaches the record is a fact nobody can act on", f.Name)
			continue
		}
		if w.Type.Kind() != f.Type.Kind() {
			t.Errorf("journal.BuildInfo.%s is a %s but build.Info.%s is a %s",
				f.Name, w.Type.Kind(), f.Name, f.Type.Kind())
		}
		if w.Tag.Get("json") == "" {
			t.Errorf("journal.BuildInfo.%s has no json tag — the wire name would be "+
				"the Go name, which is not the contract", f.Name)
		}
	}
}

// TestTextCarriesTheFullSize: a reader must be able to tell what it is holding
// without fetching the blob, and a spilled value must not look empty.
func TestTextCarriesTheFullSize(t *testing.T) {
	inline := journal.InlineText("four")
	if inline.Size != 4 || inline.Spilled() {
		t.Errorf("InlineText: %+v", inline)
	}

	spilled := journal.BlobText("e3b0c442", 1<<20)
	if !spilled.Spilled() || spilled.Size != 1<<20 || spilled.Inline != "" {
		t.Errorf("BlobText: %+v", spilled)
	}
}
