package provider_test

import (
	"reflect"
	"testing"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
)

// The pin, the token counts and the sampling parameters are declared three
// times over in this repo: here, in internal/journal as the wire form the
// engine records, and in internal/provider/fixture as the shape recorded
// traffic carries. None of the three may import another — the journal is a
// compatibility surface that must not move when a producer is refactored, the
// fixtures are data that must not pull in a client, and this package must not
// import the journal because the engine journals and packages return data.
//
// So the coupling is held by test, the way internal/journal holds itself to
// internal/build. A field added to one and not the others fails here rather
// than being discovered when a journaled pin is missing the part that made it a
// pin.

// fieldShapes reduces a struct to name → json tag → kind, which is what the
// three declarations have to agree on. Go type identity is not the question:
// []string in one package is []string in another.
func fieldShapes(t *testing.T, v any) map[string]string {
	t.Helper()

	rt := reflect.TypeOf(v)
	if rt.Kind() != reflect.Struct {
		t.Fatalf("%T is not a struct", v)
	}
	out := map[string]string{}
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		out[f.Name] = tag + " " + f.Type.String()
	}
	if len(out) == 0 {
		t.Fatalf("%T has no exported fields; this check would pass over anything", v)
	}
	return out
}

func TestPinShapesAgreeAcrossThePackagesThatCannotImportEachOther(t *testing.T) {
	cases := []struct {
		name string
		of   []any
	}{
		{
			name: "the provider pin",
			of:   []any{provider.Pin{}, journal.ProviderPin{}, fixture.Pin{}},
		},
		{
			name: "the token counts",
			of:   []any{provider.Usage{}, journal.TokenCounts{}, fixture.Usage{}},
		},
		{
			// fixture has no sampling shape: a recording carries the reply, and
			// the sampling parameters were the request's.
			name: "the sampling parameters",
			of:   []any{provider.Sampling{}, journal.Sampling{}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := fieldShapes(t, tc.of[0])
			for _, other := range tc.of[1:] {
				got := fieldShapes(t, other)
				if !reflect.DeepEqual(want, got) {
					t.Errorf("%T is %v but %T is %v\n"+
						"these three are declared separately on purpose and held together by this test; "+
						"a field added to one belongs in all of them",
						tc.of[0], want, other, got)
				}
			}
		})
	}
}
