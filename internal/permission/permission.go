// Package permission decides whether an action the model proposed needs
// consent, and what the answer means.
//
// The line this package exists to hold is the one CLAUDE.md states as a
// structural promise: the engine decides *that* permission is required and what
// a decision means, never *how* it is asked. So nothing here formats a prompt,
// reads a key, writes to a terminal or knows that a terminal exists. The REPL
// renders a [Request] however it likes; the bench runner answers one without a
// human. Both sit on top of a [Gate] that cannot tell which of them it is
// talking to.
//
// The concrete failure that separation prevents: if the classification lived
// next to the prompt, the headless bench runner would inherit an interactive
// dependency it cannot satisfy, and the usual fix — a "non-interactive mode"
// flag that approves everything — is how a corpus run escapes its task's
// worktree and contaminates the next task's result.
//
// # Shape
//
// Policy in, request out, decision in, outcome out:
//
//	g, err := permission.New(root, resolver, policy)
//	out, err := g.Check(ctx, permission.Action{...})
//
// [Gate.Check] classifies the action, and if consent is required hands the
// resulting [Request] to the [Policy] value it was built with. The policy
// answers with a [Decision]. The [Outcome] carries both, so the engine can
// journal PermissionRequested and PermissionDecided from it.
//
// # Fail closed
//
// A gate whose default is "allow" is not a gate. Every path that cannot reach a
// positive answer denies: an operation nobody classified, a malformed action, a
// path the resolver cannot resolve, a cancelled context, a policy that returns
// nothing. Two properties make that hard to get wrong at a call site:
//
//   - The zero [Outcome] is a denial, so a caller who drops the value on error
//     still fails closed.
//   - A non-allow always returns a non-nil error wrapping [ErrDenied], so a
//     caller who checks only the error still fails closed.
//
// Consent denied is therefore an error rather than a quiet false. It is a
// routine, expected error — the engine turns it into an observation the model
// sees — but it is never silence.
//
// # What this package does not import
//
// Not internal/journal. The engine journals, this package decides, and that
// arrow points one way. What this package owes in exchange is that its
// vocabulary lines up with journal.PermissionRequested and
// journal.PermissionDecided exactly: [Kind], [Verdict] and [Source] stringify
// to the values those events document, and the engine copies them across
// without a translation table that could drift.
package permission

import (
	"errors"
	"fmt"
)

// ErrDenied is the sentinel every refusal wraps, whatever the reason: a human
// said no, a non-interactive policy refused, or the gate could not establish
// that the action was safe. Callers compare with errors.Is.
//
// It is deliberately one sentinel rather than one per cause. The call site's
// question is "may I proceed", which has one negative answer; the cause is for
// the journal and the message, not for control flow.
var ErrDenied = errors.New("permission denied")

// ErrUnknownOperation is joined onto the refusal when an [Action] carries an
// [Operation] the classifier does not handle. It is the deliberately unhandled
// case: adding an operation without classifying it denies every use of it and
// fails the suite, rather than defaulting silently to allow.
var ErrUnknownOperation = errors.New("unclassified operation")

// ErrInvalidAction is joined onto the refusal when an [Action] is missing the
// field its operation is about — a write with no path, a shell call with no
// argv. A malformed request is not a request to interpret generously.
var ErrInvalidAction = errors.New("malformed action")

// ErrNoDecision is joined onto the refusal when a [Policy] returns
// [VerdictUnspecified] — the zero value. A policy that answers nothing has not
// answered "yes".
var ErrNoDecision = errors.New("policy returned no decision")

// Operation is what a tool wants to do, at the granularity policy
// distinguishes. It is not the tool name: read_file and grep are both
// [OperationRead], and a future tool that shells out is [OperationShell]
// whatever it is called.
//
// The zero value is [OperationUnspecified], which is never classified. That is
// intentional: an Action built by a caller who forgot to set the field is
// denied rather than treated as a read.
type Operation uint8

const (
	// OperationUnspecified is the zero value and is never allowed.
	OperationUnspecified Operation = iota

	// OperationRead observes the tree without changing it: read_file,
	// list_dir, grep.
	OperationRead

	// OperationWrite changes the tree: write_file, edit_file.
	OperationWrite

	// OperationShell runs a subprocess with model-authored argv.
	OperationShell
)

