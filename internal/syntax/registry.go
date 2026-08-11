package syntax

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Language is a language the gate knows how to check.
//
// The set is closed and small on purpose: SLICE-1's affordance V2 promises
// "full for Go, Python, JS/TS; honest no-op elsewhere", and an extension that
// is not here gets the honest no-op rather than a guess. Adding one means
// adding a constant, a [checker] entry and a row in the registry test — and the
// test derives the constants from this file's source, so a language added
// without being wired up fails the suite instead of quietly checking nothing.
type Language string

const (
	// LangUnknown is the zero value: no checker is registered for this file.
	LangUnknown Language = ""
	// LangGo is checked by gofmt -e, then advisorily by go build.
	LangGo Language = "go"
	// LangPython is checked by python -m py_compile.
	LangPython Language = "python"
	// LangJavaScript is checked by node --check.
	LangJavaScript Language = "javascript"
	// LangTypeScript is checked by tsc --noEmit, where a tsconfig exists.
	LangTypeScript Language = "typescript"
)

// checker is one language's entry in the registry.
type checker struct {
	lang Language
	// exts are the file extensions this language claims, with the leading dot
	// and lowercased.
	//
	// Deliberately absent: .jsx, which `node --check` rejects outright because
	// JSX is not JavaScript. Registering it would turn every edit to a React
	// file into a syntax-gate failure and, per SLICE-1 §9, into a `harness`
	// attribution. "No checker" is the true answer there.
	exts []string
	// plan builds the steps for one file. rel is the file as a local path
	// relative to [Gate.Root]. The returned Skip is gate-level: it means no
	// verdict step could run, so nothing vouched for the file.
	plan func(g *Gate, rel string) ([]spec, Skip)
}

// registry is every language the gate checks. Order is not significant;
// LanguageOf indexes it by extension.
var registry = []checker{
	{lang: LangGo, exts: []string{".go"}, plan: planGo},
	{lang: LangPython, exts: []string{".py"}, plan: planPython},
	{lang: LangJavaScript, exts: []string{".js", ".mjs", ".cjs"}, plan: planJavaScript},
	{lang: LangTypeScript, exts: []string{".ts", ".tsx", ".mts", ".cts"}, plan: planTypeScript},
}

// byExt indexes the registry. It is built once and never mutated; a duplicate
// extension would be a silent last-one-wins, so TestNoExtensionIsClaimedTwice
// refuses one.
var byExt = func() map[string]checker {
	m := make(map[string]checker)
	for _, c := range registry {
		for _, ext := range c.exts {
			m[ext] = c
		}
	}
	return m
}()

// LanguageOf reports which language the gate would check path as, or
// [LangUnknown] when none is registered for its extension.
//
// It is exported because the engine may want to know whether a gate run is
// worth journaling before it pays for one, and because a caller asking the
// question directly is better than a caller inferring it from an empty Result.
func LanguageOf(path string) Language {
	ext := strings.ToLower(filepath.Ext(path))
	if c, ok := byExt[ext]; ok {
		return c.lang
	}
	return LangUnknown
}

