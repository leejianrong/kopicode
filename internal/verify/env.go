package verify

import (
	"os"
	"strings"
)

// APIKeyEnv is the one variable no subprocess kopicode starts ever inherits.
// CLAUDE.md: the provider credential must appear in no journal, no blob and no
// log, and the cheapest way to keep it out is for the process that could print
// it not to have it. A verification command runs a project's whole test suite
// and its output is journaled verbatim, so this one is not optional.
const APIKeyEnv = "OPENROUTER_API_KEY"

// stripped is what a verification command never inherits, and why.
//
// **This is a denylist and that is deliberate**, for the reason internal/syntax
// gives: an allowlist is right for secrets, where the cost of missing one is
// unbounded, and wrong for a toolchain, where GOMODCACHE, XDG_CACHE_HOME,
// SSL_CERT_FILE, TMPDIR, PATH and a dozen distribution-specific variables all
// have to survive or the suite does not run at all. A verification that cannot
// run reports [NotRun] on every turn, which is honest and useless.
//
// So the rule is narrower: strip the credential, and strip what changes *what
// the suite concludes* or *which repository it acts on*. This is not a sandbox
// and does not pretend to be — the command is the project's own, and SLICE-1 §9
// puts isolation in slice 3.
var stripped = []struct {
	name string
	why  string
}{
	{APIKeyEnv, "the provider credential belongs in no subprocess"},

	// Git. This is the class that has already corrupted this repository once:
	// GIT_DIR beats a correctly-set working directory outright, so a test suite
	// that shells out to git would act on whatever the ambient environment
	// names rather than on the tree being verified
	// (.claude/skills/agent-ground-rules/SKILL.md).
	{"GIT_DIR", "overrides the working directory and redirects git at another repository"},
	{"GIT_WORK_TREE", "redirects git at another working tree"},
	{"GIT_INDEX_FILE", "redirects git at another index"},
	{"GIT_COMMON_DIR", "redirects git's shared refs, config and objects"},
	{"GIT_OBJECT_DIRECTORY", "redirects where git writes objects"},
	{"GIT_CONFIG", "replaces the repository's configuration"},
	{"GIT_CONFIG_GLOBAL", "replaces the user's configuration"},
	{"GIT_CONFIG_SYSTEM", "replaces the system configuration"},

	// The Go toolchain. GOFLAGS can inject build tags or -mod=vendor and change
	// which files are compiled at all; GOOS and GOARCH make the suite run
	// against a platform nobody is on.
	{"GOFLAGS", "can inject build tags and change which files compile"},
	{"GOOS", "would verify the change against the wrong platform"},
	{"GOARCH", "would verify the change against the wrong platform"},
	{"CGO_ENABLED", "changes which build constraints are satisfied"},
	{"GOWORK", "a workspace file can redirect the build to other modules"},

	// Python and Node. These change what the suite imports and can make a
	// passing tree look broken, or a broken one look clean.
	{"PYTHONPATH", "changes what the interpreter imports"},
	{"PYTHONHOME", "points the interpreter at a different standard library"},
	{"PYTHONSTARTUP", "runs someone else's code before the suite"},
	{"NODE_OPTIONS", "can require arbitrary modules before the suite"},
	{"NODE_PATH", "changes module resolution"},
}

// baseEnv is the environment a verification command runs in: the parent's, minus
// [stripped].
//
// It is a named builder rather than an os.Environ() at the call site because
// assigning Env is not the same as deciding what it holds (KAN-845): the name
// and this comment are the review surface for what a project's own test suite
// gets to see.
func (v *Verifier) baseEnv() []string {
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
