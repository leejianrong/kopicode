package harness_test

import (
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

// writeConfig puts a .kopicode/config.toml under dir and returns dir.
func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	cfgDir := filepath.Join(dir, harness.ConfigDirName)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", cfgDir, err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, harness.ConfigFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return dir
}

// TestPrecedence is ADR-0007 decision 2, driven rather than asserted: the flag
// beats the config file, the config file beats the built-in default, and the
// source each value came from is reported.
//
// The unregistered-model cases are deliberately absent — they are a usage error
// and live in their own test — so every row here is one that must resolve.
func TestPrecedence(t *testing.T) {
	const registered = harness.DefaultModelID

	cases := []struct {
		name       string
		file       string
		overrides  harness.Overrides
		wantModel  string
		wantSource harness.Source
	}{
		{
			name:       "nothing given falls to the built-in default",
			wantModel:  registered,
			wantSource: harness.SourceDefault,
		},
		{
			name:       "the config file beats the default",
			file:       "model = \"" + registered + "\"\n",
			wantModel:  registered,
			wantSource: harness.SourceFile,
		},
		{
			name:       "the flag beats the config file",
			file:       "model = \"qwen/not-a-model\"\n",
			overrides:  harness.Overrides{Model: registered},
			wantModel:  registered,
			wantSource: harness.SourceFlag,
		},
		{
			name:       "an empty flag value is not a value",
			file:       "model = \"" + registered + "\"\n",
			overrides:  harness.Overrides{Model: ""},
			wantModel:  registered,
			wantSource: harness.SourceFile,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file != "" {
				writeConfig(t, dir, tc.file)
			}

			sel, err := harness.Resolve(dir, tc.overrides)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if sel.ModelID != tc.wantModel {
				t.Errorf("model = %q, want %q", sel.ModelID, tc.wantModel)
			}
			if sel.ModelSource != tc.wantSource {
				t.Errorf("model source = %q, want %q", sel.ModelSource, tc.wantSource)
			}
		})
	}
}

// TestHarnessPrecedence is the same chain on the other axis. --harness defaults
// to whatever the model's registry row maps to, which is what makes a bench arm
// selectable per invocation rather than per build.
func TestHarnessPrecedence(t *testing.T) {
	entry, ok := harness.Lookup(harness.DefaultModelID)
	if !ok {
		t.Fatalf("the default model %q is not registered", harness.DefaultModelID)
	}

	cases := []struct {
		name       string
		file       string
		overrides  harness.Overrides
		wantSource harness.Source
	}{
		{"nothing given comes from the registry row", "", harness.Overrides{}, harness.SourceRegistry},
		{"the config file beats the row", "harness = \"" + entry.HarnessConfig + "\"\n", harness.Overrides{}, harness.SourceFile},
		{"the flag beats the config file", "harness = \"nope\"\n", harness.Overrides{Harness: entry.HarnessConfig}, harness.SourceFlag},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file != "" {
				writeConfig(t, dir, tc.file)
			}

			sel, err := harness.Resolve(dir, tc.overrides)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if sel.Config.Name != entry.HarnessConfig {
				t.Errorf("harness = %q, want %q", sel.Config.Name, entry.HarnessConfig)
			}
			if sel.HarnessSource != tc.wantSource {
				t.Errorf("harness source = %q, want %q", sel.HarnessSource, tc.wantSource)
			}
		})
	}
}

