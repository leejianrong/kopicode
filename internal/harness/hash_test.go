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
	return configByName(t, harness.DefaultConfigName)
}

// configByName fetches a compiled-in configuration by name, fetched rather
// than rebuilt for the same reason [defaultConfig] is: these tests hash the
// value the binary actually ships, not a copy that could quietly diverge
// from it.
func configByName(t *testing.T, name string) harness.Config {
	t.Helper()
	cfg, ok := harness.ConfigByName(name)
	if !ok {
		t.Fatalf("harness.ConfigByName(%q) found nothing; the binary does not ship this configuration", name)
	}
	return cfg
}

// everyConfig fetches every compiled-in configuration by name, so a test that
// holds one to a discipline can hold all of them to it without a config
// added later going unchecked.
func everyConfig(t *testing.T) []harness.Config {
	t.Helper()
	names := harness.ConfigNames()
	if len(names) == 0 {
		t.Fatal("positive control failed: harness.ConfigNames() lists no configuration at all")
	}
	out := make([]harness.Config, 0, len(names))
	for _, name := range names {
		out = append(out, configByName(t, name))
	}
	return out
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
//
// The literal moved once, on KAN-843, when Config.SystemPrompt went from the
// empty placeholder to prompt_default.md. That is a configuration *value*
// changing and not an encoding change, so PreimageVersion stayed at 1 — and no
// result was invalidated by it, because no bench run had happened.
//
// It moved a second time on KAN-844, and that move *is* an encoding change:
// Config.ToolCatalogue and Config.AdvertiseNativeTools are new fields the
// preimage did not cover at all before (hash.go's hashWith gained the
// p.toolCatalogue and the advertise_native_tools lines), so PreimageVersion
// moved from 1 to 2 alongside it — the literal moving without the version
// moving would have meant an old and a new binary silently disagreeing about
// what the same configuration hashes to. Again, no bench run had happened
// under version 1's hash, so nothing pools incorrectly either side of the
// bump. A future move needs the same two sentences: which value or which
// coverage changed, and what it means for results recorded under the old hash.
//
// It moved a third time on KAN-946, when "ask" landed in ToolSet and
// ToolCatalogue. That is a configuration *value* changing — the tool set the
// model is presented, not a new field the preimage did not cover — so
// PreimageVersion stays at 2, the same category KAN-843's own move was. No
// bench run had happened under the prior hash either, so nothing pools
// incorrectly across this move.
//
// It moved a fourth time on KAN-960, when prompt_default.md's Verification
// section was rewritten to say a pass answers back with a one-line
// confirmation rather than staying silent — another prompt *value* change,
// not new preimage coverage, so PreimageVersion again stays where it was.
// One real bench run (z-ai/glm-5.2, 2026-08-20) had already happened under
// the prior hash; see docs/provider-pin.md for that arm's own record.
func TestDefaultConfigHashIsStable(t *testing.T) {
	const want = "34f95a0068f38695dfeb45ba4da3dbb4a756ea88a386abc5ebea15e1a3ace4ee"

	got := defaultConfig(t).Hash()
	if got != want {
		t.Errorf("default harness config hash = %s, want %s\n"+
			"if a configuration value changed this is expected and the literal moves with it;\n"+
			"if nothing changed, the preimage encoding did, and harness.PreimageVersion must be bumped",
			got, want)
	}
}

// TestMinimaxM2ConfigHashIsStable is [TestDefaultConfigHashIsStable]'s
// counterpart for [harness.MinimaxM2ConfigName] (KAN-942), pinned the same
// way and for the same reason: a hash that moved between two runs of this
// suite would mean the preimage is not actually deterministic, and nothing
// in the process that computed the literal below differs from the process
// that will compute it in CI or in a bench run.
//
// The literal is a fresh computation, not derived from
// TestDefaultConfigHashIsStable's — this configuration carries a different
// Sampling.SeedPolicy (see MinimaxM2ConfigName's own doc comment in
// harness.go), so the two hashes are expected to disagree, and
// TestNoTwoConfigsShareAHash below is what checks that expectation rather
// than this test silently passing if they happened to collide.
func TestMinimaxM2ConfigHashIsStable(t *testing.T) {
	const want = "c1169a045ee59785abf5ca6e6c19087125f7fcce8f3a08204de0c0b5428d32ef"

	got := configByName(t, harness.MinimaxM2ConfigName).Hash()
	if got != want {
		t.Errorf("minimax-m2-v1 harness config hash = %s, want %s\n"+
			"if a configuration value changed this is expected and the literal moves with it;\n"+
			"if nothing changed, the preimage encoding did, and harness.PreimageVersion must be bumped",
			got, want)
	}
}

// TestNoTwoConfigsShareAHash is the sibling check TestMinimaxM2ConfigHashIsStable
// names: two distinct registered configurations must hash differently, or
// ADR-0005's paired method would pool two arms that a report distinguishes
// by name.
//
// It is written against harness.ConfigNames() rather than the two configs
// this card happens to add, so a third configuration landing with the same
// values as an existing one (Config.Name itself is in the preimage — see
// hash.go — so this should be structurally impossible, but the whole point
// of a hash is not trusting a structural argument over a computed one) fails
// here rather than silently under-counting an experiment's arms.
func TestNoTwoConfigsShareAHash(t *testing.T) {
	seen := make(map[string]string)
	for _, cfg := range everyConfig(t) {
		h := cfg.Hash()
		if other, ok := seen[h]; ok {
			t.Errorf("configurations %q and %q both hash to %s", other, cfg.Name, h)
			continue
		}
		seen[h] = cfg.Name
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
//
// It runs over every registered configuration (harness.ConfigNames()) rather
// than the default alone, so a second configuration — [MinimaxM2ConfigName]
// as of KAN-942 — is held to the same "every field reaches the hash"
// discipline from the moment it exists, rather than by someone remembering
// to add a second test the day it is registered.
func TestHashCoversEveryConfigField(t *testing.T) {
	for _, name := range harness.ConfigNames() {
		t.Run(name, func(t *testing.T) {
			base := configByName(t, name)

			// Positive control 1: the same configuration must hash the same,
			// or every "the hash changed" assertion below passes for the
			// wrong reason.
			if base.Hash() != configByName(t, name).Hash() {
				t.Fatalf("two copies of the same configuration hash differently (%s vs %s) — "+
					"the preimage is not deterministic and nothing below means anything",
					base.Hash(), configByName(t, name).Hash())
			}

			paths := leafPaths(t, reflect.TypeOf(base), "")
			if len(paths) == 0 {
				t.Fatal("positive control failed: the field walk found no fields in harness.Config")
			}

			// Positive control 2: the walk must have recursed. Config holds
			// Sampling and Verification as nested structs, and a walk that
			// stopped at the top level would silently assert nothing about
			// their fields.
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

			// Every top-level field of Config must be represented, so a field
			// the walk cannot reach at all is a failure rather than an
			// absence.
			typ := reflect.TypeOf(base)
			for i := range typ.NumField() {
				fieldName := typ.Field(i).Name
				found := false
				for _, p := range paths {
					if p == fieldName || strings.HasPrefix(p, fieldName+".") {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("the field walk never reached harness.Config.%s", fieldName)
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
		})
	}
}

// # Is the reverse direction mechanisable? (KAN-855)
//
// KAN-855 found the defect this test cannot catch: Config.ParseRoutes was in
// every preimage above and read by nothing, because internal/parse.Extract had
// no order parameter at all. TestHashCoversEveryConfigField proves "every field
// reaches the hash"; it says nothing about "every field reaches behaviour", and
// that is the direction a forgotten wire actually fails in — the hash claims a
// distinction the binary does not honour, which is the worse of the two ways
// to be wrong (a stricter hash than reality, rather than a laxer one).
//
// Having now built the fix — internal/engine.decodeParseRoutes plus
// parse.NewRepairer's new order parameter, proven load-bearing by
// TestRouteOrderIsLoadBearing in internal/parse/repair_test.go — here is a
// concrete answer, not a deferred one.
//
// **A mutate-and-observe test symmetric with the one above is not feasible**,
// for a reason sharper than "harder": Hash() is one pure function every field
// provably passes through, because hashWith is a fixed sequence of p.* calls
// over the whole struct and the field-path walker only has to prove each
// field's bytes reach *that* function. "Behaviour" has no equivalent single
// function to mutate against. It is whatever internal/engine's loop, the
// journal it writes and the calls it dispatches end up doing over a whole
// session, and different fields change *different, incomparable* observables:
// MaxTurns changes a loop-iteration count, SystemPrompt changes the first
// message's bytes, ParseRoutes changes which Route an ambiguous reply resolves
// to, Sampling.Seed changes nothing at all unless the provider on the other end
// chooses to honour it. A generic walker would have to know, per field, which
// canned session and which assertion is sensitive to it — which is exactly the
// bespoke work TestRouteOrderIsLoadBearing did by hand (a message deliberately
// carrying both a native call and a fenced one, so reordering routes provably
// changes which tool gets dispatched). That is per-field judgement, not
// reflection, and no amount of struct-walking substitutes for it.
//
// It is also not merely a labour problem. A naive proxy for "reaches
// behaviour" — grep internal/engine's source for the literal field name — is
// unsound in both directions on fields this package already wires correctly.
// It has false negatives on Config itself: Sampling's fields reach the wire
// only through Config.RequestSampling, a method on this package, so
// internal/engine never spells "Sampling.Temperature" at all and a checker
// scoped to *reads the exact field name* would flag a correctly-wired field as
// unread. And it has no way to tell "read and used" from "read and its result
// silently discarded" — the shape of bug a name search cannot distinguish from
// KAN-855's actual defect. Recognising the difference needs a human to say
// what "reaches behaviour" even means for that one field, which is the
// judgement call this whole note is answering: yes, it is fundamentally a
// per-field call, and no sound, general, non-vacuous mechanisation of it
// exists that would not just be TestHashCoversEveryConfigField's walker
// wearing a new docstring.
//
// **What is mechanisable, and worth doing as a follow-on, is weaker and honest
// about being weaker.** This repository already has the pattern: not "prove
// the behaviour", but "prove someone claims it, by name" — the same shape as
// TestToolSetMatchesInternalTools (ToolSet held equal to internal/tools) and
// internal/engine/toolcatalogue_test.go (ToolCatalogue held equal to the real
// engine catalogue). The analogous guard here is a small, hand-maintained
// registry — one entry per Config field naming the test that is its
// behavioural evidence (ParseRoutes → parse_test.TestRouteOrderIsLoadBearing,
// MaxTurns → whichever engine test drives the turn cap, and so on) — checked by
// reflection for *completeness* (every field has an entry, the same walk this
// test already performs) but never for *correctness* (nothing can verify the
// named test actually exercises the field beyond a human having written it
// thinking about that field). That converts "a forgotten wire is invisible
// until someone reads the card queue and asks the question KAN-855 asked" into
// "a completeness test fails with the field's name in it until a human
// supplies the answer" — which is real leverage, and honestly short of a proof.
// It was not built as part of KAN-855: the registry Sampling's method-based
// wiring argues for is a second scoping decision (which package's source may a
// field's evidence live in?) that deserves its own card rather than riding in
// on this one's diff.

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
		switch v.Type().Elem().Kind() {
		case reflect.String:
			v.Set(reflect.Append(v, reflect.ValueOf("mutated")))
		case reflect.Struct:
			// Appending the zero value is enough: it changes the slice's
			// length, which changing anything requires (DeepEqual demands the
			// mutation be real, not merely re-encoded the same way) and, for
			// [Config.ToolCatalogue], the preimage's own ".len" prefix moves
			// with it (hash.go's toolCatalogue), so the hash assertion below
			// is not resting only on the length happening to coincide with a
			// content difference.
			v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
		default:
			t.Fatalf("%s is a slice of %s, which this test cannot vary; teach it, or the field "+
				"may be outside the hash preimage and nothing would say so", path, v.Type().Elem())
		}
	default:
		t.Fatalf("%s is a %s, which this test cannot vary; teach it, or the field may be "+
			"outside the hash preimage and nothing would say so", path, v.Kind())
	}
}
