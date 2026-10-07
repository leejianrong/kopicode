package engine

import (
	"context"
	"fmt"
	"slices"

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
// the whole of what crosses (internal/permission's package doc, AGENTS.md).
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
	// Argv is the exact argv a shell action will run, element for element, and
	// nil for a write. Detail is this joined by single spaces, which loses where
	// each element's own spaces were; a surface that has to match or show the
	// command reads this instead of splitting Detail.
	Argv []string
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

// ConsentReply is what a [Consenter] returns: the answer, and for a refusal
// what the user would rather happen.
//
// Note is free text, not interpreted by the engine. It is journaled on the
// resulting PermissionDecided and put in front of the model in the denial it
// reads, which is the point: a bare "no" stops the work, and "no, use uv and a
// venv" redirects it. It means something only with [ConsentDeny]; the other
// answers drop it.
type ConsentReply struct {
	Answer ConsentAnswer
	Note   string
}

// Deny is the refusal reply, with an optional note.
func Deny(note string) ConsentReply { return ConsentReply{Answer: ConsentDeny, Note: note} }

// Reply wraps a bare answer, for a surface with nothing to add.
func Reply(a ConsentAnswer) ConsentReply { return ConsentReply{Answer: a} }

// Consenter answers a consent request.
//
// ctx is first because an interactive implementation blocks on a human, and a
// turn the user just interrupted must abandon the question rather than answer
// it. An implementation that cannot decide returns an error, which becomes a
// refusal — treating an unanswerable question as a yes is the failure the whole
// permission package exists to make impossible.
type Consenter func(ctx context.Context, req ConsentRequest) (ConsentReply, error)

// asker adapts a [Consenter] to internal/permission's own interface.
type asker struct{ consent Consenter }

// Ask puts the request to the surface.
//
// A nil Consenter refuses everything, and says why. That is the fail-closed
// direction and it is load-bearing: a front end that forgot to wire consent
// would otherwise get whichever behaviour the zero value happened to imply, and
// the one that runs model-authored shell unasked must never be reachable by
// forgetting a field.
func (a asker) Ask(ctx context.Context, req permission.Request) (permission.Reply, error) {
	deny := permission.Reply{Verdict: permission.VerdictDeny}
	if a.consent == nil {
		return deny, fmt.Errorf(
			"engine: no Consenter was supplied, so %s cannot be approved by anyone", req.Kind)
	}

	reply, err := a.consent(ctx, ConsentRequest{
		Kind:     req.Kind.String(),
		Tool:     req.Action.Tool,
		Detail:   req.Detail,
		Reason:   req.Reason,
		Resolved: req.Resolved,
		Argv:     slices.Clone(req.Action.Command),
	})
	if err != nil {
		return deny, err
	}

	switch reply.Answer {
	case ConsentAllow:
		return permission.Reply{Verdict: permission.VerdictAllow}, nil
	case ConsentAllowSession:
		return permission.Reply{Verdict: permission.VerdictAllowSession}, nil
	case ConsentDeny:
		return permission.Reply{Verdict: permission.VerdictDeny, Note: reply.Note}, nil
	default:
		// An answer nobody declared is not an approval.
		return deny, fmt.Errorf(
			"engine: surface returned %s, which is not an answer", reply.Answer)
	}
}
