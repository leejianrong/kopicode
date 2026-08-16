package engine

import (
	"context"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/permission"
)

// EventOfForTest exposes the journal-to-surface mapper to the external test
// package.
//
// The mapper is unexported because a caller must never build an [Event] out of
// a journal event by hand — the whole point of the tee is that the record is
// written first and only what it accepted is announced, and an exported mapper
// is an invitation to announce something that was not. What the test needs is
// the mapping itself, one payload at a time, so that "every payload type
// reaches a surface" can be checked without running a session per type.
func EventOfForTest(ev journal.Event) Event { return eventOf(ev) }

// AskForTest exposes the [Consenter]-to-permission.Asker adapter.
//
// The adapter is unexported because it is an implementation detail of [Open]:
// a surface supplies a Consenter and never sees a permission.Verdict, which is
// the whole reason ConsentAnswer exists. What the test needs is the half of it
// no session can reach — a nil Consenter, an answer outside the enumeration —
// because those are exactly the cases a session cannot be built to produce and
// exactly the cases that must fail closed.
//
// The verdict comes back as its wire string rather than as a permission.Verdict
// so the external test package does not have to import internal/permission to
// read it — the same boundary a front end lives behind, which keeps the test
// honest about what it is checking.
func AskForTest(ctx context.Context, c Consenter, req ConsentRequest) (string, error) {
	kinds := map[string]permission.Kind{
		"run_shell":          permission.KindRunShell,
		"write_outside_root": permission.KindWriteOutsideRoot,
	}
	v, err := asker{consent: c}.Ask(ctx, permission.Request{
		Kind:     kinds[req.Kind],
		Reason:   req.Reason,
		Detail:   req.Detail,
		Resolved: req.Resolved,
		Action:   permission.Action{Tool: req.Tool},
	})
	return v.String(), err
}
