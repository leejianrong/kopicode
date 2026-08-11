package permission

import (
	"context"
	"errors"
	"fmt"
)

// Policy answers the requests classification decided must be asked.
//
// It is a value rather than a branch inside the gate because two answerers
// exist on the first day — a human at a REPL and a bench run with no human at
// all — and a third is foreseeable (a policy file, a --yes flag, a
// deny-everything dry run). A gate that switched on "am I interactive" would
// have to be edited for each of them, and the edit that adds "and if we are
// headless, approve" is the one that breaks worktree isolation.
type Policy interface {
	// Decide answers req.
	//
	// ctx is first because an interactive implementation blocks on a human,
	// and a cancelled turn must abandon the question rather than answer it. An
	// implementation that cannot decide returns an error; the gate turns that
	// into a refusal.
	Decide(ctx context.Context, req Request) (Decision, error)
}

// Asker is the surface's half of the interactive path: it puts a request to a
// human and returns the answer.
//
// This is the whole of what a surface implements. Everything about how the
// question looks — wording, colour, key bindings, whether it is a line prompt
// or something else entirely — lives behind this interface, which is why this
// package can be linked into a headless binary without dragging a terminal in
// with it.
type Asker interface {
	Ask(ctx context.Context, req Request) (Verdict, error)
}

// AskPolicy is the interactive policy: it forwards every request to an [Asker]
// and attributes the answer to the user.
type AskPolicy struct {
	asker Asker
}

// NewAsk builds an interactive policy over asker.
func NewAsk(asker Asker) (*AskPolicy, error) {
	if asker == nil {
		return nil, errors.New("permission: asker is required")
	}
	return &AskPolicy{asker: asker}, nil
}

// Decide asks the human.
//
// The answer is stamped [SourceUser] and nothing else, including when it is a
// refusal: a decision that arrived through this path was made by a person, and
// the journal has to be able to say so. An asker that fails — a closed stdin, a
// cancelled turn — produces an error, which the gate turns into a refusal.
// Treating an unanswerable question as a yes is the failure mode this whole
// package exists to make impossible.
func (p *AskPolicy) Decide(ctx context.Context, req Request) (Decision, error) {
	v, err := p.asker.Ask(ctx, req)
	if err != nil {
		return Decision{}, fmt.Errorf("asking about %s: %w", req.Kind, err)
	}
	return Decision{Verdict: v, Source: SourceUser}, nil
}

// BenchPolicy is the non-interactive policy the bench runner supplies. It
// approves what falls inside the task's worktree and refuses everything else.
//
// "And refuses everything else" is the point of it. The obvious headless policy
// — approve everything, there is nobody to ask — lets a task's shell command or
// out-of-root write reach the checkout of the next task, or the corpus itself,
// and a corpus run whose tasks can touch each other is not producing paired
// measurements any more. So this policy re-derives containment itself against
// the worktree it was given, rather than trusting that the gate's root and the
// worktree are the same directory.
//
// It is not a sandbox. It gates the action the model asked for; a shell command
// approved because it starts in the worktree can still walk out of it. Real
// isolation is a container, and docs/SLICE-1.md names that as a known gap for a
// later slice rather than pretending this closes it.
type BenchPolicy struct {
	worktree string
	resolver Resolver
}

// NewBench builds the bench policy for a task worktree.
//
// The worktree is resolved once through the same resolver the gate uses, so
// containment is judged on comparable paths.
func NewBench(worktree string, resolver Resolver) (*BenchPolicy, error) {
	if worktree == "" {
		return nil, errors.New("permission: worktree is required")
	}
	if resolver == nil {
		return nil, errors.New("permission: resolver is required")
	}
	abs, err := resolver.Resolve(worktree)
	if err != nil {
		return nil, fmt.Errorf("permission: resolving worktree %q: %w", worktree, err)
	}
	return &BenchPolicy{worktree: abs, resolver: resolver}, nil
}

// Worktree returns the resolved worktree the policy approves within.
func (p *BenchPolicy) Worktree() string { return p.worktree }

// Decide answers without a human.
//
// Every answer is [SourcePolicy]. A bench result that recorded an auto-approval
// as a user's consent would be claiming evidence of something that never
// happened.
//
// It never returns [VerdictAllowSession]: standing consent is a convenience for
// a human being asked repeatedly, and there is no human here. Answering each
// request on its own merits also means every gated action appears in the
// journal with its own decision, which is what the failure classifier reads.
func (p *BenchPolicy) Decide(_ context.Context, req Request) (Decision, error) {
	switch req.Kind {
	case KindRunShell:
		// An empty working directory means the runner's own, which is
		// process-global state shared by every task in the corpus. That is
		// precisely the leak, so it is refused rather than defaulted.
		if req.Action.Dir == "" {
			return Decision{
				Verdict: VerdictDeny,
				Source:  SourcePolicy,
				Reason:  "shell command has no working directory",
			}, nil
		}
		return p.confine(req.Action.Dir, "working directory")

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
		// through on the assumption that it resembles one of the above.
		return Decision{
			Verdict: VerdictDeny,
			Source:  SourcePolicy,
			Reason:  fmt.Sprintf("no bench rule for %s", req.Kind),
		}, nil
	}
}

// confine allows path only if it resolves inside the worktree.
func (p *BenchPolicy) confine(path, what string) (Decision, error) {
	abs, err := p.resolver.Resolve(path)
	if err != nil {
		return Decision{}, fmt.Errorf("resolving %s %q: %w", what, path, err)
	}
	if !contains(p.worktree, abs) {
		return Decision{
			Verdict: VerdictDeny,
			Source:  SourcePolicy,
			Reason:  fmt.Sprintf("%s %s is outside the task worktree %s", what, abs, p.worktree),
		}, nil
	}
	return Decision{
		Verdict: VerdictAllow,
		Source:  SourcePolicy,
		Reason:  fmt.Sprintf("%s is inside the task worktree", what),
	}, nil
}
