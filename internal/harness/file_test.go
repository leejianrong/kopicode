package harness_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

// TestReadingTheConfigFile drives the flat-key reader.
//
// ADR-0007 decision 2 chose `model = "..."` at the top level over a `[model]`
// table because it is the smaller addition, and this is what "smaller" bought:
// a reader small enough to check line by line, and no TOML dependency in a
// project whose dependency budget is two.
//
// The refusals matter more than the acceptances. This file decides which model
// runs, so a line the reader cannot make sense of is a session that would be
// attributed to the wrong arm — it says so and stops, rather than guessing.
func TestReadingTheConfigFile(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		wantModel   string
		wantHarness string
		wantErr     string
	}{
		{
			name:      "a plain key",
			content:   "model = \"a/b\"\n",
			wantModel: "a/b",
		},
		{
			name:        "both keys, comments and blank lines",
			content:     "# the arm\n\nmodel = \"a/b\"   # why\nharness = 'default'\n",
			wantModel:   "a/b",
			wantHarness: "default",
		},
		{
			name:      "an unrelated key is skipped, value uninterpreted",
			content:   "somebody_elses_key = { deeply = [1, 2, 3] }\nmodel = \"a/b\"\n",
			wantModel: "a/b",
		},
		{
			name:      "keys under a table header are not top-level keys",
			content:   "model = \"a/b\"\n[experiment]\nmodel = \"c/d\"\n",
			wantModel: "a/b",
		},
		{
			name:      "no keys at all",
			content:   "# nothing here\n",
			wantModel: "",
		},
		{
			name:    "an unquoted value",
			content: "model = a/b\n",
			wantErr: "not a quoted string",
		},
		{
			name:    "an unterminated string",
			content: "model = \"a/b\n",
			wantErr: "missing its closing",
		},
		{
			name:    "trailing junk after the value",
			content: "model = \"a/b\" oops\n",
			wantErr: "trailing text",
		},
		{
			name:    "a line that is not a pair",
			content: "model\n",
			wantErr: "not a key = value line",
		},
		{
			name:    "the same key twice",
			content: "model = \"a/b\"\nmodel = \"c/d\"\n",
			wantErr: "set twice",
		},
		{
			name:    "a dotted key",
			content: "tool.model = \"a/b\"\n",
			wantErr: "not a bare key",
		},
		{
			name:    "an escape this reader does not decode",
			content: "model = \"a\\tb\"\n",
			wantErr: "backslash escape",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeConfig(t, t.TempDir(), tc.content)

			got, err := harness.LoadFileConfig(dir)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadFileConfig succeeded, want an error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				if !harness.IsUsageError(err) {
					t.Errorf("a malformed config file is a usage error (exit 2), not a harness "+
						"error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadFileConfig: %v", err)
			}
			if got.Model != tc.wantModel {
				t.Errorf("model = %q, want %q", got.Model, tc.wantModel)
			}
			if got.Harness != tc.wantHarness {
				t.Errorf("harness = %q, want %q", got.Harness, tc.wantHarness)
			}
			if got.Path == "" {
				t.Error("the loaded config does not say which file it came from, so a diagnostic " +
					"cannot name it")
			}
		})
	}
}

