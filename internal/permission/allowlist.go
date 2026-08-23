package permission

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// AllowlistPolicy is ADR-0011's declared-allowlist policy: the non-interactive
// answerer for a caller — cuttlefish-agent, or any future orchestrator — that
// spawns kopicode against a real, persistent repository with nobody at a
// terminal to answer a consent prompt.
//
// It deliberately does not reuse [BenchPolicy], even though the two look
// similar at a glance. BenchPolicy's confine rule answers one question — "is
// this inside the one ephemeral worktree this task owns" — and both of its
// [Kind] branches reduce to that single check because a bench task's whole
// point is that nothing it does reaches outside that one directory.
// AllowlistPolicy answers two different questions that ADR-0011 decision 1
// keeps apart on purpose:
//
//   - Which shell invocations may run at all — an explicit, closed, exact-match
//     set of argv, because arbitrary model-authored shell is the thing most
//     worth stopping and there is no way to make that judgement from where a
//     command happens to run.
//   - Where a write outside the repo root may land — a declared containment
//     root, using the same [Resolver] and [contains] BenchPolicy already uses.
//
// So an approved shell command's Dir is not checked against the declared root
// here, unlike BenchPolicy's Dir confinement: folding the two together would
// make an argv's approval silently depend on its working directory, which
// would quietly turn "a closed, exact-match set" (decision 1's own words)
// into something wider than what was declared. ADR-0011 decision 4 is explicit
// that nothing in this package limits what an *approved* command can do once
// it starts — containment of the running process is the invoking
// orchestrator's job, not this policy's.
type AllowlistPolicy struct {
	root     string
	resolver Resolver
	allow    [][]string
}

// NewAllowlist builds the policy: root is the declared write-confinement
// scope, resolved once through resolver so containment is judged on
// comparable paths (mirroring [NewBench]); allow is the closed set of
// permitted shell argv, exact-match, in the shape [AllowlistFile.Allow]
// already validated syntactically.
//
// allow may be empty. That is not a misconfiguration to refuse — unlike
// internal/harness's `verify` key, where an empty array reads as "turn off a
// required gate" and is refused for exactly that reason, an empty allow list
// here is a legitimate declaration in its own right: "this invocation may
// write inside its declared root, and may never run a shell command at all."
func NewAllowlist(root string, resolver Resolver, allow [][]string) (*AllowlistPolicy, error) {
	if root == "" {
		return nil, errors.New("permission: root is required")
	}
	if resolver == nil {
		return nil, errors.New("permission: resolver is required")
	}
	abs, err := resolver.Resolve(root)
	if err != nil {
		return nil, fmt.Errorf("permission: resolving root %q: %w", root, err)
	}

	cp := make([][]string, len(allow))
	for i, argv := range allow {
		if len(argv) == 0 {
			return nil, fmt.Errorf("permission: allow[%d] is empty; an allowed command needs at least argv[0]", i)
		}
		cp[i] = append([]string(nil), argv...)
	}
	return &AllowlistPolicy{root: abs, resolver: resolver, allow: cp}, nil
}

// Root returns the resolved root the policy confines writes to.
func (p *AllowlistPolicy) Root() string { return p.root }

// Decide answers without a human, mirroring [BenchPolicy.Decide]'s shape:
// every branch that cannot establish safety denies, and every answer is
// [SourcePolicy] because there is no human here to attribute one to.
func (p *AllowlistPolicy) Decide(_ context.Context, req Request) (Decision, error) {
	switch req.Kind {
	case KindRunShell:
		if len(req.Action.Command) == 0 {
			return Decision{
				Verdict: VerdictDeny,
				Source:  SourcePolicy,
				Reason:  "shell command has no argv",
			}, nil
		}
		if !p.commandAllowed(req.Action.Command) {
			return Decision{
				Verdict: VerdictDeny,
				Source:  SourcePolicy,
				Reason:  fmt.Sprintf("%q is not on the declared allowlist", req.Action.Command),
			}, nil
		}
		return Decision{
			Verdict: VerdictAllow,
			Source:  SourcePolicy,
			Reason:  "command matches the declared allowlist exactly",
		}, nil

	case KindWriteOutsideRoot:
		if req.Action.Path == "" {
			return Decision{
				Verdict: VerdictDeny,
				Source:  SourcePolicy,
				Reason:  "write has no path",
			}, nil
		}
		return p.confine(req.Action.Path, "path")

	case KindUnspecified:
		return Decision{
			Verdict: VerdictDeny,
			Source:  SourcePolicy,
			Reason:  "request carries no kind",
		}, nil

	default:
		// A kind added later and not considered here is refused, not waved
		// through on the assumption that it resembles one of the above — the
		// same default-deny BenchPolicy.Decide holds to.
		return Decision{
			Verdict: VerdictDeny,
			Source:  SourcePolicy,
			Reason:  fmt.Sprintf("no allowlist rule for %s", req.Kind),
		}, nil
	}
}

// commandAllowed reports whether argv exact-matches one of the declared
// commands. Exact-match only — no prefix, no subcommand, no argument
// reordering — is the same discipline [VerdictAllowSession]'s own doc comment
// argues for and ADR-0008 already gave the reason for: a prefix or "close
// enough" grant is the version of this feature that quietly becomes "allow
// everything."
func (p *AllowlistPolicy) commandAllowed(argv []string) bool {
	for _, allowed := range p.allow {
		if slices.Equal(allowed, argv) {
			return true
		}
	}
	return false
}

// confine allows path only if it resolves inside the declared root. It is
// [BenchPolicy.confine] verbatim, on this type's own root and resolver: two
// policies re-deriving the identical containment check against two different
// roots is what "a new caller of the existing [Resolver]/[contains]
// mechanism, not a new mechanism" (ADR-0011 decision 1) means concretely.
func (p *AllowlistPolicy) confine(path, what string) (Decision, error) {
	abs, err := p.resolver.Resolve(path)
	if err != nil {
		return Decision{}, fmt.Errorf("resolving %s %q: %w", what, path, err)
	}
	if !contains(p.root, abs) {
		return Decision{
			Verdict: VerdictDeny,
			Source:  SourcePolicy,
			Reason:  fmt.Sprintf("%s %s is outside the declared root %s", what, abs, p.root),
		}, nil
	}
	return Decision{
		Verdict: VerdictAllow,
		Source:  SourcePolicy,
		Reason:  fmt.Sprintf("%s is inside the declared root", what),
	}, nil
}