// operationText is also the enumeration the internal classifier guard walks. An
// operation added here but not classified fails that test.
var operationText = map[Operation]string{
	OperationUnspecified: "unspecified",
	OperationRead:        "read",
	OperationWrite:       "write",
	OperationShell:       "shell",
}

// String returns the operation's name, or a form naming the numeric value when
// it is not one of the declared constants.
func (o Operation) String() string {
	if s, ok := operationText[o]; ok {
		return s
	}
	return fmt.Sprintf("operation(%d)", uint8(o))
}

// Kind names why consent was required. It is the value the engine writes to
// journal.PermissionRequested.Kind, and the strings here are that event's
// documented wire values.
type Kind uint8

const (
	// KindUnspecified is the zero value; no request carries it.
	KindUnspecified Kind = iota

	// KindRunShell is model-authored shell. It always requires consent —
	// arbitrary argv is the single thing most worth stopping, and there is no
	// location that makes it routine.
	KindRunShell

	// KindWriteOutsideRoot is a write whose resolved target lies outside the
	// repo root. Inside the root, writes are the agent's job and asking about
	// each one trains the user to approve without reading.
	KindWriteOutsideRoot
)

var kindText = map[Kind]string{
	KindUnspecified:      "unspecified",
	KindRunShell:         "run_shell",
	KindWriteOutsideRoot: "write_outside_root",
}

