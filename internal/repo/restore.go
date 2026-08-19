package repo

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Sentinel causes for [Repo.Restore], for errors.Is.
var (
	// ErrRestoreDestInsideGitDir reports a restore destination that resolves
	// inside this repository's own git directory (its GitDir or CommonDir).
	// Every other destination — including this Repo's own working tree root
	// — is the caller's decision to make; this is the one Restore refuses on
	// its own, because writing a tree's files on top of git's bookkeeping is
	// not what either legitimate use of Restore means, and unlike
	// overwriting the user's ordinary files it is not recoverable by
	// re-running Restore afterward.
	ErrRestoreDestInsideGitDir = errors.New("restore destination is inside the repository's git directory")
	// ErrRestoreUnsafeEntry reports an archive entry whose path, or whose
	// symlink target, would write outside the restore destination, or that
	// names a path component of ".git".
	ErrRestoreUnsafeEntry = errors.New("archive entry is unsafe to extract")
	// ErrRestoreUnsupportedEntry reports a tar entry type Restore does not
	// know how to extract: anything other than a regular file, a directory
	// or a symlink. git never produces one of these from a real tree object,
	// so reaching this is either a corrupt archive or a tree built by hand
	// through low-level plumbing.
	ErrRestoreUnsupportedEntry = errors.New("archive entry type is not supported")
)

// Restore materializes the tree identified by tree into dest.
//
// tree is any tree-ish this repository can resolve to a tree object —
// ordinarily a [Snapshot]'s Tree field, or the Tree recorded in a
// journal.TurnSnapshot event, so a caller restoring from the journal alone
// never needs to have kept a Snapshot value around.
//
// # Mechanism, and why it holds the package's promise
//
// Restore runs `git archive --format=tar <tree>` through [Repo.git] — the
// same guarded, read-only path Head and ExcludeStateDir use — and decodes the
// tar stream itself. `archive` reads a tree object and writes out the blobs
// it names; it is not in [indexWriting] (git.go), because it has no index to
// write, no HEAD to move and no ref to touch. It is the read counterpart to
// [Snapshotter.Snapshot] in the same sense Head is a read counterpart to
// commit-tree: both go through Repo.git, which structurally refuses any
// subcommand that could touch the user's index, so there is no second,
// unguarded git invocation for a later change to slip a checkout or a reset
// into. The tar bytes are decoded by this process rather than piped into a
// second `tar` subprocess or a second `git` invocation with a destination
// flag, which is what keeps the path-safety checks below in one place
// instead of trusting two external programs to agree on them.
//
// # What dest is, and is not
//
// dest is not inspected to guess which of Restore's two legitimate uses the
// caller means. It may be this repository's own [Repo.Root] — a caller
// choosing to overwrite the live working tree with a prior turn's state — or
// an unrelated scratch directory for read-only inspection. Restore does the
// caller's chosen thing wherever dest points, except for one refusal: a dest
// that resolves inside this repository's GitDir or CommonDir is rejected
// with [ErrRestoreDestInsideGitDir], because neither legitimate use above
// ever means "overwrite git's own bookkeeping" and there is no recovery from
// getting that one wrong. dest is created (mode 0o755) if it does not
// already exist; if it exists it must be a directory.
//
// Restore does not clear dest first, and this is deliberate rather than an
// oversight: a path the tree contains is overwritten in place, and a path
// already in dest that the tree does not mention is left exactly as it was.
// Turning this into an exact checkout — dest ends up holding nothing the
// tree does not — is the caller's decision (an `os.RemoveAll(dest)` before
// calling, or the equivalent for a live working tree), not something this
// primitive does on the caller's behalf. Deciding to delete an arbitrary
// caller-supplied directory's existing contents is a materially bigger
// promise than "read a tree back out safely", and folding it in here would
// mean a caller who wanted the read-only inspection copy could lose data by
// pointing Restore at a scratch directory that was not as empty as they
// thought.
//
// # Path safety
//
// Every entry is written through an [os.Root] opened on dest — the same
// mechanism internal/tools' Root.Resolve rests on for read_file, write_file
// and edit_file — so a write is refused at the syscall level if any
// component of its path, once resolved, would leave dest, and that holds
// even for a component that is a symlink already sitting in dest before this
// call ever ran (planted by an earlier restore, or by anything else with
// filesystem access). Restore adds two checks Root does not make on its own,
// because both name legitimate directory or file names Root has no reason to
// refuse: an entry naming a ".git" path component is rejected outright
// (case-insensitively, since the filesystems this can land on fold case) —
// ordinary `git add` already refuses to track a path literally called
// ".git", which is why every tree [Snapshotter.Snapshot] produces is safe on
// this count already, but Restore accepts any tree this repository can
// resolve, including one built by hand through commit-tree or mktree that
// bypasses that guard — and a symlink entry's own target is checked to
// resolve inside dest before the link is created, because
// `os.Root.Symlink` "does not validate oldname" (its own documentation): Root
// stops a later entry from being written *through* an escaping symlink, but
// nothing stops the escaping symlink itself from being created unless this
// package checks its target first.
func (r *Repo) Restore(ctx context.Context, tree, dest string) error {
	if strings.TrimSpace(tree) == "" {
		return fmt.Errorf("repo: Restore needs a tree-ish to read, got %q", tree)
	}

	abs, err := r.prepareRestoreDest(dest)
	if err != nil {
		return err
	}

	out, err := r.git(ctx, "archive", "--format=tar", tree)
	if err != nil {
		return fmt.Errorf("repo: archiving tree %s: %w", tree, err)
	}

	root, err := os.OpenRoot(abs)
	if err != nil {
		return fmt.Errorf("repo: opening restore destination %s: %w", abs, err)
	}
	defer func() { _ = root.Close() }()

	if err := extractTar(strings.NewReader(out), root, abs); err != nil {
		return fmt.Errorf("repo: extracting tree %s into %s: %w", tree, abs, err)
	}
	return nil
}

