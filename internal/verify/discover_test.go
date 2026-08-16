package verify_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/leejianrong/kopicode/internal/verify"
)

// tree writes a fixture project and returns its root. Every path is relative and
// every file lands under t.TempDir(), so nothing here can reach the repository
// this test is running in.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	return root
}

// TestDiscoveryPicksTheRightCommand is KAN-787's "done when" clause, in full:
// a Makefile, a Go module, a Node project and a uv project each get the command
// the card names, and a tree matching none of them is told so rather than given
// a default.
//
// It is one table on purpose. The four positive cases and the honest-none case
// are one claim — that discovery is a total function over a tree with no
// fabricated answers — and splitting them into five tests would let four pass
// while the fifth quietly returned `go test ./...` for everything.
func TestDiscoveryPicksTheRightCommand(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		want   []string
		source verify.Source
		rule   verify.Rule
	}{
		{
			name:   "a Makefile with a test target",
			files:  map[string]string{"Makefile": "help:\n\t@echo hi\n\ntest:\n\tgo test ./...\n"},
			want:   []string{"make", "test"},
			source: verify.SourceDiscovered,
			rule:   verify.RuleMakefileTarget,
		},
		{
			name:   "a Go module",
			files:  map[string]string{"go.mod": "module example.com/x\n\ngo 1.26\n"},
			want:   []string{"go", "test", "./..."},
			source: verify.SourceDiscovered,
			rule:   verify.RuleGoModule,
		},
		{
			name:   "a Node project",
			files:  map[string]string{"package.json": `{"name":"x","scripts":{"test":"vitest run"}}`},
			want:   []string{"npm", "test"},
			source: verify.SourceDiscovered,
			rule:   verify.RuleNodeTestScript,
		},
		{
			name: "a uv project, by its lock file",
			files: map[string]string{
				"pyproject.toml": "[project]\nname = \"x\"\n",
				"uv.lock":        "version = 1\n",
			},
			want:   []string{"uv", "run", "pytest"},
			source: verify.SourceDiscovered,
			rule:   verify.RuleUvProject,
		},
		{
			name:   "a uv project, by its tool table",
			files:  map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n\n[tool.uv]\ndev-dependencies = []\n"},
			want:   []string{"uv", "run", "pytest"},
			source: verify.SourceDiscovered,
			rule:   verify.RuleUvProject,
		},
		{
			name:   "a tree kopicode does not recognise",
			files:  map[string]string{"README.md": "# hello\n", "main.rs": "fn main() {}\n"},
			want:   nil,
			source: verify.SourceNone,
			rule:   verify.RuleNone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := verify.Discover(tree(t, tc.files))

			if !slices.Equal(got.Argv, tc.want) {
				t.Errorf("Discover() argv = %v, want %v", got.Argv, tc.want)
			}
			if got.Source != tc.source {
				t.Errorf("Discover() source = %q, want %q", got.Source, tc.source)
			}
			if got.Rule != tc.rule {
				t.Errorf("Discover() rule = %q, want %q", got.Rule, tc.rule)
			}
			if got.Found() != (tc.want != nil) {
				t.Errorf("Discover().Found() = %v for argv %v", got.Found(), got.Argv)
			}
			if got.Found() && got.Detail == "" {
				t.Error("a discovered command records no evidence; the record should say which " +
					"file in the tree produced this command")
			}
		})
	}
}

// TestNothingFoundIsNotAFabricatedDefault is the other half of "says so honestly
// when it finds none", and it is separate because it is a different failure.
//
// The table above would still pass if [verify.SourceNone] came back beside a
// plausible-looking argv. What must not exist is a command nobody asked for: a
// harness that ran `go test ./...` in a tree with no go.mod would report a
// failure that is about nothing.
func TestNothingFoundIsNotAFabricatedDefault(t *testing.T) {
	plan := verify.Discover(tree(t, map[string]string{"notes.txt": "nothing here\n"}))

	if plan.Argv != nil {
		t.Errorf("Discover() invented %v for a tree with no verification command", plan.Argv)
	}
	if plan.Found() {
		t.Error("Discover().Found() is true with no command")
	}
	if plan.String() != "(none)" {
		t.Errorf("Discover().String() = %q, want %q", plan.String(), "(none)")
	}
}