// Extensions returns every extension the gate claims, sorted. It exists for the
// registry test and for anyone auditing the promise in SLICE-1's affordance
// table.
func Extensions() []string {
	out := make([]string, 0, len(byExt))
	for ext := range byExt {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// spec is one step, resolved and ready to run.
type spec struct {
	// argv is the command, with argv[0] the plain binary name.
	argv []string
	// resolved is the absolute path the binary was found at. It is what gets
	// executed; argv[0] is what gets recorded.
	resolved string
	// dir is the absolute working directory, and relDir the same as a slash
	// path relative to [Gate.Root].
	dir, relDir string
	// env is the environment, built rather than inherited.
	env []string

	// verdict reports whether this step's exit status sets the gate's outcome.
	verdict bool
	// captureStdout is false where stdout is not a diagnostic. gofmt writes the
	// reformatted source there, and capturing it would put a copy of the file
	// into the journal on every edit.
	captureStdout bool

	// skip, when set, means this step cannot run and why. It is only ever set
	// on advisory steps: a verdict step that cannot run is a gate-level skip,
	// because the gate then has nothing to say about the file.
	skip Skip
	// cleanup releases anything the step needed, and is always called.
	cleanup func()
}

// plan builds the steps for one file.
func (g *Gate) plan(lang Language, rel string) ([]spec, Skip) {
	for _, c := range registry {
		if c.lang == lang {
			return c.plan(g, rel)
		}
	}
	return nil, SkipNoChecker
}

// planGo is `gofmt -e` and then, advisorily, `go build`.
//
// gofmt -e is the verdict: it parses the one file that was edited, needs no
// package context, and reports every parse error rather than the first ten.
// Its stdout is the reformatted source and is discarded; its stderr is the
// diagnostic.
//
// go build is advisory, and the package doc argues why at length: it typechecks
// a whole package, and a correct edit can legitimately leave a package failing
// to typecheck mid-refactor. It is scoped to the edited file's own package
// rather than to ./... so that a break elsewhere in the module is not even
// reported here, and it builds to os.DevNull so a main package does not drop a
// binary into the user's tree.
func planGo(g *Gate, rel string) ([]spec, Skip) {
	gofmt, err := g.lookPath("gofmt")
	if err != nil {
		return nil, SkipToolMissing
	}
	verdict := spec{
		argv:     []string{"gofmt", "-e", filepath.ToSlash(rel)},
		resolved: gofmt,
		dir:      g.Root,
		relDir:   ".",
		env:      goEnv(g.baseEnv()),
		verdict:  true,
	}

	build := spec{
		argv:          []string{"go", "build", "-o", os.DevNull, "."},
		relDir:        ".",
		dir:           g.Root,
		env:           goEnv(g.baseEnv()),
		captureStdout: true,
	}
	switch goBin, gerr := g.lookPath("go"); {
	case gerr != nil:
		build.skip = SkipToolMissing
	default:
		build.resolved = goBin
		moduleDir, ok := g.findUp(rel, "go.mod")
		if !ok {
			build.skip = SkipNoProject
			break
		}
		build.relDir = moduleDir
		build.dir = filepath.Join(g.Root, filepath.FromSlash(moduleDir))
		build.argv[len(build.argv)-1] = "./" + relSlashDir(moduleDir, rel)
	}

	return []spec{verdict, build}, SkipNone
}

// planPython is `python -m py_compile`.
//
// python3 is preferred over python because on most current machines only the
// former exists, and where both do they are the same interpreter. argv[0] is
// recorded as whichever was found, so the journal says which interpreter
// vouched for the file.
//
// py_compile writes bytecode next to the source, which would drop __pycache__
// directories into the user's tree on every edit and into every git snapshot.
// PYTHONPYCACHEPREFIX redirects it into a temporary directory that is removed
// when the check finishes; see env.go.
func planPython(g *Gate, rel string) ([]spec, Skip) {
	var name, resolved string
	for _, candidate := range []string{"python3", "python"} {
		if p, err := g.lookPath(candidate); err == nil {
			name, resolved = candidate, p
			break
		}
	}
	if resolved == "" {
		return nil, SkipToolMissing
	}

	env, cleanup := pythonEnv(g.baseEnv())
	return []spec{{
		argv:          []string{name, "-m", "py_compile", filepath.ToSlash(rel)},
		resolved:      resolved,
		dir:           g.Root,
		relDir:        ".",
		env:           env,
		verdict:       true,
		captureStdout: true,
		cleanup:       cleanup,
	}}, SkipNone
}

// planJavaScript is `node --check`, which parses one file and nothing else.
func planJavaScript(g *Gate, rel string) ([]spec, Skip) {
	node, err := g.lookPath("node")
	if err != nil {
		return nil, SkipToolMissing
	}
	return []spec{{
		argv:          []string{"node", "--check", filepath.ToSlash(rel)},
		resolved:      node,
		dir:           g.Root,
		relDir:        ".",
		env:           nodeEnv(g.baseEnv()),
		verdict:       true,
		captureStdout: true,
	}}, SkipNone
}

// planTypeScript is `tsc --noEmit`, and only where a tsconfig exists — the card
// is explicit about that, so the tsconfig is looked for rather than assumed.
//
// This is the one project-scoped check that is nonetheless a verdict step, and
// the package doc says why: TypeScript ships no single-file syntax checker, so
// the alternative to accepting tsc's breadth is no coverage at all for a
// language SLICE-1 lists as covered.
//
// The project's own node_modules/.bin/tsc is preferred over one on PATH,
// because a project pinned to a TypeScript version is checked against that
// version rather than against whatever is installed globally. argv[0] stays
// "tsc" either way, so the recorded command does not depend on where the binary
// was found.
func planTypeScript(g *Gate, rel string) ([]spec, Skip) {
	projectDir, ok := g.findUp(rel, "tsconfig.json")
	if !ok {
		return nil, SkipNoProject
	}

	resolved := ""
	local := filepath.Join(g.Root, filepath.FromSlash(projectDir), "node_modules", ".bin", "tsc")
	if info, err := os.Stat(local); err == nil && !info.IsDir() {
		resolved = local
	} else if p, lerr := g.lookPath("tsc"); lerr == nil {
		resolved = p
	}
	if resolved == "" {
		return nil, SkipToolMissing
	}

	project := path.Join(projectDir, "tsconfig.json")
	return []spec{{
		argv:          []string{"tsc", "--noEmit", "-p", project},
		resolved:      resolved,
		dir:           g.Root,
		relDir:        ".",
		env:           nodeEnv(g.baseEnv()),
		verdict:       true,
		captureStdout: true,
	}}, SkipNone
}

// findUp walks from the directory holding rel up to [Gate.Root] looking for
// name, and returns the directory holding it as a slash path relative to the
// root.
//
// The walk stops at the root and never above it. A go.mod or tsconfig.json
// outside the repository is not this repository's project file, and following
// one would make the gate's behaviour depend on where the checkout happens to
// sit on disk.
func (g *Gate) findUp(rel, name string) (string, bool) {
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "." || dir == "/" {
		dir = "."
	}
	for {
		candidate := filepath.Join(g.Root, filepath.FromSlash(dir), name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return dir, true
		}
		if dir == "." {
			return "", false
		}
		dir = path.Dir(dir)
	}
}

// relSlashDir returns the directory of rel relative to base, as the "." or
// "x/y" form a Go package pattern wants after its "./" prefix.
func relSlashDir(base, rel string) string {
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "" {
		dir = "."
	}
	if base == "." {
		return dir
	}
	trimmed := strings.TrimPrefix(dir, base)
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" {
		return "."
	}
	return trimmed
}