// TestNoEnvironmentVariableInTheChain holds ADR-0007 decision 3.
//
// The reasoning is provenance: a flag is in the shell history and the CI job
// log, a config key shows up in a diff, and an environment variable is ambient
// and invisible in both — which is the standard mechanism by which "I ran the
// same command and got a different number" happens. Adding one later is a
// one-line change nobody would think to argue about, which is exactly why it is
// checked rather than trusted.
//
// The names are every plausible spelling rather than the one the ADR names, so
// the guard does not become a check on a single string.
func TestNoEnvironmentVariableInTheChain(t *testing.T) {
	const other = "qwen/not-a-model"

	for _, name := range []string{
		"KOPICODE_MODEL", "KOPICODE_HARNESS", "MODEL", "HARNESS",
		"KOPICODE_MODEL_ID", "KOPI_MODEL",
	} {
		t.Setenv(name, other)
	}

	sel, err := harness.Resolve(t.TempDir(), harness.Overrides{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.ModelID != harness.DefaultModelID {
		t.Errorf("an environment variable changed the resolved model to %q\n"+
			"docs/adr/0007-model-selection-and-harness-config-shape.md decision 3: no environment "+
			"variable is in the chain, at any position", sel.ModelID)
	}
	if sel.ModelSource != harness.SourceDefault {
		t.Errorf("model source = %q, want %q", sel.ModelSource, harness.SourceDefault)
	}
}

// TestUnknownModelIsAUsageError is ADR-0007 decision 4's contract, minus the
// exit code, which is the front end's half and is driven in
// cmd/kopicode's integration test.
func TestUnknownModelIsAUsageError(t *testing.T) {
	const requested = "qwen/qwen3-coder-nxt"

	_, err := harness.Resolve(t.TempDir(), harness.Overrides{Model: requested})
	if err == nil {
		t.Fatal("resolving an unregistered model succeeded; an unknown id must be refused at " +
			"startup rather than deferred to the provider")
	}
	if !harness.IsUsageError(err) {
		t.Errorf("error is not a usage error, so a front end would exit 4 rather than 2: %v", err)
	}

	msg := err.Error()
	for _, want := range []string{requested, "--model", "supported models", harness.DefaultModelID} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}

	// A usage error must stay identifiable through wrapping, or a caller that
	// adds context turns an exit 2 into an exit 4.
	wrapped := errors.Join(errors.New("starting session"), err)
	if !harness.IsUsageError(wrapped) {
		t.Error("a wrapped usage error is no longer recognised as one")
	}
}

// TestUnknownHarnessIsAUsageError. --harness selects a *registered*
// configuration and nothing else: users do not author one (ADR-0007 decision 5).
func TestUnknownHarnessIsAUsageError(t *testing.T) {
	_, err := harness.Resolve(t.TempDir(), harness.Overrides{Harness: "aggressive"})
	if err == nil {
		t.Fatal("resolving an unregistered harness configuration succeeded")
	}
	if !harness.IsUsageError(err) {
		t.Errorf("error is not a usage error: %v", err)
	}
	for _, name := range harness.ConfigNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not list the available configuration %q:\n%s", name, err)
		}
	}
}

// TestRefusalWritesNothing is the ordering half of ADR-0007 decision 4, made
// observable.
//
// The decision is about *when* the refusal happens — before the system prompt is
// assembled, before the journal is opened, before SessionStarted is written — so
// checking the error alone would miss the point entirely. What a caller can
// observe is that a session which never started left no trace: no .kopicode
// directory, no journal, no lock, nothing.
//
// The positive control matters as much as the assertion. A tree comparison that
// cannot see a new file would pass over a journal being written.
func TestRefusalWritesNothing(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "model = \"qwen/not-a-model\"\n")

	before := treeOf(t, dir)

	if _, err := harness.Resolve(dir, harness.Overrides{}); err == nil {
		t.Fatal("resolving an unregistered model succeeded")
	}

	if after := treeOf(t, dir); !equalTrees(before, after) {
		t.Errorf("a refused session changed the working tree\nbefore: %v\nafter:  %v\n"+
			"failing before SessionStarted means no half-session record exists for a session that "+
			"never started (ADR-0007 decision 4)", before, after)
	}

	// Positive control: the comparison must be able to notice a file.
	if err := os.WriteFile(filepath.Join(dir, "control"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing the control file: %v", err)
	}
	if equalTrees(before, treeOf(t, dir)) {
		t.Fatal("positive control failed: the tree comparison did not notice a new file, so the " +
			"assertion above would not notice a journal either")
	}
}

// treeOf lists every path under dir, relative and sorted.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

func equalTrees(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBindDeclaresBothFlagsOnce keeps the two front ends from drifting.
//
// ADR-0007 decision 2 requires --model and --harness on both surfaces. They are
// bound from one place so that a rename or a changed default cannot land on one
// binary and not the other, and this checks the binding rather than the
// intention.
func TestBindDeclaresBothFlagsOnce(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := harness.Bind(fs)

	declared := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { declared[f.Name] = true })
	for _, name := range []string{harness.FlagModel, harness.FlagHarness} {
		if !declared[name] {
			t.Errorf("Bind did not declare --%s", name)
		}
	}

	if err := fs.Parse([]string{"--" + harness.FlagModel, "a/b", "--" + harness.FlagHarness, "c"}); err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if o.Model != "a/b" || o.Harness != "c" {
		t.Errorf("parsed overrides = %+v, want {Model:a/b Harness:c}", *o)
	}
}

// TestSelectionLogsNoCredential.
//
// OPENROUTER_API_KEY must never reach a log line (CLAUDE.md). A Selection has
// nowhere to put one, and LogValue is written out field by field rather than
// reflected, so this is a check on the shape staying that way rather than on the
// current fields.
func TestSelectionLogsNoCredential(t *testing.T) {
	sel, err := harness.Resolve(t.TempDir(), harness.Overrides{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	rendered := sel.LogValue().String()
	for _, banned := range []string{"key", "token", "secret", "authorization", "password"} {
		if strings.Contains(strings.ToLower(rendered), banned) {
			t.Errorf("the slog rendering of a Selection contains %q:\n%s", banned, rendered)
		}
	}
	for _, want := range []string{sel.ModelID, sel.HarnessConfigHash, sel.Config.Name} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the slog rendering omits %q, so --debug would not say which arm ran:\n%s",
				want, rendered)
		}
	}
}

