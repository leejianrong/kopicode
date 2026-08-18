// Package build carries the identity of the binary that is running.
//
// # Why this exists
//
// ADR-0007 decision 6 deliberately leaves the *code* out of the harness config
// hash preimage: the hash covers configuration, and hashing the binary would
// make every recompile a new arm. The cost is that a code change which alters
// loop behaviour without touching a configuration field — a parse-route fix, a
// different retry condition, a changed context assembly — is invisible to the
// hash, so two results from two different binaries can carry identical hashes
// and be pooled as if they were one arm. ADR-0005's whole point is that an
// unpinned comparison is not evidence, and that is an unpinned comparison.
//
// The envelope's schema_version does not help: it versions the journal format,
// not the binary. So the binary says who it is, on SessionStarted, and
// ADR-0007 decision 7 makes the bench runner act on it.
//
// # Where the values come from
//
// Three package-level variables, set by the linker (see the Makefile):
//
//	-X github.com/leejianrong/kopicode/internal/build.version=...
//	-X github.com/leejianrong/kopicode/internal/build.commit=...
//	-X github.com/leejianrong/kopicode/internal/build.treeState=...
//
// They are unexported and deliberately zero-valued, because the zero value is
// the honest answer for a binary that was built without them. Resolve reports
// "unknown" rather than inventing a plausible default: a fabricated build id is
// worse than a missing one for exactly the reason this package exists.
//
// # ldflags versus runtime/debug
//
// Both, in that order, because neither is sufficient alone.
//
// The linker path is primary: only the Makefile's `git describe` call names a
// version relative to a tag reliably, in every checkout state. That
// "reliably" is load-bearing rather than decorative — `git describe` gives
// the same answer whether HEAD is attached to a branch or detached at a
// commit, and a tag-triggered release build (`git checkout <tag>`, or
// `actions/checkout` on a `push: tags` event, `.github/workflows/release.yml`)
// always detaches HEAD. This package's fallback below does not have that
// property, which is the reason it stays a fallback.
//
// runtime/debug.ReadBuildInfo is the fallback, and it covers the case the
// Makefile cannot reach at all: `go build ./cmd/kopicode` or `go install
// github.com/leejianrong/kopicode/cmd/kopicode@latest` by a user who never runs
// make. It always stamps vcs.revision and vcs.modified from a VCS checkout.
// What it stamps as bi.Main.Version is less uniform, and this is measured
// against this repo's pinned Go 1.26.5 rather than assumed:
//
//   - `go install .../cmd/kopicode@vX.Y.Z` resolves through the module
//     system and reports the resolved tag verbatim — a real semver, and the
//     realistic path for a user with no release binary to fetch.
//   - `go build ./cmd/kopicode` from a local checkout with HEAD attached to
//     a branch reports the nearest reachable tag too: exactly the tag string
//     at that commit, or a pseudo-version naming it as ancestor
//     (`vX.Y.(Z+1)-0.<timestamp>-<commit12>`) past it. Both are real
//     information handed to moduleVersion unfiltered.
//   - The same build with HEAD detached — the exact state a tag-triggered CI
//     checkout leaves it in — reports "(devel)" even sitting on the tagged
//     commit with a clean tree. Go does not consider a detached checkout
//     eligible for its own tag derivation, which is precisely why the
//     release build (`make xbuild`, driven by ldflags) does not depend on
//     this path ever seeing a tag: it doesn't need to.
//
// "(devel)" and an empty Main.Version are filtered to Unknown; anything else
// — pseudo-version or exact tag alike — passes through moduleVersion as-is,
// because it is real information the toolchain supplied and this package
// does not second-guess it. That is strictly better than "unknown", so it is
// used when the linker said nothing.
//
// The two are never mixed. A record whose Source is "ldflags" describes one
// coherent build; splicing a revision from one source onto a version from
// another would produce an identity that no build ever had.
//
// # Boundary note
//
// This package is a leaf: it imports nothing from this module, and
// internal/arch enforces that. It is on the short list of internal packages a
// front end under cmd/ may import (ADR-0003 decision 3), because `--version` is
// a surface concern and because a leaf value package is not engine internals.
// The leaf guard is what keeps that exception from widening into a back door.
package build

import (
	"runtime/debug"
	"strings"
)

// Injected by the linker. Never read these directly — go through Current,
// which applies the fallbacks and the "unknown" rules. Kept unexported so the
// only way to read a build identity is one that cannot report a half-resolved
// one.
var (
	version   string
	commit    string
	treeState string
)

// TreeState says whether the working tree was modified when the binary was
// built. It is a tri-state on purpose: a bool would have to spell "we do not
// know" as "clean", which is the fabrication this package refuses to make.
type TreeState string

