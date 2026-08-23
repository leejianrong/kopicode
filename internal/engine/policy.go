package engine

import (
	"github.com/leejianrong/kopicode/internal/permission"
)

// PolicyFile is the engine's facade over [permission.AllowlistFile]
// (ADR-0011), the same way [Selection] is a facade over harness.Selection:
// ADR-0003's allowlist gives a front end three internal imports —
// internal/engine, internal/bench, internal/build — and internal/permission
// is not one of them, so a front end cannot spell permission.AllowlistFile
// itself. Every field is a stdlib type (string, [][]string), so a caller that
// only ever writes `engine.PolicyFile` never needs to know
// internal/permission exists.
type PolicyFile = permission.AllowlistFile

// LoadPolicyFile reads and parses a declared-allowlist file (ADR-0011) at
// path, for a caller — `cmd/kopicode run --print --policy-file`, today — that
// wants to hand [Options.Policy] something other than the zero value.
//
// Every error this returns is the caller's own configuration mistake: a bad
// path, a malformed line, a relative root. None of them are a harness or
// provider failure, which is why a front end maps every one of them to the
// usage exit code rather than the harness one — see [permission.LoadAllowlistFile]'s
// own doc comment.
func LoadPolicyFile(path string) (PolicyFile, error) {
	return permission.LoadAllowlistFile(path)
}
