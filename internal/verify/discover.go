package verify

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MakefileNames are the makefiles GNU make itself looks for, in its own order.
//
// The order matters and is not alphabetical: GNU make reads GNUmakefile first,
// then makefile, then Makefile, and a repository that carries two is relying on
// that. Guessing differently here would run a different file from the one
// `make test` in a terminal runs, which is the one thing discovery must not do.
var MakefileNames = []string{"GNUmakefile", "makefile", "Makefile"}

// MakefileTargets are the targets discovery will run, in preference order.
//
// `test` first because it is the card's example and because a project that has
// both means the narrower one. `check` second: several projects — this one
// included — use it for the static gates and it is a real verification. `verify`
// last, as the explicit spelling.
//
// This is deliberately short. Every entry is a target this harness will run
// unattended in a bench arm, so widening it widens what a stranger's Makefile
// gets to execute.
var MakefileTargets = []string{"test", "check", "verify"}

// maxProbeBytes caps how much of a discovery file is read.
//
// A makefile, a package.json and a pyproject.toml are all small. The cap exists
// so that a tree holding a multi-gigabyte file under one of those names cannot
// make discovery — which runs on the way to every verification — allocate it.
const maxProbeBytes = 1 << 20 // 1 MiB

// Discover picks a verification command for the project rooted at root, or
// reports honestly that there is none.
//
// **Nothing is executed.** Every rule below is a question about bytes in the
// tree: is there a makefile, does it declare a target, does package.json declare
// a test script. Running `make -n` or `npm run` to find out would turn discovery
// into a side effect, and one that happens before the user has consented to
// anything.
//
// The order is fixed and is the one thing here worth arguing about, so it is
// written down:
//
//  1. **A makefile target.** A project that ships a `test` target has stated how
//     it is verified, and that statement outranks anything inferred from which
//     language files happen to be present. It is also the only rule that works
//     for a polyglot repository, which is why it is first rather than last.
//  2. **A Go module** — go.mod in the root — runs `go test ./...`.
//  3. **A Node project** — package.json declaring a non-empty `scripts.test` —
//     runs `npm test`. The script has to exist: `npm test` on a package.json
//     without one prints "no test specified" and exits 1, and a discovery that
//     produced a command guaranteed to fail would block every success report on
//     a project that simply has no tests.
//  4. **A uv project** — pyproject.toml beside a uv.lock or a `[tool.uv]` table —
//     runs `uv run pytest`.
//  5. **Nothing**, reported as [SourceNone].
//
// Go before Node before uv only decides a polyglot tree with no makefile, which
// is the case where any answer is a guess; what matters is that the guess is
// fixed rather than dependent on directory-read order.
func Discover(root string) Plan {
	if root == "" {
		return Plan{Source: SourceNone}
	}
	for _, rule := range discoveryRules {
		if plan, ok := rule(root); ok {
			return plan
		}
	}
	return Plan{Source: SourceNone}
}

// discoveryRules is the ordered chain [Discover] walks. A slice and not a map:
// the order is the decision.
var discoveryRules = []func(root string) (Plan, bool){
	discoverMakefile,
	discoverGoModule,
	discoverNodeProject,
	discoverUvProject,
}

// discoverMakefile looks for a makefile in the root declaring one of
// [MakefileTargets].
func discoverMakefile(root string) (Plan, bool) {
	for _, name := range MakefileNames {
		content, ok := probe(filepath.Join(root, name))
		if !ok {
			continue
		}
		declared := makefileTargets(content)
		for _, target := range MakefileTargets {
			if !declared[target] {
				continue
			}
			return Plan{
				Argv:   []string{"make", target},
				Source: SourceDiscovered,
				Rule:   RuleMakefileTarget,
				Detail: name + " declares a `" + target + "` target",
			}, true
		}
		// A makefile with none of the targets is not a reason to keep looking
		// for another makefile — make would have read this one — but it is a
		// reason to fall through to the language rules.
		break
	}
	return Plan{}, false
}

// makefileTargets is the set of target names a makefile declares.
//
// It is a deliberately small reading of make's grammar, and its bias is towards
// missing a target rather than inventing one: a target discovery does not see
// costs a fallback to the language rules, while a target that is not really
// there costs a verification command that fails on every turn.
//
// What it understands:
//
//   - A rule line is one that does not begin with a tab (that is a recipe) and
//     holds a colon before any `=`.
//   - Everything left of that colon is a whitespace-separated list of targets,
//     which is why `test lint:` declares both.
//   - `::` is a double-colon rule and its left side is still the target list.
//
// What it refuses:
//
//   - `:=`, `::=`, `?=`, `+=` and plain `=` are variable assignments. `test :=
//     go test ./...` declares a variable, not a target, and treating it as one
//     would have discovery run a target that does not exist.
//   - Pattern rules (`%`) and lines with no colon at all.
//
// Included files are not followed. A target declared in an include would be
// missed, which is the direction chosen above; the alternative is an include
// resolver that has to reimplement make's search path.
func makefileTargets(content string) map[string]bool {
	targets := map[string]bool{}
	for line := range strings.Lines(content) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, "\t") || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		// An `=` before the colon means an assignment of some other form.
		if eq := strings.IndexByte(line, '='); eq >= 0 && eq < colon {
			continue
		}
		rest := line[colon+1:]
		rest = strings.TrimPrefix(rest, ":") // a double-colon rule
		if strings.HasPrefix(rest, "=") {
			// `:=` or `::=`: a variable assignment.
			continue
		}
		for _, name := range strings.Fields(line[:colon]) {
			if strings.Contains(name, "%") {
				continue
			}
			targets[name] = true
		}
	}
	return targets
}