// TestReadingTheVerifyCommand drives the `verify` key: the repository naming its
// own forced-verification command (docs/SLICE-1.md §5).
//
// The refusals are the interesting half, and each one is a guess this reader
// declines to make. journal.VerificationRun records argv, so a command line
// accepted here would have to be split by something that is not the shell that
// would have run it — and the two disagree the first time a path holds a space.
func TestReadingTheVerifyCommand(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
		wantErr string
	}{
		{
			name:    "a plain argv",
			content: "verify = [\"make\", \"test\"]\n",
			want:    []string{"make", "test"},
		},
		{
			name:    "one element",
			content: "verify = ['./run-tests']\n",
			want:    []string{"./run-tests"},
		},
		{
			name:    "a trailing comma and a comment",
			content: "verify = [\"go\", \"test\", \"./...\",]  # the usual\n",
			want:    []string{"go", "test", "./..."},
		},
		{
			name:    "a comma inside an element",
			content: "verify = [\"go\", \"test\", \"-run\", \"A,B\"]\n",
			want:    []string{"go", "test", "-run", "A,B"},
		},
		{
			name:    "a bracket inside an element",
			content: "verify = [\"pytest\", \"-k\", \"test[1]\"]\n",
			want:    []string{"pytest", "-k", "test[1]"},
		},
		{
			name:    "unset",
			content: "model = \"a/b\"\n",
			want:    nil,
		},
		{
			name:    "a command line rather than argv",
			content: "verify = \"make test\"\n",
			wantErr: "is not an array",
		},
		{
			name:    "an empty array",
			content: "verify = []\n",
			wantErr: "is empty",
		},
		{
			name:    "an array that does not close on this line",
			content: "verify = [\"make\",\n  \"test\"]\n",
			wantErr: "does not close on this line",
		},
		{
			name:    "an unquoted element",
			content: "verify = [make, test]\n",
			wantErr: "not a quoted string",
		},
		{
			name:    "a hole in the middle",
			content: "verify = [\"make\", , \"test\"]\n",
			wantErr: "has no value",
		},
		{
			name:    "trailing junk after the array",
			content: "verify = [\"make\"] oops\n",
			wantErr: "trailing text",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeConfig(t, t.TempDir(), tc.content)

			got, err := harness.LoadFileConfig(dir)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadFileConfig succeeded, want an error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				if !harness.IsUsageError(err) {
					t.Errorf("a malformed config file is a usage error (exit 2), not a harness "+
						"error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadFileConfig: %v", err)
			}
			if !slices.Equal(got.Verify, tc.want) {
				t.Errorf("verify = %v, want %v", got.Verify, tc.want)
			}
		})
	}
}

// TestNoConfigFileIsNotAnError. Most repositories will never have one, and the
// absence resolves to the built-in default.
func TestNoConfigFileIsNotAnError(t *testing.T) {
	got, err := harness.LoadFileConfig(t.TempDir())
	if err != nil {
		t.Fatalf("LoadFileConfig on a directory with no config file: %v", err)
	}
	if got.Path != "" || got.Model != "" || got.Harness != "" {
		t.Errorf("absent config file produced %+v, want the zero value", got)
	}
}

// TestConfigIsFoundFromASubdirectory. A session started three directories down
// is still that repository's session, and the model it declared still applies.
func TestConfigIsFoundFromASubdirectory(t *testing.T) {
	root := writeConfig(t, t.TempDir(), "model = \"a/b\"\n")
	deep := filepath.Join(root, "internal", "engine")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("creating %s: %v", deep, err)
	}

	got, err := harness.LoadFileConfig(deep)
	if err != nil {
		t.Fatalf("LoadFileConfig: %v", err)
	}
	if got.Model != "a/b" {
		t.Errorf("model = %q, want %q — the upward search did not reach the repository root",
			got.Model, "a/b")
	}
}

// TestTheSearchStopsAtTheRepositoryBoundary.
//
// A checkout nested inside another project must not inherit the outer project's
// model. That would be a session silently attributed to an arm the repository
// never declared, which is the kind of provenance failure ADR-0007 decision 3
// rejects an environment variable over.
func TestTheSearchStopsAtTheRepositoryBoundary(t *testing.T) {
	outer := writeConfig(t, t.TempDir(), "model = \"outer/model\"\n")

	inner := filepath.Join(outer, "vendored")
	if err := os.MkdirAll(filepath.Join(inner, ".git"), 0o755); err != nil {
		t.Fatalf("creating the inner repository: %v", err)
	}

	got, err := harness.LoadFileConfig(inner)
	if err != nil {
		t.Fatalf("LoadFileConfig: %v", err)
	}
	if got.Model != "" {
		t.Errorf("the search escaped the inner repository and read %q from %s", got.Model, got.Path)
	}

	// Positive control: without the boundary the outer file is exactly what the
	// search would have found, so the assertion above is about the boundary and
	// not about the file being unreachable.
	sibling := filepath.Join(outer, "plain")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("creating %s: %v", sibling, err)
	}
	control, err := harness.LoadFileConfig(sibling)
	if err != nil {
		t.Fatalf("LoadFileConfig: %v", err)
	}
	if control.Model != "outer/model" {
		t.Fatalf("positive control failed: the upward search did not find the outer config from a "+
			"plain subdirectory either (got %q), so the test above proves nothing", control.Model)
	}
}