// prepareRestoreDest resolves dest to an absolute path, refuses one that
// falls inside this repository's git directory, and ensures it exists as a
// directory.
//
// The git-directory check is a plain path comparison rather than a
// symlink-aware one: GitDir and CommonDir come from git's own rev-parse, and
// a dest that is only a symlinked alias of one of them is not something this
// check claims to catch. It is here to refuse the straightforward mistake —
// pointing Restore at ".git" or a linked worktree's private git directory by
// accident — not to defend against a deliberately constructed alias.
func (r *Repo) prepareRestoreDest(dest string) (string, error) {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", fmt.Errorf("repo: resolving restore destination %s: %w", dest, err)
	}
	abs = filepath.Clean(abs)

	for _, guarded := range []string{r.gitDir, r.commonDir} {
		if guarded == "" {
			continue
		}
		if abs == guarded || strings.HasPrefix(abs, guarded+string(filepath.Separator)) {
			return "", fmt.Errorf("repo: restore destination %s: %w", dest, ErrRestoreDestInsideGitDir)
		}
	}

	switch info, err := os.Stat(abs); {
	case err == nil:
		if !info.IsDir() {
			return "", fmt.Errorf("repo: restore destination %s exists and is not a directory", abs)
		}
	case os.IsNotExist(err):
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", fmt.Errorf("repo: creating restore destination %s: %w", abs, err)
		}
	default:
		return "", fmt.Errorf("repo: checking restore destination %s: %w", abs, err)
	}
	return abs, nil
}

// extractTar decodes a tar stream produced by `git archive --format=tar`
// through root, which must be open on dest. See [Repo.Restore] for the
// safety rules each entry is held to; dest is needed only to check a
// symlink's target, everything else is enforced by root itself.
func extractTar(r io.Reader, root *os.Root, dest string) error {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("reading tar stream: %w", err)
		}

		// pax extended headers carry no file of their own; Go's tar.Reader
		// already folds the per-entry form (TypeXHeader) into the following
		// header automatically, so only the global form can reach here, and
		// only defensively.
		if header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeXHeader {
			continue
		}

		relName, err := restoreEntryName(header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(relName, 0o755); err != nil {
				return fmt.Errorf("creating directory %s: %w", relName, err)
			}
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA is what older archives still use.
			if err := extractRegular(tr, header, root, relName); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := extractSymlink(header, root, dest, relName); err != nil {
				return err
			}
		default:
			return fmt.Errorf("repo: %w: %q has tar type %q",
				ErrRestoreUnsupportedEntry, header.Name, string(header.Typeflag))
		}
	}
}

// restoreEntryName turns a tar entry's name (forward-slashed, relative) into
// a path safe to hand to an [os.Root] method: no absolute path, no ".."
// component, no ".git" component. Root enforces containment on its own, but
// has no reason to refuse an ordinary-looking ".git" directory name, which is
// exactly the case this exists to catch — see [Repo.Restore].
func restoreEntryName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("repo: %w: an archive entry has an empty name", ErrRestoreUnsafeEntry)
	}

	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("repo: %w: %q is an absolute path", ErrRestoreUnsafeEntry, name)
	}
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		switch {
		case part == "..":
			return "", fmt.Errorf("repo: %w: %q escapes the destination", ErrRestoreUnsafeEntry, name)
		case strings.EqualFold(part, ".git"):
			return "", fmt.Errorf("repo: %w: %q contains a .git path component", ErrRestoreUnsafeEntry, name)
		}
	}
	return clean, nil
}

// extractRegular writes one regular file's content, read from tr, into root
// at relName.
func extractRegular(tr *tar.Reader, header *tar.Header, root *os.Root, relName string) error {
	if dir := filepath.Dir(relName); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	mode := header.FileInfo().Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	f, err := root.OpenFile(relName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("creating %s: %w", relName, err)
	}
	if _, err := io.Copy(f, tr); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", relName, err)
	}
	return f.Close()
}

// extractSymlink creates the symlink at relName inside root, refusing one
// whose link text would resolve outside dest (root.Symlink does not check
// this on its own — see [Repo.Restore]). The link text itself, not its
// resolved form, is what gets written, which is what makes a relative
// symlink inside the tree portable to wherever dest is; only the safety
// check resolves it.
func extractSymlink(header *tar.Header, root *os.Root, dest, relName string) error {
	linkText := header.Linkname
	if linkText == "" {
		return fmt.Errorf("repo: %w: %q is a symlink with no target", ErrRestoreUnsafeEntry, header.Name)
	}

	linkPath := filepath.FromSlash(linkText)
	if filepath.IsAbs(linkPath) {
		return fmt.Errorf("repo: %w: symlink %q targets the absolute path %q",
			ErrRestoreUnsafeEntry, header.Name, linkText)
	}

	resolved := filepath.Clean(filepath.Join(dest, filepath.Dir(relName), linkPath))
	if resolved != dest && !strings.HasPrefix(resolved, dest+string(filepath.Separator)) {
		return fmt.Errorf("repo: %w: symlink %q targets %q, which resolves outside the destination",
			ErrRestoreUnsafeEntry, header.Name, linkText)
	}

	if dir := filepath.Dir(relName); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	if err := root.Remove(relName); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replacing %s: %w", relName, err)
	}
	if err := root.Symlink(linkPath, relName); err != nil {
		return fmt.Errorf("creating symlink %s -> %s: %w", relName, linkText, err)
	}
	return nil
}
