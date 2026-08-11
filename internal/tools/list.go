package tools

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

// ListRequest is one list_dir call.
type ListRequest struct {
	// Path is the directory to list, relative to the repository root or
	// absolute. Empty means the root.
	Path string
	// Recursive walks the whole subtree rather than one directory.
	Recursive bool
	// Pattern filters entries by name; see matchName. Directories are still
	// descended into when recursing, so "*.go" finds Go files at any depth.
	Pattern string
}

// skipDirs are directory names a recursive walk never descends into.
//
// .git is thousands of files a model has no use for and will happily spend a
// turn reading. .kopicode is the harness's own journal and blobs — a session
// that lists its own transcript back into its context is a feedback loop, not a
// finding. Both are named in the output, so their absence is a stated exclusion
// rather than a silent one.
var skipDirs = []string{".git", ".kopicode"}

// ListDir lists a directory.
//
// Output is one entry per line under a header: a size in bytes for a regular
// file, "-" for anything else, then the name — with "/" appended to a directory
// and "@" to a symlink, so the model can tell them apart without a second call.
// Recursive listings print paths relative to the listed directory, in the
// lexical order io/fs walks in.
//
//	internal/tools: 3 entries
//	  4001  grep.go
//	  2914  list.go
//	     -  sub/
//
// list_dir carries the name pattern that makes SLICE-1's decision to drop a
// separate glob tool hold up: `list_dir(path=".", recursive=true,
// pattern="*_test.go")` is the query a glob tool would have served, and grep's
// include filter takes the same pattern with the same meaning.
//
// Symlinks are listed but never followed, so a recursive walk cannot leave the
// tree and cannot loop.
func (s *Set) ListDir(ctx context.Context, req ListRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", cancelledErr(ToolListDir, req.Path, err, "nothing was listed")
	}

	p, err := s.Root.Resolve(ToolListDir, req.Path)
	if err != nil {
		return "", err
	}
	if err := validPattern(req.Pattern); err != nil {
		return "", taskErr(ToolListDir, req.Path, err,
			fmt.Sprintf("pattern %q is malformed", req.Pattern))
	}

	info, err := s.stat(ToolListDir, p)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", taskErr(ToolListDir, req.Path, ErrNotRegular,
			"it is a file, not a directory; use read_file")
	}

	entries, total, err := s.collect(ctx, p, req)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", p.Slash(), plural(total, "entry", "entries"))
	if req.Recursive {
		b.WriteString(", recursive, excluding " + strings.Join(skipDirs, " and "))
	}
	if req.Pattern != "" {
		fmt.Fprintf(&b, ", matching %q", req.Pattern)
	}
	b.WriteByte('\n')
	if total > len(entries) {
		fmt.Fprintf(&b, "showing the first %d; bounded at max_entries=%d, narrow the pattern for the rest\n",
			len(entries), s.Limits.MaxEntries)
	}

	width := 0
	for _, e := range entries {
		width = max(width, len(e.size))
	}
	for _, e := range entries {
		fmt.Fprintf(&b, "  %*s  %s\n", width, e.size, e.name)
	}
	return b.String(), nil
}

// entry is one rendered listing line, with the size pre-formatted so the column
// can be aligned before anything is written.
type entry struct {
	name string
	size string
}

// collect gathers the entries to show and reports how many matched in total, so
// a bounded listing can say what it left out rather than merely stopping.
func (s *Set) collect(ctx context.Context, p Path, req ListRequest) ([]entry, int, error) {
	fsys := s.Root.dir.FS()
	base := p.Slash()

	var out []entry
	total := 0

	add := func(name string, d fs.DirEntry) {
		total++
		if len(out) >= s.Limits.MaxEntries {
			return
		}
		size := "-"
		switch {
		case d.IsDir():
			name += "/"
		case d.Type()&fs.ModeSymlink != 0:
			name += "@"
		case d.Type().IsRegular():
			if info, err := d.Info(); err == nil {
				size = strconv.FormatInt(info.Size(), 10)
			}
		}
		out = append(out, entry{name: name, size: size})
	}

	if !req.Recursive {
		dirents, err := fs.ReadDir(fsys, base)
		if err != nil {
			return nil, 0, s.handleErr(ToolListDir, p, err)
		}
		for _, d := range dirents {
			if ok, _ := matchName(req.Pattern, d.Name()); ok {
				add(d.Name(), d)
			}
		}
		return out, total, nil
	}

	err := fs.WalkDir(fsys, base, func(walked string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if walked == base {
			return nil
		}
		if d.IsDir() && slices.Contains(skipDirs, d.Name()) {
			return fs.SkipDir
		}
		rel := relTo(base, walked)
		if ok, _ := matchName(req.Pattern, rel); ok {
			add(rel, d)
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, cancelledErr(ToolListDir, req.Path, err, "the listing was abandoned part-way")
		}
		return nil, 0, s.handleErr(ToolListDir, p, err)
	}
	return out, total, nil
}

// relTo strips the listed directory's prefix, so a recursive listing reads as
// paths under what was asked for rather than under the repository root.
func relTo(base, walked string) string {
	if base == "." {
		return walked
	}
	return strings.TrimPrefix(walked, base+"/")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + one
	}
	return strconv.Itoa(n) + " " + many
}
