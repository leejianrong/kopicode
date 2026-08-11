package fixture_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/parse"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
)

// This file is the guard on a package whose whole content is data, so it is
// written to fail loudly on the two ways a data package rots.
//
// The first is a fixture nobody checks. Every test here discovers fixtures by
// walking the directory rather than from a list in code, and
// TestDiscoveryFindsEveryFileOnDisk holds that walk to the filesystem, so a
// file added to data/ is validated by everything below without anyone
// remembering to register it — and a file the embed directive stops matching
// fails rather than disappearing.
//
// The second is a check that walks nothing and reports green. This repo has
// caught that three times. Every test that iterates fixtures counts what it
// iterated and fails on zero, and TestDiscoveryFindsEveryFileOnDisk fails if
// the count ever drops below what the directory holds.

// minFixtures is the floor the data directory must not fall below.
//
// Three, because the extractor has three routes and the primary test seam is
// supposed to exercise all of them (docs/SLICE-1.md §3). It is a floor and not
// an equality: adding a fixture must not require editing this file.
const minFixtures = 3

func TestDiscoveryFindsEveryFileOnDisk(t *testing.T) {
	embedded, err := fixture.Names(fixture.FS())
	if err != nil {
		t.Fatalf("listing embedded fixtures: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join("data"))
	if err != nil {
		t.Fatalf("reading the data directory: %v", err)
	}
	var onDisk []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		onDisk = append(onDisk, strings.TrimSuffix(e.Name(), ".json"))
	}
	slices.Sort(onDisk)

	if len(onDisk) < minFixtures {
		t.Fatalf("data/ holds %d fixtures, want at least %d — one per extraction route, so a route "+
			"is not left untested by the primary seam", len(onDisk), minFixtures)
	}
	if !slices.Equal(embedded, onDisk) {
		t.Fatalf("the embedded fixtures are %v but data/ holds %v\n"+
			"every test in this package walks the embedded set; a file the embed directive does not "+
			"match is a fixture that silently goes unvalidated",
			embedded, onDisk)
	}
	t.Logf("validating %d fixtures: %v", len(onDisk), onDisk)
}

// loadAll is the entry point every test below shares. It fails the test rather
// than returning an error, and it refuses an empty set.
func loadAll(t *testing.T) []fixture.Fixture {
	t.Helper()
	all, err := fixture.LoadAll(fixture.FS())
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	if len(all) < minFixtures {
		t.Fatalf("loaded %d fixtures, want at least %d — a check that walks nothing reports green",
			len(all), minFixtures)
	}
	return all
}

func TestEveryFixtureLoadsAndValidates(t *testing.T) {
	for _, f := range loadAll(t) {
		t.Run(f.Name, func(t *testing.T) {
			// Load already validated; re-running it here is what makes the
			// subtest name the fixture that failed rather than the whole set.
			if err := fixture.Validate(f); err != nil {
				t.Fatalf("validating: %v", err)
			}
			if len(f.Exchanges) == 0 {
				t.Fatal("no exchanges")
			}
		})
	}
}

// TestEveryFixtureIsMarkedWithItsOrigin is the honesty constraint, mechanised.
//
// Every fixture in the tree today is hand-authored, and the danger of that is
// stated at length in the package doc: a synthetic body that does not match
// what OpenRouter sends produces a green suite over a loop that would fail
// live. The one defence that survives contact with a future reader is that the
// synthetic ones say so in the file, so KAN-774's recorder can replace them and
// nobody quotes one as evidence about the provider.
func TestEveryFixtureIsMarkedWithItsOrigin(t *testing.T) {
	synthetic := 0
	for _, f := range loadAll(t) {
		t.Run(f.Name, func(t *testing.T) {
			if !f.Origin.Valid() {
				t.Fatalf("origin is %q, which names nothing", f.Origin)
			}
			if !f.Origin.Synthetic() {
				return
			}
			synthetic++
			if !strings.Contains(f.OriginNote, "hand") {
				t.Errorf("origin_note does not say the fixture was written by hand:\n%s", f.OriginNote)
			}
			if !f.Pin.Provisional() {
				t.Errorf("pin %v is not marked provisional; KAN-775 chooses the real slug and "+
					"quantization, and a hand-authored fixture carrying a plausible one presents a "+
					"decision this project has not made", f.Pin.Order)
			}
		})
	}
	t.Logf("%d hand-authored fixtures; replace each with a recording (KAN-774) rather than keeping both", synthetic)
}

// TestEveryExtractionRouteHasAFixture is the completeness check.
//
// It enumerates parse.Route rather than listing the routes here, so a fourth
// route added to the extractor fails this suite until a fixture drives it —
// the same shape as internal/tools/cancel_test.go's reflective check over the
// tool set. A hand-written list would have gone stale silently instead.
func TestEveryExtractionRouteHasAFixture(t *testing.T) {
	covered := map[string][]string{}
	for _, f := range loadAll(t) {
		for _, ex := range f.Exchanges {
			if ex.Expect.Route == "" {
				continue
			}
			covered[ex.Expect.Route] = append(covered[ex.Expect.Route], f.Name)
		}
	}

	routes := 0
	// parse.Route is a uint8, so this enumerates the whole space rather than a
	// list that can drift from the constants.
	for i := 0; i <= 255; i++ {
		r := parse.Route(uint8(i))
		if !r.Valid() {
			continue
		}
		routes++
		if len(covered[r.String()]) == 0 {
			t.Errorf("no fixture exercises parse route %q\n"+
				"which route a model reaches for is a finding (docs/SLICE-1.md §3), and a route with "+
				"no fixture is one the engine's primary test seam never drives",
				r.String())
		}
	}
	if routes == 0 {
		t.Fatal("enumerated no valid parse routes; this check would pass over anything")
	}
	t.Logf("%d extraction routes; fixtures cover %v", routes, covered)
}

// TestDeclaredRoutesMatchTheExtractor runs the real extractor over each
// fixture's assembled reply.
//
// This is what stops expect.route from being a label. A fixture claiming the
// XML route whose content the extractor reads as prose would otherwise satisfy
// every other check in the package while driving nothing.
func TestDeclaredRoutesMatchTheExtractor(t *testing.T) {
	checked := 0
	for _, f := range loadAll(t) {
		for i, ex := range f.Exchanges {
			msg := messageFrom(t, f.Name, ex)

			ext, err := parse.Extract(msg)
			if ex.Expect.Route == "" {
				if err == nil {
					t.Errorf("%s exchange %d: expect.route is empty but the extractor found %d calls "+
						"via %s", f.Name, i, len(ext.Calls()), ext.Route())
				} else if !errors.Is(err, parse.ErrNoToolCall) {
					t.Errorf("%s exchange %d: expect.route is empty but extraction failed with a real "+
						"error rather than a no-call: %v", f.Name, i, err)
				}
				checked++
				continue
			}

			if err != nil {
				t.Errorf("%s exchange %d: expect.route is %q but extraction failed: %v",
					f.Name, i, ex.Expect.Route, err)
				continue
			}
			if got := ext.Route().String(); got != ex.Expect.Route {
				t.Errorf("%s exchange %d: extraction took route %q, fixture declares %q",
					f.Name, i, got, ex.Expect.Route)
			}
			var names []string
			for _, c := range ext.Calls() {
				names = append(names, c.Name)
			}
			if !slices.Equal(names, ex.Expect.Tools) {
				t.Errorf("%s exchange %d: extraction found tools %v, fixture declares %v",
					f.Name, i, names, ex.Expect.Tools)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("checked no exchanges")
	}
	t.Logf("ran the extractor over %d exchanges", checked)
}

// TestEveryFixtureDrivesAtLeastTwoTurns is the card's acceptance bar: enough
// fixtures to drive a two-turn session end to end. A single canned reply is not
// a session — the shape that matters is reply, tool call, result, reply, stop.
func TestEveryFixtureDrivesAtLeastTwoTurns(t *testing.T) {
	for _, f := range loadAll(t) {
		t.Run(f.Name, func(t *testing.T) {
			turns := map[int]bool{}
			for _, ex := range f.Exchanges {
				turns[ex.Turn] = true
			}
			if len(turns) < 2 {
				t.Fatalf("covers %d turn(s); the acceptance bar is a two-turn session", len(turns))
			}
			first, last := f.Exchanges[0], f.Exchanges[len(f.Exchanges)-1]
			if len(first.Expect.Tools) == 0 {
				t.Error("the first reply calls no tool, so nothing goes back to the model and the " +
					"second turn has no reason to exist")
			}
			if last.Expect.FinishReason != "stop" {
				t.Errorf("the last reply finishes with %q; the loop needs a terminal reason to stop on",
					last.Expect.FinishReason)
			}
			if len(last.Expect.Tools) != 0 {
				t.Errorf("the last reply still calls %v", last.Expect.Tools)
			}
		})
	}
}

// TestNoFixtureCarriesACredentialShapedValue.
//
// The recorder scrubs at write time through a header allowlist, and these
// fixtures carry no request headers at all, so there should be nothing to find.
// This is the check that says so rather than assuming it — and it also catches
// somebody pasting a real capture into data/ on the way to replacing a
// hand-authored file.
func TestNoFixtureCarriesACredentialShapedValue(t *testing.T) {
	needles := []string{"authorization", "bearer ", "sk-or-", "api_key", "apikey", "openrouter_api_key"}

	names, err := fixture.Names(fixture.FS())
	if err != nil {
		t.Fatalf("listing fixtures: %v", err)
	}
	if len(names) < minFixtures {
		t.Fatalf("scanned %d fixtures, want at least %d", len(names), minFixtures)
	}
	for _, name := range names {
		raw, err := fs.ReadFile(fixture.FS(), name+".json")
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		lower := strings.ToLower(string(raw))
		for _, needle := range needles {
			if strings.Contains(lower, needle) {
				t.Errorf("%s contains %q — the API key must appear in no fixture, and a fixture "+
					"carrying a credential-shaped placeholder is one gitleaks will trip on later",
					name, needle)
			}
		}
	}
}

// TestHandAuthoredStreamsModelTheKeepAliveComment.
//
// OpenRouter sends `: OPENROUTER PROCESSING` comment lines while it waits, and
// a client that feeds one to a JSON decoder loses the stream. It is precisely
// the framing detail a hand-written fixture omits and a live run then
// discovers, so every synthetic stream here carries one.
//
// This is a rule about *hand-authored* fixtures and not a validation rule: a
// real recording from a provider that answered immediately would carry no
// keep-alive, and rejecting it would be rejecting the truth. It is checked per
// fixture rather than per exchange for the same reason — the shipped set
// deliberately mixes exchanges that carry one with exchanges that do not, so a
// client cannot come to depend on either.
func TestHandAuthoredStreamsModelTheKeepAliveComment(t *testing.T) {
	checked := 0
	for _, f := range loadAll(t) {
		if !f.Origin.Synthetic() {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			streamed, withKeepAlive := 0, 0
			for _, ex := range f.Exchanges {
				if !ex.Response.Streamed() {
					continue
				}
				streamed++
				if slices.Contains(ex.Response.Stream, fixture.KeepAliveComment) {
					withKeepAlive++
				}
			}
			if streamed == 0 {
				t.Fatal("no streamed exchanges; this fixture cannot drive the real client's SSE path")
			}
			if withKeepAlive == 0 {
				t.Fatalf("none of the %d streamed exchanges carries a %q line; a client that hands "+
					"that comment to a JSON decoder drops the stream, and a fixture set that never "+
					"sends one lets that bug through", streamed, fixture.KeepAliveComment)
			}
		})
		checked++
	}
	if checked == 0 {
		t.Fatal("no hand-authored fixtures found; this check would pass over anything")
	}
}

func TestLoadRejectsAMissingFixture(t *testing.T) {
	if _, err := fixture.Load(fixture.FS(), "no_such_fixture"); !errors.Is(err, fixture.ErrNotFound) {
		t.Fatalf("Load of a missing fixture returned %v, want ErrNotFound", err)
	}
	if _, err := fixture.Load(fixture.FS(), "../secrets"); !errors.Is(err, fixture.ErrNotFound) {
		t.Fatalf("Load of a path-shaped name returned %v, want ErrNotFound", err)
	}
}

// messageFrom maps a fixture's assembled body onto the shape the extractor
// consumes.
//
// This mapping is the provider client's job (KAN-776) and this is deliberately
// not it: it is the minimum a test needs, it lives in a test file, and it is
// not exported. If KAN-773 or KAN-776 wants a real one, it belongs in the
// provider package where the wire format is owned — not copied out of here.
func messageFrom(t *testing.T, name string, ex fixture.Exchange) parse.Message {
	t.Helper()

	var body struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(ex.Response.Body, &body); err != nil {
		t.Fatalf("%s: decoding body: %v", name, err)
	}
	if len(body.Choices) != 1 {
		t.Fatalf("%s: body carries %d choices, want 1", name, len(body.Choices))
	}

	m := body.Choices[0].Message
	msg := parse.Message{}
	if m.Content != nil {
		msg.Content = *m.Content
	}
	for _, tc := range m.ToolCalls {
		// The wire carries arguments as a JSON string holding JSON, so the
		// value handed to the extractor is that string as a JSON value —
		// which is exactly what parse.ArgsJSONString exists to record.
		encoded, err := json.Marshal(tc.Function.Arguments)
		if err != nil {
			t.Fatalf("%s: re-encoding arguments: %v", name, err)
		}
		msg.ToolCalls = append(msg.ToolCalls, parse.NativeCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: encoded,
		})
	}
	return msg
}