// discoverGoModule looks for go.mod in the root.
func discoverGoModule(root string) (Plan, bool) {
	if !isFile(filepath.Join(root, "go.mod")) {
		return Plan{}, false
	}
	return Plan{
		Argv:   []string{"go", "test", "./..."},
		Source: SourceDiscovered,
		Rule:   RuleGoModule,
		Detail: "go.mod is in the repository root",
	}, true
}

// nodeManifest is the part of package.json this package reads. Everything else
// is skipped: a manifest is shared, and a reader that refused a neighbour's key
// would make adding one a breaking change.
type nodeManifest struct {
	Scripts struct {
		Test string `json:"test"`
	} `json:"scripts"`
}

// discoverNodeProject looks for a package.json declaring a non-empty
// scripts.test.
//
// A package.json that does not parse is treated as absent rather than as an
// error. Discovery is a best-effort reading of somebody else's tree and it runs
// on the way to every verification; refusing to verify because a manifest has a
// trailing comma would put a JSON syntax error into ADR-0006 §3's harness
// bucket.
func discoverNodeProject(root string) (Plan, bool) {
	content, ok := probe(filepath.Join(root, "package.json"))
	if !ok {
		return Plan{}, false
	}
	var manifest nodeManifest
	if err := json.Unmarshal([]byte(content), &manifest); err != nil {
		return Plan{}, false
	}
	if strings.TrimSpace(manifest.Scripts.Test) == "" {
		return Plan{}, false
	}
	return Plan{
		Argv:   []string{"npm", "test"},
		Source: SourceDiscovered,
		Rule:   RuleNodeTestScript,
		Detail: "package.json declares a scripts.test",
	}, true
}

// discoverUvProject looks for a pyproject.toml that is a uv project.
//
// pyproject.toml alone is not enough: poetry, hatch, setuptools and pdm all use
// it, and `uv run pytest` in a poetry project is a command that does not work.
// The evidence is a uv.lock beside it or a `[tool.uv]` table inside it — the two
// things only uv writes.
func discoverUvProject(root string) (Plan, bool) {
	content, ok := probe(filepath.Join(root, "pyproject.toml"))
	if !ok {
		return Plan{}, false
	}

	var detail string
	switch {
	case isFile(filepath.Join(root, "uv.lock")):
		detail = "pyproject.toml sits beside a uv.lock"
	case hasTOMLTable(content, "tool.uv"):
		detail = "pyproject.toml declares a [tool.uv] table"
	default:
		return Plan{}, false
	}

	return Plan{
		Argv:   []string{"uv", "run", "pytest"},
		Source: SourceDiscovered,
		Rule:   RuleUvProject,
		Detail: detail,
	}, true
}

// hasTOMLTable reports whether content declares [name] or a table under it.
//
// This is a line scan and not a TOML parse, for the reason internal/harness gives
// for its own reader: dependencies stay near-zero, and the question being asked —
// "is there a [tool.uv] header" — is answered by looking at the headers. It is
// used only as corroborating evidence for a file that already exists, so a false
// negative falls through to "no verification command found", which is the honest
// answer this package is built to give.
func hasTOMLTable(content, name string) bool {
	for line := range strings.Lines(content) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") {
			continue
		}
		// An array-of-tables header is [[name]]; trimming twice reads both forms.
		header := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
		header = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(header, "["), "]"))
		if header == name || strings.HasPrefix(header, name+".") {
			return true
		}
	}
	return false
}

// probe reads a discovery file, reporting false for anything that is not a
// readable regular file within [maxProbeBytes].
func probe(path string) (string, bool) {
	if !isFile(path) {
		return "", false
	}
	f, err := os.Open(path) //nolint:gosec // the path is built from the repository root and a fixed name
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	content, err := io.ReadAll(io.LimitReader(f, maxProbeBytes))
	if err != nil {
		return "", false
	}
	return string(content), true
}

// isFile reports whether path is a regular file. A directory named go.mod is
// not a Go module, and a dangling symlink is not a makefile.
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
