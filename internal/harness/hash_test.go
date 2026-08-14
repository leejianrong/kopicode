package harness_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

// defaultConfig is the one configuration slice 1 registers, fetched rather than
// rebuilt so these tests hash the value the binary actually ships.
func defaultConfig(t *testing.T) harness.Config {
	t.Helper()
	cfg, ok := harness.ConfigByName(harness.DefaultConfigName)
	if !ok {
		t.Fatalf("harness.ConfigByName(%q) found nothing; the binary ships no default configuration",
			harness.DefaultConfigName)
	}
	return cfg
}

// TestDefaultConfigHashIsStable pins the shipped hash to a literal.
//
// This is the cross-process half of "deterministic". Computing the hash twice in
// one process proves nothing about a preimage built by ranging a map: Go's map
// iteration order is randomised per *process*, so both calls in one run can
// agree and every other run disagree. A literal committed to the repository was
// computed by a previous process, so a hash that moves between runs fails here.
//
// It is also the tripwire for an accidental preimage change. If this fails and
// no configuration value changed, the *encoding* changed, and
// harness.PreimageVersion has to move with it — otherwise two binaries write
// different hashes for the same arm and their results silently stop pooling.
func TestDefaultConfigHashIsStable(t *testing.T) {
	const want = "96502aa752d90c9109159e8651ce5174c2d2ca9985410b58083080fe8e06ceef"

	got := defaultConfig(t).Hash()
	if got != want {
		t.Errorf("default harness config hash = %s, want %s\n"+
			"if a configuration value changed this is expected and the literal moves with it;\n"+
			"if nothing changed, the preimage encoding did, and harness.PreimageVersion must be bumped",
			got, want)
	}
}

// TestHashCoversEveryConfigField is the guard that makes the hash falsifiable.
//
// A hash is worth exactly what its preimage covers. A field added to
// harness.Config and forgotten in Config.Hash produces a configuration that can
// change while the hash does not — two different arms pooling as one, which
// corrupts the project's core metric rather than merely breaking something.
//
// So the field list is derived from the struct by reflection rather than written
// out here: a hand-written table would have to be updated by the same person who
// forgot the preimage, at the same moment, which is not a check.
func TestHashCoversEveryConfigField(t *testing.T) {
	base := defaultConfig(t)

	// Positive control 1: the same configuration must hash the same, or every
	// "the hash changed" assertion below passes for the wrong reason.
	if base.Hash() != defaultConfig(t).Hash() {
		t.Fatalf("two copies of the same configuration hash differently (%s vs %s) — "+
			"the preimage is not deterministic and nothing below means anything",
			base.Hash(), defaultConfig(t).Hash())
	}

	paths := leafPaths(t, reflect.TypeOf(base), "")
	if len(paths) == 0 {
		t.Fatal("positive control failed: the field walk found no fields in harness.Config")
	}

	// Positive control 2: the walk must have recursed. Config holds Sampling
	// and Verification as nested structs, and a walk that stopped at the top
	// level would silently assert nothing about their fields.
	nested := false
	for _, p := range paths {
		if strings.Contains(p, ".") {
			nested = true
			break
		}
	}
	if !nested {
		t.Fatal("positive control failed: no nested field was reached, so the walk did not recurse " +
			"into Sampling or Verification and their fields are untested")
	}

	// Every top-level field of Config must be represented, so a field the walk
	// cannot reach at all is a failure rather than an absence.
	typ := reflect.TypeOf(base)
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		found := false
		for _, p := range paths {
			if p == name || strings.HasPrefix(p, name+".") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the field walk never reached harness.Config.%s", name)
		}
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			mutated := base
			target := fieldByPath(t, reflect.ValueOf(&mutated).Elem(), path)
			mutate(t, path, target)

			if reflect.DeepEqual(base, mutated) {
				t.Fatalf("mutating %s did not change the configuration; this test is asserting nothing", path)
			}
			if base.Hash() == mutated.Hash() {
				t.Errorf("changing %s left the harness config hash at %s\n"+
					"every field of harness.Config is in the hash preimage "+
					"(docs/adr/0007-model-selection-and-harness-config-shape.md decision 6):\n"+
					"a field outside it can change an arm's behaviour while two runs still pool as one",
					path, base.Hash())
			}
		})
	}
}

// TestConfigHoldsNoMap keeps the preimage out of reach of Go's randomised map
// iteration order.
//
// A map anywhere in harness.Config would tempt the encoder into ranging it, and
// a preimage that ranges a map hashes differently in every process — which does
// not fail loudly, it just means no two bench results ever pool. The structural
// version of that rule is "no map in the type", which is checkable.
func TestConfigHoldsNoMap(t *testing.T) {
	var seen int
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		seen++
		switch typ.Kind() {
		case reflect.Map:
			t.Errorf("harness.Config%s is a %s\n"+
				"no field of a harness configuration may be a map: Go randomises map iteration "+
				"order, so a preimage built from one hashes differently in every process and no "+
				"two results ever pool", path, typ)
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				walk(f.Type, path+"."+f.Name)
			}
		case reflect.Slice, reflect.Array, reflect.Pointer:
			walk(typ.Elem(), path+"[]")
		default:
		}
	}
	walk(reflect.TypeOf(defaultConfig(t)), "")

	if seen < 2 {
		t.Fatal("positive control failed: the type walk visited nothing")
	}
}

// leafPaths lists the dotted paths of every scalar-or-slice field reachable in
// typ, recursing into nested structs.
func leafPaths(t *testing.T, typ reflect.Type, prefix string) []string {
	t.Helper()

	var out []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		path := f.Name
		if prefix != "" {
			path = prefix + "." + f.Name
		}
		if f.Type.Kind() == reflect.Struct {
			out = append(out, leafPaths(t, f.Type, path)...)
			continue
		}
		out = append(out, path)
	}
	return out
}

// fieldByPath navigates a dotted path to an addressable field.
func fieldByPath(t *testing.T, v reflect.Value, path string) reflect.Value {
	t.Helper()

	for _, name := range strings.Split(path, ".") {
		v = v.FieldByName(name)
		if !v.IsValid() {
			t.Fatalf("no field %s on the path %s", name, path)
		}
	}
	return v
}

// mutate changes a field to a different value of the same type.
//
// It fails on a kind it does not know rather than skipping it, which is the
// whole point: a field of an unhandled type is a field this test cannot prove is
// in the preimage, and silently passing over it is how the guard would become
// decorative.
func mutate(t *testing.T, path string, v reflect.Value) {
	t.Helper()

	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "-mutated")
	case reflect.Int, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Float64:
		v.SetFloat(v.Float() + 1)
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Slice:
		if v.Type().Elem().Kind() != reflect.String {
			t.Fatalf("%s is a slice of %s, which this test cannot vary; teach it, or the field "+
				"may be outside the hash preimage and nothing would say so", path, v.Type().Elem())
		}
		v.Set(reflect.Append(v, reflect.ValueOf("mutated")))
	default:
		t.Fatalf("%s is a %s, which this test cannot vary; teach it, or the field may be "+
			"outside the hash preimage and nothing would say so", path, v.Kind())
	}
}