// String returns the journal wire value for the kind.
func (k Kind) String() string {
	if s, ok := kindText[k]; ok {
		return s
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// Verdict is a policy's answer. It is the value the engine writes to
// journal.PermissionDecided.Decision, and the strings here are that event's
// documented wire values.
type Verdict uint8

const (
	// VerdictUnspecified is the zero value. A policy returning it has not
	// decided, and the gate denies with [ErrNoDecision].
	VerdictUnspecified Verdict = iota

	// VerdictDeny refuses this action.
	VerdictDeny

	// VerdictAllow permits this action and only this one.
	VerdictAllow

	// VerdictAllowSession permits this action and later actions with the same
	// kind and the same detail, for the life of the gate. The scope is exact
	// match and nothing wider: consenting to `go test ./...` does not consent
	// to `go test; rm -rf .`, and consenting to a write at one path does not
	// consent to its directory. A prefix or directory grant is the version of
	// this feature that quietly becomes "allow everything", so it is not
	// offered.
	VerdictAllowSession
)

var verdictText = map[Verdict]string{
	VerdictUnspecified:  "unspecified",
	VerdictDeny:         "deny",
	VerdictAllow:        "allow",
	VerdictAllowSession: "allow_session",
}

// String returns the journal wire value for the verdict.
func (v Verdict) String() string {
	if s, ok := verdictText[v]; ok {
		return s
	}
	return fmt.Sprintf("verdict(%d)", uint8(v))
}

// Source records who answered. It is the value the engine writes to
// journal.PermissionDecided.Source.
//
// The distinction is load bearing for the benchmark: a bench run's
// auto-approval must not be indistinguishable from a human saying yes, or a
// scored session can claim a consent that never happened.
type Source uint8

const (
	// SourceUnspecified is the zero value; a decision carrying it is not
	// attributable and the gate denies.
	SourceUnspecified Source = iota

	// SourceUser is a human's answer, relayed by a surface.
	SourceUser

	// SourcePolicy is the harness answering without a human.
	SourcePolicy
)

var sourceText = map[Source]string{
	SourceUnspecified: "unspecified",
	SourceUser:        "user",
	SourcePolicy:      "policy",
}

// String returns the journal wire value for the source.
func (s Source) String() string {
	if t, ok := sourceText[s]; ok {
		return t
	}
	return fmt.Sprintf("source(%d)", uint8(s))
}

// Action is what a tool is about to do, described in the terms policy reasons
// about. The engine builds one per tool dispatch, before the tool runs.
type Action struct {
	// ID pairs the journal's PermissionRequested with its PermissionDecided.
	// The caller supplies it — the engine already mints a tool call id, and
	// reusing it is what lets a reader tie a consent to the call it gated.
	// This package mints no identifiers of its own, which is also what keeps
	// it free of a clock and an RNG.
	ID string

	// Tool is the tool name as the model called it: "read_file",
	// "write_file", "run_shell". It is recorded and rendered, never switched
	// on — [Operation] is what policy reasons about.
	Tool string

	// Operation is the classification policy acts on.
	Operation Operation

	// Path is the target of a read or a write. It may be relative, and for a
	// write it may not exist yet. Resolution is the [Resolver]'s job.
	Path string

	// Command is a shell action's argv — not a shell string, so what was
	// consented to is unambiguous on replay.
	Command []string

	// Dir is the working directory a shell action runs in. It is required:
	// an empty Dir means the process's own working directory, which is
	// global state and exactly the way a bench task escapes its worktree.
	Dir string
}

// Request is the consent request: what the gate decided must be asked, and why.
// The engine journals it as PermissionRequested; a surface renders it however
// it likes.
type Request struct {
	// ID is [Action.ID], echoed.
	ID string

	// Kind is why consent is required.
	Kind Kind

	// Reason states the rule that fired, in one sentence, for the journal and
	// for whatever the surface shows. It explains the policy, not the action.
	Reason string

	// Detail is the thing being consented to, rendered for the journal: the
	// command line, or the resolved path. A surface is free to render the
	// [Action] differently; this field exists so the audit record does not
	// depend on which surface was attached.
	Detail string

	// Resolved is the absolute, symlink-followed path a read or write targets,
	// empty for a shell action. It is what containment was judged on, and
	// showing it rather than the model's spelling is the difference between a
	// user consenting to "../../etc/hosts" and consenting to "/etc/hosts".
	Resolved string

	// Action is the full action, so a policy can re-derive anything it needs
	// without parsing Detail.
	Action Action
}

// Decision is a policy's answer, with attribution. It maps one-to-one onto
// journal.PermissionDecided.
type Decision struct {
	Verdict Verdict
	Source  Source

	// Reason is optional, and is the answerer's reason rather than the
	// policy's rule: "outside the task worktree", "user declined".
	Reason string
}

// Outcome is what [Gate.Check] returns.
//
// Its zero value is a denial that required no consent — the safe reading for a
// caller that dropped it on an error path.
type Outcome struct {
	// Allowed is true only when the action may proceed. It is true exactly
	// when Check returned a nil error.
	Allowed bool

	// Required reports whether the action needed consent at all. When it is
	// false, Request and Decision are zero and the engine journals nothing:
	// a permission event per read would bury the events that matter.
	Required bool

	// Request is the consent request, valid when Required is true.
	Request Request

	// Decision is the answer, valid when Required is true. A session grant
	// answers with [VerdictAllow] and [SourcePolicy] rather than replaying the
	// original [VerdictAllowSession], so every gated action carries its own
	// pair of events and the audit shows each shell command rather than only
	// the first.
	Decision Decision
}

// Resolver turns a path a tool was handed into an absolute path with every
// symlink followed and every ".." collapsed.
//
// It is an interface, and a small one, because containment is not string
// comparison and this package must not own the answer. Three cases a correct
// implementation has to survive:
//
//   - "../../etc/hosts" resolves outside the root even though it reads like a
//     path under it.
//   - a symlink inside the root pointing outside it resolves outside.
//   - a path that does not exist yet resolves anyway, because a write creates
//     its target. Resolving the nearest existing ancestor and re-appending the
//     remainder is the usual way; returning an error is not, because it would
//     deny every new file.
//
// The production implementation is tools.Resolver, built with
// tools.Root.Resolver (KAN-810); the engine supplies it at the call site. It is
// not imported here: this package is consumed by the engine, and a third
// implementation of containment is how two of them end up disagreeing.
//
// The signature returns a string rather than the validated tools.Path, and that
// was weighed rather than defaulted to. tools.Path exists only for a path proven
// inside one specific root, and the two things this package must resolve are a
// write that may be *outside* the root — which is the whole of
// [KindWriteOutsideRoot] — and a path judged against the bench task's worktree,
// which is not that root at all. A return type that cannot represent a path
// outside the root cannot answer either question, so the richer type is not
// available here; what replaces it is that the string is only ever judged and
// never opened. Opening goes back through tools.Root.Resolve and the os.Root
// handle.
//
// No context parameter: this is a path computation over local filesystem
// metadata, not something a user waits on. [Policy.Decide] is the call that
// blocks.
type Resolver interface {
	Resolve(path string) (string, error)
}

// denied builds the refusal error every non-allow path returns. Both sentinels
// wrap, so errors.Is finds [ErrDenied] for the control-flow question and the
// specific cause for the message.
func denied(cause error, format string, args ...any) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrDenied, fmt.Sprintf(format, args...))
	}
	return fmt.Errorf("%w: %s: %w", ErrDenied, fmt.Sprintf(format, args...), cause)
}
