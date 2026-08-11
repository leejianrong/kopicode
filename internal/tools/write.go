package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/leejianrong/kopicode/internal/anchor"
)

// WriteRequest is one write_file call.
type WriteRequest struct {
	// Path is the file to write, relative to the repository root or absolute.
	// It need not exist.
	Path string
	// Content is the file's new content, written verbatim. Nothing is appended,
	// trimmed or re-encoded: what the model sent is what lands on disk, so a
	// later read_file derives anchors over exactly the bytes it asked for.
	Content string
}

// WriteResult is what one write_file call produced.
//
// It is a struct and not a rendered string for the same reason [ShellResult]
// is: two of its fields are not prose. Replaced is the fact the engine may want
// to act on, and Cancelled is what tells an abandoned turn apart from a
// successful write. Output is the model-facing rendering of the rest.
type WriteResult struct {
	// Path is the file written, as a slash path relative to the root. On a
	// cancellation it is the argument as given, because nothing was resolved.
	Path string
	// Bytes is how many bytes were written.
	Bytes int
	// Lines is the line count, split the way read_file and grep split, so the
	// three agree on what a line is for a CRLF checkout.
	Lines int

	// Replaced reports that a file was already there and its content is gone.
	Replaced bool
	// ReplacedBytes is the size of what was overwritten, 0 when nothing was.
	ReplacedBytes int64
	// CreatedDirs are the parent directories brought into being, outermost
	// first, as slash paths relative to the root.
	CreatedDirs []string

	// Cancelled reports that the context was cancelled before the write
	// happened, so nothing was written. See the note on [Set.WriteFile] about
	// why this is a result rather than an error.
	Cancelled bool

	// Output is the model-facing rendering of everything above.
	Output string
}

// defaultFileMode and defaultDirMode are the modes new files and new
// directories get. An existing file keeps its own mode, because os.WriteFile
// applies perm only when it creates — so write_file cannot silently strip the
// executable bit off a script it rewrites.
const (
	defaultFileMode = 0o644
	defaultDirMode  = 0o755
)

// WriteFile writes a file whole, creating it and its parent directories if they
// are not there.
//
// **Containment is the point of this tool, and it is harder here than for a
// read.** A write creates its target, so the path does not exist when it is
// checked and there is nothing to call realpath on. [Root.Resolve] handles that
// — it peels the missing tail off, resolves the nearest existing ancestor, and
// rejoins — which is why the classic escape is caught:
//
//	root/safe -> /etc        a symlink already on disk
//	write_file("safe/passwd")
//
// "safe/passwd" is lexically inside the root and resolves outside it, so the
// call is refused with [ErrOutsideRoot] and nothing is created. A second layer
// sits under that: every directory and the file itself are made through the
// os.Root handle, which refuses to traverse out of the root at the syscall
// level, so the window between the check and the write is closed too.
//
// Refusing is all this tool does about an escape. SLICE-1 §M1 has writes
// outside the root *ask*, and that decision belongs to internal/permission via
// the engine — nothing here prompts, and nothing here imports it.
//
// The three decisions the card asks to be made explicitly:
//
//   - **Missing parents are created.** The alternative is a refusal the model
//     answers with `run_shell mkdir -p`, which trades a contained write for a
//     shell command that SLICE-1 §M1 gates on every call — a worse outcome for
//     both safety and turn count. Creation is never silent: the directories are
//     named in the output.
//   - **An existing file is overwritten, truncated to the new content.** There
//     is no append mode and no merge; edit_file (KAN-784) is how part of a file
//     changes.
//   - **An overwrite is declared.** The model may never have read what it just
//     destroyed, and a result that says only "42 bytes written" makes that
//     indistinguishable from creating a new file. So the output states the size
//     of what was replaced and points at edit_file. This is a *visibility*
//     answer rather than a refusal: refusing to overwrite an unread file would
//     need this package to track what has been read, which is session state the
//     engine owns, and G1's turn snapshot already keeps the previous content
//     recoverable.
//
// The write is a truncating write and not a write-then-rename. A rename would
// buy atomicity at the cost of changing the inode on every write — breaking
// hard links and the identity a watcher tracks — and the failure it protects
// against, a half-written file, is already covered by the turn snapshot.
//
// **A cancellation is a result, not an error.** It returns a WriteResult with
// Cancelled set and a nil error, which is [Set.RunShell]'s convention.
// Classifying a user pressing Ctrl-C as [FaultInternal] would bucket every
// interrupted session as a harness failure under ADR-0006 §3, which is the one
// thing that is nobody's failure. (read_file, list_dir and grep do route
// cancellation into internalErr today; KAN-808 makes that consistent, and this
// tool follows run_shell rather than adding a fifth behaviour.)
func (s *Set) WriteFile(ctx context.Context, req WriteRequest) (WriteResult, error) {
	if cancelled(ctx) {
		return cancelledWrite(req.Path), nil
	}

	p, err := s.Root.Resolve(ToolWriteFile, req.Path)
	if err != nil {
		return WriteResult{}, err
	}
	if p.Rel == "." {
		return WriteResult{}, taskErr(ToolWriteFile, req.Path, ErrNotRegular,
			"it is the repository root, which is a directory")
	}

	res := WriteResult{Path: p.Slash()}

	switch info, serr := s.Root.dir.Stat(p.Rel); {
	case serr == nil && info.IsDir():
		return WriteResult{}, taskErr(ToolWriteFile, req.Path, ErrNotRegular,
			"it is a directory; write_file writes files")
	case serr == nil && !info.Mode().IsRegular():
		return WriteResult{}, taskErr(ToolWriteFile, req.Path, ErrNotRegular,
			fmt.Sprintf("its mode is %s", info.Mode()))
	case serr == nil:
		res.Replaced, res.ReplacedBytes = true, info.Size()
	case !errors.Is(serr, fs.ErrNotExist):
		return WriteResult{}, s.writeErr(req.Path, p, serr)
	}

	missing, err := s.missingParents(req.Path, p)
	if err != nil {
		return WriteResult{}, err
	}

	// The last check before anything on disk changes. After the write there is
	// nothing to cancel: reporting a completed write as cancelled would leave
	// the model believing a file it created is not there.
	if cancelled(ctx) {
		return cancelledWrite(req.Path), nil
	}

	if len(missing) > 0 {
		if err := s.Root.dir.MkdirAll(filepath.Dir(p.Rel), defaultDirMode); err != nil {
			return WriteResult{}, s.writeErr(req.Path, p, err)
		}
		res.CreatedDirs = make([]string, len(missing))
		for i, d := range missing {
			res.CreatedDirs[i] = filepath.ToSlash(d)
		}
	}

	content := []byte(req.Content)
	if err := s.Root.dir.WriteFile(p.Rel, content, defaultFileMode); err != nil {
		return WriteResult{}, s.writeErr(req.Path, p, err)
	}

	res.Bytes = len(content)
	res.Lines = len(anchor.Split(content))
	res.Output = res.render()
	return res, nil
}

