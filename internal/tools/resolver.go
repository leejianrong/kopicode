package tools

import "errors"

// Resolver answers one question — "where does this path really point?" — and
// nothing else. It is what internal/permission's Resolver interface names as
// its intended implementation, and the value that satisfies it (KAN-810).
//
// Nothing here imports internal/permission. The interface is declared where it
// is consumed, this is the value that fits it, and the engine puts the two
// together. That is also why the signature is the plain one: see the note on
// the return type below.
//
// # Why this is not [Root.Resolve]
//
// Root.Resolve does two jobs — it resolves *and* it contains, refusing anything
// that lands outside the root with [ErrOutsideRoot]. The permission gate's whole
// question about a write is whether it landed outside the root, and SLICE-1 §10
// says such a write must *ask*. A gate handed Root.Resolve would get an error
// where it needed a path, deny the write outright, and never ask — the rule
// silently unimplemented, and the same for the bench policy, which judges
// containment against the task worktree rather than against this root at all.
//
// So the resolution half is shared with Root.Resolve and the containment half
// is not. There is no second link walk and no second containment check here.
//
// # What it gives up, stated rather than hidden
//
// [Root.Resolve] returns a [Path], which is a *validated* value: it exists only
// for a path proven inside the root. This returns a bare string, and the
// difference is real. The string is a path to **judge**, never a path to open.
// Opening still goes through Root.Resolve and then the os.Root handle, which is
// the syscall-level enforcement described on [Root], so nothing about this type
// widens what the tools will touch.
//
// A [Path] could not be returned here even in principle: it asserts containment
// in one specific root, and a resolver that can only describe paths inside the
// root cannot describe the path the gate has to ask about.
//
// The zero Resolver has no root and refuses every call rather than panicking;
// build one with [Root.Resolver].
type Resolver struct{ root *Root }

// Resolver returns the [Resolver] over r.
func (r *Root) Resolver() Resolver { return Resolver{root: r} }

// Resolve makes path absolute — a relative path against the root — follows
// every symlink in it, and tolerates a tail that does not exist yet, because a
// write creates its target.
//
// It never refuses a path for being outside the root. That is the caller's
// judgement to make, and the reason this type exists.
//
// An empty path is an error, which is one deliberate divergence from
// [Root.Resolve]: there, "" means the root and a tool goes on to fail on it
// harmlessly; here, "" resolving to the root would be a missing argument
// silently judged "inside the repository" and allowed. A gate that fails closed
// cannot have that.
func (r Resolver) Resolve(path string) (string, error) {
	if r.root == nil {
		return "", errors.New("tools: resolver has no root; build one with Root.Resolver")
	}
	if path == "" {
		return "", errors.New("tools: cannot resolve an empty path")
	}
	return realPath(r.root.candidate(path))
}
