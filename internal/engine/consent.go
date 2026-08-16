package engine

import (
	"context"
	"fmt"

	"github.com/leejianrong/kopicode/internal/permission"
)

// ConsentRequest is a permission request as a surface needs it.
//
// It exists for the reason [Event] does: a front end may not import
// internal/permission (ADR-0003 decision 3), so permission.Request cannot cross
// the boundary. The fields are that type's, copied, and the strings are the
// values journal.PermissionRequested documents — Kind is "run_shell" or
// "write_outside_root".
//
// Nothing here is about presentation. The engine decides *that* consent is
// required and what an answer means; wording, colour, key bindings and whether
// the question is even a line prompt belong to the surface, and this type is
// the whole of what crosses (internal/permission's package doc, CLAUDE.md).
type ConsentRequest struct {
	// Kind names the rule that fired.
	Kind string
	// Tool is the tool name as the model called it.
	Tool string
	// Detail is the thing being consented to: the command line, or the path.
	Detail string
	// Reason states the rule in one sentence.
	Reason string
	// Resolved is the absolute, symlink-followed path a write targets, empty
	// for a shell action. Showing it rather than the model's spelling is the
	// difference between consenting to "../../etc/hosts" and consenting to
	// "/etc/hosts".
	Resolved string
}

// ConsentAnswer is what a surface answered.
type ConsentAnswer uint8

const (
	// ConsentDeny refuses. It is the zero value, so a surface that fell
	// through a switch, or a caller that dropped the value on an error path,
	// still fails closed.
	ConsentDeny ConsentAnswer = iota
	// ConsentAllow permits this action and only this one.
	ConsentAllow
	// ConsentAllowSession permits this action and later ones with the same kind
	// and the same detail, for the life of the session. Exact match and nothing
	// wider — a prefix or directory grant is the version of this that quietly
	// becomes "allow everything", and internal/permission does not offer it.
	ConsentAllowSession
)

// String returns the journal wire value: "deny", "allow" or "allow_session".
func (a ConsentAnswer) String() string {
	switch a {
	case ConsentDeny:
		return "deny"
	case ConsentAllow:
		return "allow"
	case ConsentAllowSession:
		return "allow_session"
	default:
		return fmt.Sprintf("consent_answer(%d)", uint8(a))
	}
}

// Consenter answers a consent request.
//
// ctx is first because an interactive implementation blocks on a human, and a
// turn the user just interrupted must abandon the question rather than answer
// it. An implementation that cannot decide returns an error, which becomes a
// refusal — treating an unanswerable question as a yes is the failure the whole
// permission package exists to make impossible.
type Consenter func(ctx context.Context, req ConsentRequest) (ConsentAnswer, error)

// asker adapts a [Consenter] to internal/permission's own interface.
type asker struct{ consent Consenter }

// Ask puts the request to the surface.
//
// A nil Consenter refuses everything, and says why. That is the fail-closed
// direction and it is load-bearing: a front end that forgot to wire consent
// would otherwise get whichever behaviour the zero value happened to imply, and
// the one that runs model-authored shell unasked must never be reachable by
// forgetting a field.
func (a asker) Ask(ctx context.Context, req permission.Request) (permission.Verdict, error) {
	if a.consent == nil {
		return permission.VerdictDeny, fmt.Errorf(
			"engine: no Consenter was supplied, so %s cannot be approved by anyone", req.Kind)
	}

	answer, err := a.consent(ctx, ConsentRequest{
		Kind:     req.Kind.String(),
		Tool:     req.Action.Tool,
		Detail:   req.Detail,
		Reason:   req.Reason,
		Resolved: req.Resolved,
	})
	if err != nil {
		return permission.VerdictDeny, err
	}

	switch answer {
	case ConsentAllow:
		return permission.VerdictAllow, nil
	case ConsentAllowSession:
		return permission.VerdictAllowSession, nil
	case ConsentDeny:
		return permission.VerdictDeny, nil
	default:
		// An answer nobody declared is not an approval.
		return permission.VerdictDeny, fmt.Errorf(
			"engine: surface returned %s, which is not an answer", answer)
	}
}