const (
	// TreeClean means the working tree matched the recorded commit.
	TreeClean TreeState = "clean"
	// TreeDirty means it did not. A dirty build is not poolable with
	// anything — not even with another dirty build from the same commit,
	// because the two uncommitted trees need not be the same tree
	// (ADR-0007 decision 7).
	TreeDirty TreeState = "dirty"
	// TreeUnknown means nothing recorded it. Treated exactly as dirty is
	// for pooling: absence of evidence is not evidence of a clean tree.
	TreeUnknown TreeState = "unknown"
)

// Source names where the identity came from, so a reader can tell a
// linker-stamped record from a scavenged one without guessing.
const (
	// SourceLDFlags is the Makefile's -X injection.
	SourceLDFlags = "ldflags"
	// SourceBuildInfo is runtime/debug.ReadBuildInfo — Go's own VCS stamps,
	// or the module version for a binary built from the module cache.
	SourceBuildInfo = "buildinfo"
	// SourceUnknown is neither.
	SourceUnknown = "unknown"
)

// Unknown is the value recorded for a string field nothing supplied. It is a
// literal rather than an empty string so a reader cannot mistake it for a field
// that has not been populated yet, and it is not sha-shaped, so it cannot be
// mistaken for a commit.
const Unknown = "unknown"

// Info identifies the binary that produced a session record.
//
// Every field is always populated: Unknown or TreeUnknown where nothing was
// recorded. There is no zero-value Info in a journal.
type Info struct {
	// Version is `git describe --tags --always --dirty` as of the build, or
	// the module version, or Unknown. Human-facing; do not parse it. The
	// machine-readable dirty bit is TreeState, which is computed
	// independently and is authoritative.
	Version string
	// Commit is the full commit sha the binary was built from, or Unknown.
	Commit string
	// TreeState is whether that commit described the whole build.
	TreeState TreeState
	// Source is which mechanism supplied the three fields above.
	Source string
}

// Current returns the identity of the running binary.
func Current() Info { return resolve(version, commit, treeState, debug.ReadBuildInfo) }

// String renders the identity for `--version`. The tree state is always shown,
// including when it is clean, because a suffix that appears only on a dirty
// build is one a reader has to already know about to miss.
func (i Info) String() string {
	return i.Version + " (commit " + i.Commit + ", tree " + string(i.TreeState) + ", via " + i.Source + ")"
}

// resolve applies the rules in the package doc. readBuildInfo is a parameter so
// the fallback path is testable without building a binary.
func resolve(version, commit, treeState string, readBuildInfo func() (*debug.BuildInfo, bool)) Info {
	// The linker path is all-or-nothing on the commit: a build stamped with a
	// version but no commit is a Makefile bug, and the honest report is that
	// the linker did not supply an identity.
	if strings.TrimSpace(commit) != "" {
		return Info{
			Version:   orUnknown(version),
			Commit:    strings.TrimSpace(commit),
			TreeState: parseTreeState(treeState),
			Source:    SourceLDFlags,
		}
	}

	bi, ok := readBuildInfo()
	if !ok || bi == nil {
		return unknownInfo()
	}

	out := Info{
		Version:   moduleVersion(bi.Main.Version),
		Commit:    Unknown,
		TreeState: TreeUnknown,
		Source:    SourceBuildInfo,
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if v := strings.TrimSpace(s.Value); v != "" {
				out.Commit = v
			}
		case "vcs.modified":
			switch s.Value {
			case "true":
				out.TreeState = TreeDirty
			case "false":
				out.TreeState = TreeClean
			}
		}
	}

	// Nothing usable came back — a test binary, or a build with -buildvcs=false
	// from outside a repository. Say so rather than claiming a source that
	// supplied no facts.
	if out.Version == Unknown && out.Commit == Unknown {
		return unknownInfo()
	}
	return out
}

func unknownInfo() Info {
	return Info{Version: Unknown, Commit: Unknown, TreeState: TreeUnknown, Source: SourceUnknown}
}

// parseTreeState refuses to guess. Anything the build did not spell exactly
// "clean" or "dirty" is unknown, so a typo in the Makefile degrades to the
// cautious answer instead of the flattering one.
func parseTreeState(s string) TreeState {
	switch TreeState(strings.TrimSpace(s)) {
	case TreeClean:
		return TreeClean
	case TreeDirty:
		return TreeDirty
	default:
		return TreeUnknown
	}
}

// moduleVersion filters the placeholders the toolchain uses for a build that
// has no released version: "(devel)" and "". Neither identifies anything.
func moduleVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "(devel)" {
		return Unknown
	}
	return v
}

func orUnknown(s string) string {
	if s = strings.TrimSpace(s); s != "" {
		return s
	}
	return Unknown
}