// TestDiscoveryRunsNothing holds the rule that discovery reads the tree rather
// than probing it.
//
// The Makefile's target creates a sentinel file. If discovery ever shelled out —
// `make -n`, `make -q`, `npm run` — the sentinel would appear, and a discovery
// step would have become a side effect that happens before the user has
// consented to anything.
func TestDiscoveryRunsNothing(t *testing.T) {
	root := tree(t, map[string]string{
		"Makefile":     "test:\n\ttouch ran-the-target\n",
		"package.json": `{"scripts":{"test":"touch ran-the-script"}}`,
	})

	if plan := verify.Discover(root); !plan.Found() {
		t.Fatalf("the fixture was meant to be discoverable; got %v", plan)
	}

	for _, sentinel := range []string{"ran-the-target", "ran-the-script"} {
		if _, err := os.Stat(filepath.Join(root, sentinel)); err == nil {
			t.Errorf("discovery created %s, so it executed the project's own build to decide "+
				"what to run; discovery reads files and never runs them", sentinel)
		}
	}
}

// TestAMakefileOutranksTheLanguageRules pins the precedence, which is the only
// part of discovery worth arguing about.
//
// A project that ships a `test` target has said how it is verified, and that
// statement beats anything inferred from which language files are lying around.
// It is also the only rule that works for a polyglot repository.
func TestAMakefileOutranksTheLanguageRules(t *testing.T) {
	root := tree(t, map[string]string{
		"Makefile":       "test:\n\t./run-everything\n",
		"go.mod":         "module example.com/x\n\ngo 1.26\n",
		"package.json":   `{"scripts":{"test":"vitest run"}}`,
		"pyproject.toml": "[tool.uv]\n",
	})

	got := verify.Discover(root)
	if want := []string{"make", "test"}; !slices.Equal(got.Argv, want) {
		t.Errorf("Discover() = %v, want %v; a project that declares a test target has already "+
			"answered the question", got.Argv, want)
	}
}

// TestTheLanguageRulesHaveAFixedOrder covers the case a Makefile does not
// decide. Any answer here is a guess; what matters is that the guess is the same
// every run rather than a function of directory-read order.
func TestTheLanguageRulesHaveAFixedOrder(t *testing.T) {
	root := tree(t, map[string]string{
		"go.mod":         "module example.com/x\n\ngo 1.26\n",
		"package.json":   `{"scripts":{"test":"vitest run"}}`,
		"pyproject.toml": "[tool.uv]\n",
	})

	first := verify.Discover(root)
	if want := []string{"go", "test", "./..."}; !slices.Equal(first.Argv, want) {
		t.Errorf("Discover() = %v, want %v", first.Argv, want)
	}
	for range 5 {
		if again := verify.Discover(root); !slices.Equal(again.Argv, first.Argv) {
			t.Fatalf("Discover() returned %v then %v for the same tree; discovery is not "+
				"deterministic and two runs of one arm would verify differently",
				first.Argv, again.Argv)
		}
	}
}

// TestMakefileTargetPreference walks the target list, and the last case is the
// one that matters: a makefile with none of them falls through to the language
// rules rather than producing `make` with a target that does not exist.
func TestMakefileTargetPreference(t *testing.T) {
	cases := []struct {
		name     string
		makefile string
		want     []string
	}{
		{"test wins over check", "check:\n\t@true\ntest:\n\t@true\n", []string{"make", "test"}},
		{"check when there is no test", "build:\n\t@true\ncheck:\n\t@true\n", []string{"make", "check"}},
		{"verify last", "verify:\n\t@true\n", []string{"make", "verify"}},
		{"several targets on one line", "test lint:\n\t@true\n", []string{"make", "test"}},
		{"a double-colon rule", "test::\n\t@true\n", []string{"make", "test"}},
		{"none of them falls through", "build:\n\t@true\n", []string{"go", "test", "./..."}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t, map[string]string{
				"Makefile": tc.makefile,
				// Present so the fall-through case has somewhere to fall to.
				"go.mod": "module example.com/x\n\ngo 1.26\n",
			})
			if got := verify.Discover(root); !slices.Equal(got.Argv, tc.want) {
				t.Errorf("Discover() = %v, want %v", got.Argv, tc.want)
			}
		})
	}
}

