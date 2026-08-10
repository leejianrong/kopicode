package journal_test

import (
	"reflect"
	"strings"
	"testing"

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
