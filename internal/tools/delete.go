package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
)

// DeleteRequest is one delete_file call.
type DeleteRequest struct {
	// Path is the file to delete, relative to the repository root or absolute.
	// It must exist and must be a regular file.
	Path string
}

// DeleteResult is what one delete_file call produced.
//
// It is a struct and not a rendered string for the same reason [WriteResult]
// is: Cancelled is what tells an abandoned turn apart from a successful
// delete, and a caller may want to act on that without parsing prose. Output
// is the model-facing rendering of the rest.
type DeleteResult struct {
	// Path is the file deleted, as a slash path relative to the root. On a
	// cancellation it is the argument as given, because nothing was resolved.
	Path string

	// Cancelled reports that the context was cancelled before the delete
	// happened, so nothing was removed. The call also returns a
	// [FaultCancelled] error saying the same thing — this field is for the
	// caller, that one is for the classifier. See [Set.WriteFile]'s doc
	// comment for why both exist.
	Cancelled bool

	// Output is the model-facing rendering of everything above.
	Output string
}

// DeleteFile removes a single file.
//
// **Containment works the same way it does for every other tool.**
// [Root.Resolve] proves the argument lies inside the repository root before
// anything is touched, and every disk operation after that goes through the
// os.Root handle, which refuses to traverse out of the root at the syscall
// level even if Resolve had somehow let something through. The classic escape
// [Set.WriteFile] documents applies here unchanged:
//
//	root/safe -> /etc        a symlink already on disk
//	delete_file("safe/passwd")
//
// "safe/passwd" is lexically inside the root and resolves outside it, so the
// call is refused with [ErrOutsideRoot] and nothing is removed.
//
// **This tool deletes files, not directories.** A directory argument is
// refused with [ErrNotRegular], mirroring [Set.WriteFile]'s refusal to write
// one. Recursive removal is a much larger blast radius for one bad path, and
// nothing in the harness needs it yet — a model that wants a directory gone
// can delete its files one at a time, which is also what keeps every deletion
// individually visible in the journal.
//
// **A missing path is a task failure, not a harness one.** The model asking
// to delete something that is not there — because it was already removed, or
// the model misremembered the name — is a normal, model-observable outcome,
// exactly like [Set.ReadFile] refusing a path that does not exist.
//
// **A cancellation is a result *and* a [FaultCancelled] error**, for the same
// reason [Set.WriteFile] returns both: the result tells the model nothing was
// deleted, and the error keeps a Ctrl-C out of both the harness and the model
// buckets (KAN-808).
func (s *Set) DeleteFile(ctx context.Context, req DeleteRequest) (DeleteResult, error) {
	if err := ctx.Err(); err != nil {
		return cancelledDelete(req.Path), cancelledErr(ToolDeleteFile, req.Path, err, "nothing was deleted")
	}

	p, err := s.Root.Resolve(ToolDeleteFile, req.Path)
	if err != nil {
		return DeleteResult{}, err
	}
	if p.Rel == "." {
		return DeleteResult{}, taskErr(ToolDeleteFile, req.Path, ErrNotRegular,
			"it is the repository root, which is a directory")
	}

	switch info, serr := s.Root.dir.Stat(p.Rel); {
	case serr == nil && info.IsDir():
		return DeleteResult{}, taskErr(ToolDeleteFile, req.Path, ErrNotRegular,
			"it is a directory; delete_file deletes files")
	case serr == nil && !info.Mode().IsRegular():
		return DeleteResult{}, taskErr(ToolDeleteFile, req.Path, ErrNotRegular,
			fmt.Sprintf("its mode is %s", info.Mode()))
	case serr != nil:
		return DeleteResult{}, s.deleteErr(req.Path, serr)
	}

	// The last check before anything on disk changes. After the delete there
	// is nothing to cancel: reporting a completed delete as cancelled would
	// leave the model believing a file it removed is still there.
	if err := ctx.Err(); err != nil {
		return cancelledDelete(req.Path), cancelledErr(ToolDeleteFile, req.Path, err, "nothing was deleted")
	}

	if err := s.Root.dir.Remove(p.Rel); err != nil {
		return DeleteResult{}, s.deleteErr(req.Path, err)
	}

	res := DeleteResult{Path: p.Slash()}
	res.Output = res.render()
	return res, nil
}

// cancelledDelete is the result of a call the context ended before it deleted
// anything.
func cancelledDelete(given string) DeleteResult {
	r := DeleteResult{Path: given, Cancelled: true}
	r.Output = r.render()
	return r
}

// deleteErr classifies a failure from the root handle on the delete path.
//
// It is separate from [Set.handleErr] because a missing path here is the
// model asking to delete something that is not there, which deserves a
// message about deletion, not "no such file or directory" copied from a
// read. The default arm agrees with handleErr and writeErr: Resolve already
// proved this path inside the root, so an unexplained refusal is the
// harness's problem, and calling it a task failure would hide a harness
// limitation inside a model score.
func (s *Set) deleteErr(given string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return taskErr(ToolDeleteFile, given, err, "no such file; there is nothing to delete")
	case errors.Is(err, fs.ErrPermission):
		return taskErr(ToolDeleteFile, given, err, "the path is not writable")
	default:
		return internalErr(ToolDeleteFile, given, err,
			"the root handle refused a path that resolved inside the root")
	}
}

// render is the model-facing form:
//
//	delete_file test_bug.js: deleted
func (r DeleteResult) render() string {
	if r.Cancelled {
		return fmt.Sprintf("delete_file %s: cancelled before deleting; the file was not changed\n", r.Path)
	}
	return fmt.Sprintf("delete_file %s: deleted\n", r.Path)
}