// TestAVariableIsNotATarget is the makefile reading's fail-safe direction.
//
// `test := go test ./...` and `.PHONY: test` both put the word "test" next to a
// colon and neither declares a target. Reading either as one produces `make
// test`, which fails on every turn of every session in that project — and a
// verification command that always fails blocks every success report, which is
// the worst failure this card can have.
func TestAVariableIsNotATarget(t *testing.T) {
	notTargets := map[string]string{
		"a simple assignment":         "test := go test ./...\n",
		"a spaced assignment":         "test  :=  go test ./...\n",
		"a conditional assignment":    "test ?= go test ./...\n",
		"an appending assignment":     "test += -v\n",
		"a plain assignment":          "test = go test ./...\n",
		"a POSIX assignment":          "test ::= go test ./...\n",
		"a phony declaration alone":   ".PHONY: test check verify\n",
		"a recipe line mentioning it": "build:\n\ttest -f out && echo done\n",
		"a comment":                   "# test: this is not a rule\n",
		"a pattern rule":              "%test:\n\t@true\n",
	}

	for name, makefile := range notTargets {
		t.Run(name, func(t *testing.T) {
			root := tree(t, map[string]string{
				"Makefile": makefile,
				"go.mod":   "module example.com/x\n\ngo 1.26\n",
			})
			got := verify.Discover(root)
			if slices.Equal(got.Argv, []string{"make", "test"}) {
				t.Errorf("Discover() read %q as declaring a `test` target; `make test` would then "+
					"fail on every turn and block every success report", makefile)
			}
			if want := []string{"go", "test", "./..."}; !slices.Equal(got.Argv, want) {
				t.Errorf("Discover() = %v, want the fall-through %v", got.Argv, want)
			}
		})
	}
}

// TestMakefileNamesFollowMakesOwnOrder guards the one detail a reimplementation
// gets wrong: GNU make reads GNUmakefile before Makefile, so a repository
// carrying both is relying on that and kopicode must run the same file a
// terminal would.
func TestMakefileNamesFollowMakesOwnOrder(t *testing.T) {
	root := tree(t, map[string]string{
		"GNUmakefile": "verify:\n\t@true\n",
		"Makefile":    "test:\n\t@true\n",
	})

	got := verify.Discover(root)
	if want := []string{"make", "verify"}; !slices.Equal(got.Argv, want) {
		t.Errorf("Discover() = %v, want %v; GNU make reads GNUmakefile first, and discovering "+
			"a target from a file make would not read is a command that does not exist",
			got.Argv, want)
	}
}

// TestANodeProjectNeedsATestScript is the honest-none case wearing a
// language-specific hat.
//
// `npm test` on a package.json with no scripts.test prints "no test specified"
// and exits 1. Discovering it would give every JavaScript project a verification
// command guaranteed to fail, which reads as "this model cannot finish a task"
// on a project that simply has no tests.
func TestANodeProjectNeedsATestScript(t *testing.T) {
	cases := map[string]string{
		"no scripts at all":              `{"name":"x","version":"1.0.0"}`,
		"scripts, no test":               `{"scripts":{"build":"tsc"}}`,
		"an empty test":                  `{"scripts":{"test":""}}`,
		"a whitespace test":              `{"scripts":{"test":"   "}}`,
		"a manifest that does not parse": `{"scripts":{"test":"vitest run",}}`,
	}

	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			plan := verify.Discover(tree(t, map[string]string{"package.json": manifest}))
			if plan.Found() {
				t.Errorf("Discover() = %v for %s; npm would exit 1 with nothing to run",
					plan.Argv, manifest)
			}
		})
	}
}

// TestAPyprojectAloneIsNotAUvProject keeps `uv run pytest` off the four other
// build systems that use pyproject.toml.
//
// poetry, hatch, pdm and setuptools all write one. `uv run pytest` in a poetry
// project does not work, so the evidence has to be something only uv writes.
func TestAPyprojectAloneIsNotAUvProject(t *testing.T) {
	cases := map[string]map[string]string{
		"a bare pyproject": {"pyproject.toml": "[project]\nname = \"x\"\n"},
		"a poetry project": {"pyproject.toml": "[tool.poetry]\nname = \"x\"\n"},
		"a lock file for something else": {
			"pyproject.toml": "[tool.poetry]\nname = \"x\"\n",
			"poetry.lock":    "# lock\n",
		},
	}

	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			if plan := verify.Discover(tree(t, files)); plan.Found() {
				t.Errorf("Discover() = %v; only uv.lock or [tool.uv] makes this a uv project",
					plan.Argv)
			}
		})
	}
}

// TestADirectoryIsNotAManifest covers the shape a stat-only check gets wrong. A
// directory named go.mod is not a Go module, and reading it as one produces a
// command for a project that does not exist.
func TestADirectoryIsNotAManifest(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "go.mod"), 0o755); err != nil {
		t.Fatalf("creating the directory: %v", err)
	}
	if plan := verify.Discover(root); plan.Found() {
		t.Errorf("Discover() = %v for a directory named go.mod", plan.Argv)
	}
}

// TestDiscoveryOutsideATreeFindsNothing covers the two degenerate roots a
// session can be handed: none at all, and one that does not exist.
func TestDiscoveryOutsideATreeFindsNothing(t *testing.T) {
	for _, root := range []string{"", filepath.Join(t.TempDir(), "does-not-exist")} {
		if plan := verify.Discover(root); plan.Found() {
			t.Errorf("Discover(%q) = %v", root, plan.Argv)
		}
	}
}