// cancelled reports whether ctx has ended, without producing an error value.
//
// It is a bool and not a ctx.Err() check because write_file answers a
// cancellation with a result rather than an error (see [Set.WriteFile]), so the
// error has nowhere to go — and "checked an error, returned nil" is a shape the
// nilerr linter is right to distrust everywhere it is not this.
func cancelled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// cancelledWrite is the result of a call the context ended before it wrote.
func cancelledWrite(given string) WriteResult {
	r := WriteResult{Path: given, Cancelled: true}
	r.Output = r.render()
	return r
}

// missingParents returns the ancestors of the target that do not exist yet,
// outermost first, so the output can name what it brought into being.
//
// It walks up rather than calling MkdirAll and shrugging because the
// directories created are a fact the model should be told about, and MkdirAll
// reports only that it succeeded.
//
// The arm for an ancestor that exists as a *file* is a race guard and is
// honestly rarely reached: [Root.Resolve] already refuses "sub/b.txt/nested"
// with the OS's own ENOTDIR, because EvalSymlinks cannot walk through a file
// either. It stays because the alternative when the tree does move under us is
// a bare ENOTDIR from MkdirAll landing in [FaultInternal], which would book a
// filesystem race as a harness defect.
//
// Every stat goes through the root handle, so a walk cannot climb out of the
// root even if Resolve had let something through.
func (s *Set) missingParents(given string, p Path) ([]string, error) {
	var missing []string
	for dir := filepath.Dir(p.Rel); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		info, err := s.Root.dir.Stat(dir)
		switch {
		case err == nil && info.IsDir():
			slices.Reverse(missing)
			return missing, nil
		case err == nil:
			return nil, taskErr(ToolWriteFile, given, ErrNotRegular, fmt.Sprintf(
				"%q is a file, so it cannot hold %q", filepath.ToSlash(dir), p.Slash()))
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, dir)
		default:
			return nil, s.writeErr(given, p, err)
		}
	}
	slices.Reverse(missing)
	return missing, nil
}

// writeErr classifies a failure from the root handle on the write path.
//
// It is separate from [Set.handleErr] because the messages differ where it
// matters: "not readable" is the wrong sentence for a write, and a missing
// parent is a normal thing for a write to hit rather than the flat "no such
// file" a read gets. The default arm agrees with handleErr, and deliberately:
// Resolve already proved this path inside the root, so an unexplained refusal
// is the harness's problem, and calling it a task failure would hide a harness
// limitation inside a model score.
func (s *Set) writeErr(given string, p Path, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return taskErr(ToolWriteFile, given, err,
			"a directory on the way to it disappeared while the file was being written")
	case errors.Is(err, fs.ErrPermission):
		return taskErr(ToolWriteFile, given, err, "the path is not writable")
	default:
		return internalErr(ToolWriteFile, given, err,
			"the root handle refused a path that resolved inside the root")
	}
}

// render is the model-facing form:
//
//	write_file internal/tools/write.go: replaced, 4 lines, 91 bytes
//	note: 120 bytes of previous content were overwritten in full; write_file
//	replaces a whole file, use edit_file to change part of one
func (r WriteResult) render() string {
	if r.Cancelled {
		return fmt.Sprintf("write_file %s: cancelled before writing; the file was not changed\n", r.Path)
	}

	var b strings.Builder
	verb := "created"
	if r.Replaced {
		verb = "replaced"
	}
	if r.Bytes == 0 {
		fmt.Fprintf(&b, "write_file %s: %s, empty\n", r.Path, verb)
	} else {
		fmt.Fprintf(&b, "write_file %s: %s, %s, %d bytes\n",
			r.Path, verb, plural(r.Lines, "line", "lines"), r.Bytes)
	}
	if n := len(r.CreatedDirs); n > 0 {
		word := "directory"
		if n > 1 {
			word = "directories"
		}
		fmt.Fprintf(&b, "note: created %s %s\n", word, strings.Join(r.CreatedDirs, ", "))
	}
	if r.Replaced {
		fmt.Fprintf(&b, "note: %d bytes of previous content were overwritten in full; "+
			"write_file replaces a whole file, use edit_file to change part of one\n", r.ReplacedBytes)
	}
	return b.String()
}
