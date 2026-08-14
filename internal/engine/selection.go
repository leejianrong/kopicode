package engine

import (
	"flag"

	"github.com/leejianrong/kopicode/internal/harness"
)

// Which arm a session is — model, harness configuration, provider pin — is the
// engine's to decide and the surfaces' to ask for, the same split permission
// follows. So the front ends resolve it through this file, and the loop reads
// the result off a [Selection] rather than re-deriving any of it.
//
// # Why this is a facade over internal/harness
//
// ADR-0003's allowlist lets a front end import internal/engine, internal/bench
// and internal/build, and nothing else; internal/arch enforces it and widening
// it needs an ADR. Both front ends need --model and --harness (ADR-0007 decision
// 2), so the entry point has to be here.
//
// The registry, the harness configuration, the hash preimage and the config-file
// reader do *not* have to be here, and are not: they are internal/harness, which
// is a leaf over internal/provider and internal/anchor and can be reviewed and
// tested without the loop. What lives here is the seam — the names a surface
// binds, the values it gets back — which is the part that genuinely belongs to
// the engine's public interface.

// Selection is the resolved arm: model id, provider pin, harness configuration
// and the hash journal.SessionStarted records.
//
// The loop writes ModelID, Pin and HarnessConfigHash onto SessionStarted and
// Pin onto every provider request. It does not recompute the hash: one session
// has one arm, and two events that disagreed about it would be worse than
// either.
type Selection = harness.Selection

// HarnessConfig is a named, versioned harness configuration compiled into the
// binary (ADR-0007 decision 5). The loop reads MaxTurns, TokenBudget,
// RepairBudget, the system prompt and the sampling parameters off it rather than
// holding constants of its own — a bound that is a constant somewhere in the
// loop is a bound outside the hash, and therefore outside the arm.
type HarnessConfig = harness.Config

// SelectionOverrides are the flag values --model and --harness carry. Empty
// means "not given", which is what lets the config file be consulted.
type SelectionOverrides = harness.Overrides

// BindSelectionFlags registers --model and --harness on fs.
//
// Both front ends call this rather than declaring the flags themselves: two
// independent declarations would eventually differ in a name or a default, and
// the difference would surface as a bench arm that cannot be reproduced from the
// other binary.
func BindSelectionFlags(fs *flag.FlagSet) *SelectionOverrides { return harness.Bind(fs) }

// ResolveSelection resolves the arm for a session started in dir.
//
// Precedence is the flag, then `model = "..."` in dir's `.kopicode/config.toml`
// (searched upwards to the repository root), then the built-in default —
// ADR-0007 decision 2, and no environment variable at any position (decision 3).
//
// An unrecognised model or harness name comes back as a usage error, which
// [IsSelectionUsageError] identifies and a front end turns into exit 2. That is
// ADR-0007 decision 4, and the ordering it demands is structural rather than
// conventional: this call reads a file and touches nothing else, so there is no
// journal to half-open, no lock to leave held and no request to be billed for.
func ResolveSelection(dir string, o SelectionOverrides) (Selection, error) {
	return harness.Resolve(dir, o)
}

// IsSelectionUsageError reports whether err is a startup usage error — the
// exit-2 case of docs/SLICE-1.md step 14, as opposed to a harness error (4).
func IsSelectionUsageError(err error) bool { return harness.IsUsageError(err) }