// TestAConfiguredVerifyCommandChangesTheArm is the verification half of
// docs/SLICE-1.md §5, and the reason Verification.Source is in the hash preimage
// at all (ADR-0007 decision 6).
//
// A run whose verification command was discovered from a Makefile is not the
// same arm as one where the repository named it: the harness did a different
// thing in each case — it decided, or it obeyed — and results from the two must
// not pool. The command itself is deliberately *not* hashed: two repositories
// that each name their own command are still the same arm, because in both the
// harness ran what it was told.
func TestAConfiguredVerifyCommandChangesTheArm(t *testing.T) {
	discovered, err := harness.Resolve(t.TempDir(), harness.Overrides{})
	if err != nil {
		t.Fatalf("Resolve with no config file: %v", err)
	}
	if discovered.Config.Verification.Source != harness.VerificationDiscovered {
		t.Fatalf("with no config file the source is %q, want %q",
			discovered.Config.Verification.Source, harness.VerificationDiscovered)
	}
	if discovered.Verify != nil {
		t.Errorf("Selection.Verify = %v with no config file", discovered.Verify)
	}

	dir := writeConfig(t, t.TempDir(), "verify = [\"make\", \"test\"]\n")
	configured, err := harness.Resolve(dir, harness.Overrides{})
	if err != nil {
		t.Fatalf("Resolve with a verify command: %v", err)
	}

	if got := configured.Config.Verification.Source; got != harness.VerificationConfigured {
		t.Errorf("Verification.Source = %q, want %q", got, harness.VerificationConfigured)
	}
	if want := []string{"make", "test"}; !slices.Equal(configured.Verify, want) {
		t.Errorf("Selection.Verify = %v, want %v", configured.Verify, want)
	}
	if configured.HarnessConfigHash == discovered.HarnessConfigHash {
		t.Errorf("both arms hash to %s; a repository naming its own verification command has "+
			"changed what the harness does, and two runs that differ in it would pool as one",
			discovered.HarnessConfigHash)
	}
	if configured.HarnessConfigHash != configured.Config.Hash() {
		t.Errorf("Selection carries hash %s for a configuration that hashes to %s; an arm "+
			"identified by a value it is not cannot be compared with anything",
			configured.HarnessConfigHash, configured.Config.Hash())
	}

	// Two repositories that each name a command are one arm: what is hashed is
	// that the harness obeyed, not what it was told.
	other := writeConfig(t, t.TempDir(), "verify = [\"npm\", \"test\"]\n")
	otherSel, err := harness.Resolve(other, harness.Overrides{})
	if err != nil {
		t.Fatalf("Resolve with a different verify command: %v", err)
	}
	if otherSel.HarnessConfigHash != configured.HarnessConfigHash {
		t.Errorf("two repositories that each name their own command hash differently (%s vs %s); "+
			"the command is not in the preimage and must not be",
			otherSel.HarnessConfigHash, configured.HarnessConfigHash)
	}
}

// TestAnAgentsFileDoesNotChangeTheArm holds KAN-1024's decision 3 to account:
// a repository's own AGENTS.md is content the session's own bootstrap feeds
// to the model (internal/engine's loadProjectInstructions), never a fact
// [Resolve] reads or a value that enters [harness.Config.Hash]'s preimage.
// Two repositories differing only in whether — or how — they name their own
// project instructions are still the identical arm.
func TestAnAgentsFileDoesNotChangeTheArm(t *testing.T) {
	without, err := harness.Resolve(t.TempDir(), harness.Overrides{})
	if err != nil {
		t.Fatalf("Resolve with no AGENTS.md: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, harness.AgentsFileName), []byte("run make test first\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", harness.AgentsFileName, err)
	}
	with, err := harness.Resolve(dir, harness.Overrides{})
	if err != nil {
		t.Fatalf("Resolve with an AGENTS.md: %v", err)
	}

	if with.HarnessConfigHash != without.HarnessConfigHash {
		t.Errorf("HarnessConfigHash differs with an AGENTS.md present (%s) versus absent (%s); "+
			"AGENTS.md is repository content, not a harness configuration fact, and must not move the hash",
			with.HarnessConfigHash, without.HarnessConfigHash)
	}

	// Different content, same arm: what is hashed is the harness configuration
	// Resolve produced, not anything about the repository's own AGENTS.md.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, harness.AgentsFileName), []byte("a completely different set of instructions\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", harness.AgentsFileName, err)
	}
	otherSel, err := harness.Resolve(other, harness.Overrides{})
	if err != nil {
		t.Fatalf("Resolve with a different AGENTS.md: %v", err)
	}
	if otherSel.HarnessConfigHash != with.HarnessConfigHash {
		t.Errorf("two repositories with differently worded AGENTS.md files hash differently (%s vs %s); "+
			"AGENTS.md content is not in the preimage and must not be",
			otherSel.HarnessConfigHash, with.HarnessConfigHash)
	}
}
