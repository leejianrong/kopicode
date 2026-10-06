package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

var featureName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// TestFeatureListIsStableInShape: sorted, unique and made of names a client can
// compare byte for byte. The sort is what makes two builds with the same
// capabilities print the same bytes.
func TestFeatureListIsStableInShape(t *testing.T) {
	if !slices.IsSorted(features) {
		t.Errorf("features is not sorted: %v", features)
	}
	seen := map[string]bool{}
	for _, f := range features {
		if !featureName.MatchString(f) {
			t.Errorf("feature %q is not a lower-case dotted name", f)
		}
		if seen[f] {
			t.Errorf("feature %q is listed twice", f)
		}
		seen[f] = true
	}
}

// TestFeatureListTracksTheCode ties the list to the things it describes, so a
// consent mode or a method added without a feature name fails here instead of
// leaving a client unable to detect it.
func TestFeatureListTracksTheCode(t *testing.T) {
	for _, mode := range []string{consentModeRemoteInteractive, consentModeUnattendedPolicy, consentModeAuto} {
		if !slices.Contains(features, "consent_mode."+mode) {
			t.Errorf("consent_mode %q is accepted by session.start but has no feature name", mode)
		}
	}
	for method, feature := range map[string]string{methodSessionClose: "session.close", methodServerHello: "server.hello", methodAskRequest: "ask.request"} {
		if method != feature {
			t.Errorf("method %q and feature %q should share a name", method, feature)
		}
		if !slices.Contains(features, feature) {
			t.Errorf("method %q has no feature name", method)
		}
	}
}

func TestVersionJSONPrintsCapabilities(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version", "--json"}, &stdout, &stderr); code != exitSuccess {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	var got capabilities
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, stdout.String())
	}
	if got.Protocol != serveProtocol || !slices.Equal(got.Features, features) {
		t.Errorf("got protocol %d features %v, want %d %v", got.Protocol, got.Features, serveProtocol, features)
	}
	if got.Version == "" || got.TreeState == "" {
		t.Errorf("identity fields empty: %+v", got)
	}
}

func TestVersionWithoutJSONIsUnchanged(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != exitSuccess {
		t.Fatalf("exit %d", code)
	}
	if bytes.HasPrefix(stdout.Bytes(), []byte("{")) {
		t.Errorf("plain version printed JSON: %s", stdout.String())
	}
}

// TestServeHelloAnswersWithTheSameObject: a client can ask whichever it reaches
// first and read the same thing.
func TestServeHelloAnswersWithTheSameObject(t *testing.T) {
	h := startServe(t, engine.Options{})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodServerHello})
	resp := h.awaitResponse(1)
	if resp["error"] != nil {
		t.Fatalf("server.hello errored: %v", resp["error"])
	}
	raw, _ := json.Marshal(resp["result"])
	var got capabilities
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if want := currentCapabilities(); got.Protocol != want.Protocol || !slices.Equal(got.Features, want.Features) {
		t.Errorf("hello = %+v, want protocol %d features %v", got, want.Protocol, want.Features)
	}
	h.close()
}
