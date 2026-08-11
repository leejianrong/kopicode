package syntax

import (
	"os"
	"strings"
)

// APIKeyEnv is the one variable no subprocess kopicode starts ever inherits.
// CLAUDE.md: the provider credential must appear in no journal, no blob and no
// log, and the cheapest way to keep it out is for the process that could print
// it not to have it.
const APIKeyEnv = "OPENROUTER_API_KEY"

// stripped is what a checker never inherits, and why.
//
// **This is a denylist and that is deliberate.** An allowlist is the right shape
// for secrets, where the cost of missing one is unbounded; it is the wrong shape
// for a toolchain, where GOMODCACHE, GOCACHE, XDG_CACHE_HOME, SSL_CERT_FILE,
// TMPDIR and a dozen distribution-specific variables all have to survive or the
// checker does not run at all. A gate that cannot run is a gate that reports
// [NotRun] on every file, which is honest and useless.
//
// So the rule is narrower: strip the credential, and strip the variables that
// change *what the checker concludes*. Everything a checker needs to find its
// own toolchain is left alone. This is not a sandbox and does not pretend to be
// — run_shell's doc makes the same point, and SLICE-1 §9 says isolation is
// slice 3.
var stripped = []struct {
	name string
	why  string
}{
	{APIKeyEnv, "the provider credential belongs in no subprocess"},

	// The Go toolchain. GOFLAGS can inject build tags or -mod=vendor and
	// silently change which files are even compiled; GOOS and GOARCH make the
	// gate check the file for a platform nobody is on, so a developer with
	// GOOS=windows exported would get verdicts about a cross-compile rather
	// than about their edit.
	{"GOFLAGS", "can inject build tags and change which files compile"},
	{"GOOS", "would check the edit against the wrong platform"},
	{"GOARCH", "would check the edit against the wrong platform"},
	{"GOARM", "would check the edit against the wrong platform"},
	{"GOAMD64", "would check the edit against the wrong platform"},
	{"CGO_ENABLED", "changes which build constraints are satisfied"},
	{"GO111MODULE", "changes whether the module graph is used at all"},
	{"GOEXPERIMENT", "changes the language and the toolchain's behaviour"},
	{"GOWORK", "a workspace file can redirect the build to other modules"},

	// Python. These change what py_compile imports and can make a clean file
	// look broken, or a broken one look clean.
	{"PYTHONPATH", "changes what the interpreter imports"},
	{"PYTHONHOME", "points the interpreter at a different standard library"},
	{"PYTHONSTARTUP", "runs someone else's code before the check"},
	{"PYTHONWARNINGS", "turns warnings into output, or into errors"},
	{"PYTHONOPTIMIZE", "changes what py_compile emits"},

	// Node. NODE_OPTIONS can require a module before --check runs.
	{"NODE_OPTIONS", "can require arbitrary modules before the check"},
	{"NODE_PATH", "changes module resolution"},
}

// baseEnv is the environment every checker starts from: the parent's, minus
// [stripped].
func (g *Gate) baseEnv() []string {
	parent := os.Environ()
	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		if !isStripped(kv) {
			out = append(out, kv)
		}
	}
	return out
}

func isStripped(kv string) bool {
	name, _, ok := strings.Cut(kv, "=")
	if !ok {
		return false
	}
	for _, s := range stripped {
		if name == s.name {
			return true
		}
	}
	return false
}

// goEnv is the environment the Go steps run in.
//
// GOPROXY=off and -mod=readonly together make the gate incapable of touching
// the network or of rewriting go.mod and go.sum. Both matter more than they
// look: this runs after *every* edit, so a step that can reach out to a proxy
// is a step that can hang the agent loop on someone's flaky DNS, and a step
// that can rewrite go.mod is a step that dirties the user's tree and every
// subsequent turn snapshot. A module whose dependencies are not in the cache
// therefore reports that it could not resolve them — which is fine, because the
// build step is advisory and cannot charge that to the edit.
func goEnv(base []string) []string {
	return append(base, "GOFLAGS=-mod=readonly", "GOPROXY=off")
}

// nodeEnv is the environment the node and tsc steps run in. The stripping in
// [stripped] is the whole of it; nothing needs adding.
func nodeEnv(base []string) []string { return base }

// pythonEnv is the environment the py_compile step runs in, plus the cleanup
// that removes what it writes.
//
// `python -m py_compile` writes bytecode next to the source by design —
// __pycache__/x.cpython-313.pyc — and sys.dont_write_bytecode does not stop it,
// because py_compile's whole job is to write that file. Left alone, a gate that
// runs after every edit would litter the user's tree with __pycache__
// directories and put them into every git snapshot.
//
// PYTHONPYCACHEPREFIX redirects the whole cache tree into a directory of our
// choosing, which is created here and removed when the step finishes.
// PYTHONDONTWRITEBYTECODE is set as well: it does not stop py_compile, but it
// does stop the interpreter caching the standard library modules py_compile
// itself imports, which is a few dozen files of pure noise per run.
//
// If the temporary directory cannot be created the step still runs, without the
// redirect: a missing __pycache__ guard is untidy, and refusing to check the
// file at all would be worse.
func pythonEnv(base []string) ([]string, func()) {
	env := append(base, "PYTHONDONTWRITEBYTECODE=1")
	dir, err := os.MkdirTemp("", "kopicode-pycache-")
	if err != nil {
		return env, nil
	}
	return append(env, "PYTHONPYCACHEPREFIX="+dir), func() { _ = os.RemoveAll(dir) }
}
