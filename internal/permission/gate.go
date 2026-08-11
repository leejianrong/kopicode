package permission

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// Gate applies the engine's classification rules and routes what is left to a
// [Policy].
//
// The rules are fixed and are the same on every surface, because they are a
// property of the product rather than of the front end:
//
//   - run_shell always asks. Model-authored argv is the thing worth stopping,
//     and no location makes it routine.
//   - A write whose resolved target is outside the repo root always asks.
//     Inside it, writing is the agent's job.
//   - Reads never ask. Whether a read outside the root is permitted at all is
//     the tool layer's containment check, not a consent question — asking about
//     every file the agent opens produces a user who approves without reading,
//     which costs the shell prompt its meaning.
//
// What varies between surfaces is only the answer, and that is the [Policy].
//
// A Gate is safe for concurrent use: the loop dispatches tools concurrently and
// session grants are shared state.
type Gate struct {
	root     string
	resolver Resolver
	policy   Policy

	mu     sync.Mutex
	grants map[grant]bool
}

// grant is the scope of a [VerdictAllowSession] answer: exact kind, exact
// detail. Widening this to a directory or a command prefix would turn one
// considered "yes" into standing consent for things the user never saw.
type grant struct {
	kind   Kind
	detail string
}

// New builds a gate rooted at root, resolving paths with resolver and
// answering with policy.
//
// root is resolved once, through the same resolver, so that a repo reached via
// a symlinked path is compared like for like — otherwise every write inside it
// looks like a write outside it, and the user is asked about all of them.
//
// A missing argument is an error rather than a permissive default: a gate built
// with no policy would have to invent an answer, and there is only one safe
// answer to invent.
func New(root string, resolver Resolver, policy Policy) (*Gate, error) {
	if root == "" {
		return nil, errors.New("permission: root is required")
	}
	if resolver == nil {
		return nil, errors.New("permission: resolver is required")
	}
	if policy == nil {
		return nil, errors.New("permission: policy is required")
	}
	abs, err := resolver.Resolve(root)
	if err != nil {
		return nil, fmt.Errorf("permission: resolving root %q: %w", root, err)
	}
	return &Gate{
		root:     abs,
		resolver: resolver,
		policy:   policy,
		grants:   map[grant]bool{},
	}, nil
}

// Root returns the resolved repo root the gate judges containment against.
func (g *Gate) Root() string { return g.root }

// Check decides whether a may proceed.
//
// The error is nil exactly when the outcome is allowed. Every other return
// wraps [ErrDenied], including the ordinary case of a user saying no, so a
// caller that checks only the error still fails closed — and the zero [Outcome]
// it would then be holding is itself a denial.
//
// ctx is first because this call blocks whenever consent is required and the
// policy asks a human. A cancelled context denies rather than proceeding: a
// turn the user just interrupted must not be the turn that runs the command.
func (g *Gate) Check(ctx context.Context, a Action) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{}, denied(err, "%s: cancelled before consent", a.Tool)
	}

	req, required, err := g.classify(a)
	if err != nil {
		return Outcome{}, err
	}
	if !required {
		return Outcome{Allowed: true}, nil
	}

	if g.granted(req) {
		return Outcome{
			Allowed:  true,
			Required: true,
			Request:  req,
			Decision: Decision{
				Verdict: VerdictAllow,
				Source:  SourcePolicy,
				Reason:  "already allowed for this session",
			},
		}, nil
	}

	dec, err := g.policy.Decide(ctx, req)
	if err != nil {
		return Outcome{}, denied(err, "%s: policy could not decide", a.Tool)
	}

	out := Outcome{Required: true, Request: req, Decision: dec}
	switch dec.Verdict {
	case VerdictAllow, VerdictAllowSession:
		// An unattributed approval is not an approval. Letting it through
		// would put a decision in the journal that no one can be held to.
		if dec.Source == SourceUnspecified {
			return Outcome{}, denied(ErrNoDecision, "%s: allowed by no one", a.Tool)
		}
		if dec.Verdict == VerdictAllowSession {
			g.record(req)
		}
		out.Allowed = true
		return out, nil
	case VerdictDeny:
		return out, denied(nil, "%s: %s", a.Tool, refusal(dec))
	case VerdictUnspecified:
		return Outcome{}, denied(ErrNoDecision, "%s", a.Tool)
	default:
		return Outcome{}, denied(ErrNoDecision, "%s: policy returned %s", a.Tool, dec.Verdict)
	}
}

// refusal renders a denial for the error message, falling back when the policy
// gave no reason.
func refusal(dec Decision) string {
	if dec.Reason == "" {
		return "refused"
	}
	return dec.Reason
}

// classify applies the fixed rules. It reports whether consent is required and,
// when it is, the request to put to the policy.
//
// Every branch that cannot establish safety returns an error rather than
// falling through to "no consent required". The default case is the one that
// matters: an [Operation] nobody classified is denied outright, so adding one
// without deciding its rule breaks every use of it loudly instead of letting it
// through silently.
func (g *Gate) classify(a Action) (Request, bool, error) {
	switch a.Operation {
	case OperationRead:
		return Request{}, false, nil

	case OperationWrite:
		if a.Path == "" {
			return Request{}, false, denied(ErrInvalidAction, "%s: write with no path", a.Tool)
		}
		abs, err := g.resolver.Resolve(a.Path)
		if err != nil {
			return Request{}, false, denied(err, "%s: resolving %q", a.Tool, a.Path)
		}
		if contains(g.root, abs) {
			return Request{}, false, nil
		}
		return Request{
			ID:       a.ID,
			Kind:     KindWriteOutsideRoot,
			Reason:   fmt.Sprintf("writes outside the repo root (%s) require consent", g.root),
			Detail:   abs,
			Resolved: abs,
			Action:   a,
		}, true, nil

	case OperationShell:
		if len(a.Command) == 0 {
			return Request{}, false, denied(ErrInvalidAction, "%s: shell with no command", a.Tool)
		}
		return Request{
			ID:     a.ID,
			Kind:   KindRunShell,
			Reason: "model-authored shell commands always require consent",
			Detail: strings.Join(a.Command, " "),
			Action: a,
		}, true, nil

	case OperationUnspecified:
		return Request{}, false, denied(ErrUnknownOperation, "%s: no operation set", a.Tool)

	default:
		return Request{}, false, denied(ErrUnknownOperation, "%s: %s", a.Tool, a.Operation)
	}
}

// granted reports whether a previous [VerdictAllowSession] covers req.
func (g *Gate) granted(req Request) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.grants[grant{kind: req.Kind, detail: req.Detail}]
}

// record stores a session grant.
func (g *Gate) record(req Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.grants[grant{kind: req.Kind, detail: req.Detail}] = true
}

// contains reports whether the resolved path p lies within the resolved
// directory root, root itself counting as inside.
//
// It goes through filepath.Rel rather than a string prefix test. A prefix test
// calls "/repo-backup/x" a path inside "/repo", and gets the root "/" wrong on
// every input. Both arguments must already be absolute and symlink-free — that
// is the [Resolver]'s contract, and this function cannot check it.
func contains(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
