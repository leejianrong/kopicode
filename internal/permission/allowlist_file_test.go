package permission_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/permission"
)

// writeAllowlistFile writes content to a fresh file under dir and returns its
// path.
func writeAllowlistFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "policy.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// TestReadingTheAllowlistFile drives the flat-key reader end to end: the
// grammar it accepts, and — more load-bearing — every line it refuses rather
// than guesses at, mirroring internal/harness's own test rigor for its
// (structurally identical) config reader.
func TestReadingTheAllowlistFile(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		wantRoot  string
		wantAllow [][]string
		wantErr   string
	}{
		{
			name:      "root and a two-command allowlist",
			content:   `root = "/repo"` + "\n" + `allow = [["go", "test", "./..."], ["make", "test"]]` + "\n",
			wantRoot:  "/repo",
			wantAllow: [][]string{{"go", "test", "./..."}, {"make", "test"}},
		},
		{
			name:      "comments and blank lines",
			content:   "# a declared allowlist\n\nroot = \"/repo\"   # confinement root\nallow = []\n",
			wantRoot:  "/repo",
			wantAllow: [][]string{},
		},
		{
			name:      "an argv holding a comma inside a string",
			content:   `root = "/repo"` + "\n" + `allow = [["go", "test", "-run", "A,B"]]` + "\n",
			wantRoot:  "/repo",
			wantAllow: [][]string{{"go", "test", "-run", "A,B"}},
		},
		{
			name:      "empty allow list, explicitly no shell ever",
			content:   `root = "/repo"` + "\n" + "allow = []\n",
			wantRoot:  "/repo",
			wantAllow: [][]string{},
		},
		{
			name:    "missing root",
			content: "allow = []\n",
			wantErr: `missing required key "root"`,
		},
		{
			name:    "missing allow",
			content: `root = "/repo"` + "\n",
			wantErr: `missing required key "allow"`,
		},
		{
			name:    "a relative root",
			content: `root = "repo"` + "\n" + "allow = []\n",
			wantErr: "not an absolute path",
		},
		{
			name:    "an empty root",
			content: `root = ""` + "\n" + "allow = []\n",
			wantErr: "empty string",
		},
		{
			name:    "root set twice",
			content: `root = "/repo"` + "\n" + `root = "/other"` + "\n" + "allow = []\n",
			wantErr: "set twice",
		},
		{
			name:    "allow set twice",
			content: `root = "/repo"` + "\n" + "allow = []\nallow = []\n",
			wantErr: "set twice",
		},
		{
			name:    "a table header is refused, not silently skipped",
			content: `root = "/repo"` + "\n" + "[allow]\nx = 1\n",
			wantErr: "table headers are not supported",
		},
		{
			name:    "an unrecognised key",
			content: `root = "/repo"` + "\n" + `verify = ["make", "test"]` + "\n" + "allow = []\n",
			wantErr: "not a key this file recognizes",
		},
		{
			name:    "a dotted key",
			content: `tool.root = "/repo"` + "\n",
			wantErr: "not a bare key",
		},
		{
			name:    "allow is not an array",
			content: `root = "/repo"` + "\n" + `allow = "go test"` + "\n",
			wantErr: "not an array",
		},
		{
			name:    "an allow element that is a bare string, not its own argv array",
			content: `root = "/repo"` + "\n" + `allow = ["go test"]` + "\n",
			wantErr: "is not an argv array",
		},
		{
			name:    "an allow element with an empty argv",
			content: `root = "/repo"` + "\n" + "allow = [[]]\n",
			wantErr: "empty argv",
		},
		{
			name:    "allow does not close on this line",
			content: `root = "/repo"` + "\n" + `allow = [["go", "test"]` + "\n",
			wantErr: "does not close on this line",
		},
		{
			name:    "trailing junk after allow's closing bracket",
			content: `root = "/repo"` + "\n" + `allow = [["go", "test"]] oops` + "\n",
			wantErr: "trailing text",
		},
		{
			name:    "an unquoted root value",
			content: "root = /repo\n" + "allow = []\n",
			wantErr: "not a quoted string",
		},
		{
			name:    "a line that is not a pair",
			content: "root\n",
			wantErr: "not a key = value line",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeAllowlistFile(t, t.TempDir(), tc.content)
			got, err := permission.LoadAllowlistFile(path)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadAllowlistFile succeeded, want an error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadAllowlistFile failed: %v", err)
			}
			if got.Root != tc.wantRoot {
				t.Errorf("Root = %q, want %q", got.Root, tc.wantRoot)
			}
			if len(got.Allow) != len(tc.wantAllow) {
				t.Fatalf("Allow = %v, want %v", got.Allow, tc.wantAllow)
			}
			for i := range got.Allow {
				if len(got.Allow[i]) != len(tc.wantAllow[i]) {
					t.Fatalf("Allow[%d] = %v, want %v", i, got.Allow[i], tc.wantAllow[i])
				}
				for j := range got.Allow[i] {
					if got.Allow[i][j] != tc.wantAllow[i][j] {
						t.Errorf("Allow[%d][%d] = %q, want %q", i, j, got.Allow[i][j], tc.wantAllow[i][j])
					}
				}
			}
			if got.Path != path {
				t.Errorf("Path = %q, want %q", got.Path, path)
			}
		})
	}
}

// TestLoadAllowlistFileRefusesAMissingFile: a caller who passed --policy-file
// at a path that does not exist gets an error, not the built-in default —
// unlike internal/harness's LoadFileConfig, there is no safe default a
// declared-allowlist file could fall back to.
func TestLoadAllowlistFileRefusesAMissingFile(t *testing.T) {
	if _, err := permission.LoadAllowlistFile(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("LoadAllowlistFile succeeded reading a file that does not exist")
	}
}
