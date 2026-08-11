package tools

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"

	"github.com/leejianrong/kopicode/internal/anchor"
)

// GrepRequest is one grep call.
type GrepRequest struct {
	// Pattern is an RE2 regular expression, as regexp compiles it.
	Pattern string
	// Path is the file or directory to search, relative to the repository root
	// or absolute. Empty means the root.
	Path string
	// Include filters which files are searched, by the same name pattern
	// list_dir takes; see matchName. Empty searches everything.
	Include string
	// IgnoreCase folds case, as a leading (?i) would.
	IgnoreCase bool
}

// Grep searches file contents and reports matching lines as `path:line: text`.
//
//	grep "func .*Error": 2 matches in 1 file under internal/tools
//	internal/tools/errors.go:96: func (e *Error) Error() string
//	internal/tools/errors.go:120: func (e *Error) Unwrap() error
//
// **Grep output carries no anchors, deliberately.** ADR-0006 rests on anchors
// being obtainable only from a read, which is what makes an edit into a region
// the model was never shown structurally impossible rather than merely
// discouraged. Anchoring grep hits would hand the model an edit target it had
// seen one line of, and quietly convert a structural guarantee into a
// convention. Line numbers agree with read_file's — both split lines through
// [anchor.Split], so a CRLF checkout numbers the same as an LF one — so a grep
// hit is followed by a read_file at that offset.
//
// Skips are counted and stated, never silent: a binary file cannot be searched
// as text, a file over Limits.MaxFileBytes is not read, and .git and .kopicode
// are not walked. Matching stops at Limits.MaxMatches and says so.
//
// Searching is stdlib regexp over files read through the root handle. Nothing
// is shelled out here — the "shell out, do not link" rule is about language
// tooling and CGo, and reaching for an external ripgrep would make a binary
// this project does not ship a requirement for a core tool.
func (s *Set) Grep(ctx context.Context, req GrepRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", cancelledErr(ToolGrep, req.Path, err, "nothing was searched")
	}
	if req.Pattern == "" {
		return "", taskErr(ToolGrep, req.Path, nil, "a pattern is required")
	}

	expr := req.Pattern
	if req.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", taskErr(ToolGrep, req.Path, err,
			fmt.Sprintf("pattern %q is not a valid regular expression", req.Pattern))
	}
	if err := validPattern(req.Include); err != nil {
		return "", taskErr(ToolGrep, req.Path, err,
			fmt.Sprintf("include %q is malformed", req.Include))
	}

	p, err := s.Root.Resolve(ToolGrep, req.Path)
	if err != nil {
		return "", err
	}
	info, err := s.stat(ToolGrep, p)
	if err != nil {
		return "", err
	}

	sc := &scan{set: s, re: re, include: req.Include, scope: "in " + p.Slash()}
	if info.IsDir() {
		sc.scope = "under " + p.Slash()
		if p.Rel == "." {
			sc.scope = "under the repository root"
		}
		err = sc.walk(ctx, p)
	} else {
		err = sc.file(ctx, p.Slash(), info.Size())
	}
	if err != nil {
		return "", err
	}

	return sc.render(req), nil
}

// scan is one grep's accumulating state: what matched, what was skipped, and
// why. Skips are counted rather than dropped because "no matches" and "no
// matches in the files I was willing to read" are different answers.
type scan struct {
	set     *Set
	re      *regexp.Regexp
	include string
	scope   string

	hits              []string
	files             int
	skippedBin        int
	skippedLarge      int
	skippedUnreadable int
	bounded           bool
}

func (sc *scan) walk(ctx context.Context, dir Path) error {
	fsys := sc.set.Root.dir.FS()
	base := dir.Slash()

	err := fs.WalkDir(fsys, base, func(walked string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			if walked != base && slices.Contains(skipDirs, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		// Symlinks are not followed: a link out of the tree is exactly what
		// the root guard exists to stop, and a link within it would report
		// the same content twice under two names.
		if !d.Type().IsRegular() {
			return nil
		}
		if ok, _ := matchName(sc.include, relTo(base, walked)); !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := sc.file(ctx, walked, info.Size()); err != nil {
			return err
		}
		if sc.bounded {
			// Stop walking rather than visiting the rest of the tree to do
			// nothing with it. The notice says the search stopped early, which
			// is also why the skip counts below are counts of what was reached
			// and not of the whole tree.
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return cancelledErr(ToolGrep, dir.Given, err, "the search was abandoned part-way")
		}
		return sc.set.handleErr(ToolGrep, dir, err)
	}
	return nil
}

// read reads one file for searching, reporting false rather than an error for
// a file that vanished or turned unreadable between the walk and the read.
//
// One unreadable file is not worth failing a whole search over — but it is not
// nothing either, so it is counted and reported. A file silently omitted from a
// search is worse than one reported as unsearchable, because the model
// concludes the string is not there.
func (sc *scan) read(rel string) ([]byte, bool) {
	src, err := sc.set.Root.dir.ReadFile(rel)
	if err != nil {
		sc.skippedUnreadable++
		return nil, false
	}
	return src, true
}

// file searches one file, identified by its root-relative slash path.
func (sc *scan) file(ctx context.Context, rel string, size int64) error {
	if err := ctx.Err(); err != nil {
		return cancelledErr(ToolGrep, rel, err, "the search was abandoned part-way")
	}
	if sc.bounded {
		return nil
	}
	if size > sc.set.Limits.MaxFileBytes {
		sc.skippedLarge++
		return nil
	}

	src, ok := sc.read(rel)
	if !ok {
		return nil
	}
	if looksBinary(src) {
		sc.skippedBin++
		return nil
	}

	counted := false
	for i, line := range anchor.Split(src) {
		if !sc.re.MatchString(line) {
			continue
		}
		// Stop counting at the bound rather than counting on and reporting a
		// total the output does not contain. The counts in the header always
		// describe what is printed; the notice says more remain.
		if len(sc.hits) >= sc.set.Limits.MaxMatches {
			sc.bounded = true
			return nil
		}
		if !counted {
			sc.files++
			counted = true
		}
		sc.hits = append(sc.hits, fmt.Sprintf("%s:%d: %s", rel, i+1, line))
	}
	return nil
}

func (sc *scan) render(req GrepRequest) string {
	var b strings.Builder

	fmt.Fprintf(&b, "grep %q: ", req.Pattern)
	if len(sc.hits) == 0 {
		fmt.Fprintf(&b, "no matches %s", sc.scope)
	} else {
		fmt.Fprintf(&b, "%s in %s %s",
			plural(len(sc.hits), "match", "matches"),
			plural(sc.files, "file", "files"), sc.scope)
	}
	if req.Include != "" {
		fmt.Fprintf(&b, ", including %q", req.Include)
	}
	b.WriteByte('\n')

	if sc.bounded {
		fmt.Fprintf(&b, "bounded at max_matches=%d; the search stopped early and more matches remain — refine the pattern\n",
			sc.set.Limits.MaxMatches)
	}
	var why []string
	if sc.skippedBin > 0 {
		why = append(why, plural(sc.skippedBin, "binary file", "binary files"))
	}
	if sc.skippedLarge > 0 {
		why = append(why, fmt.Sprintf("%s over max_file_bytes=%d",
			plural(sc.skippedLarge, "file", "files"), sc.set.Limits.MaxFileBytes))
	}
	if sc.skippedUnreadable > 0 {
		why = append(why, plural(sc.skippedUnreadable, "unreadable file", "unreadable files"))
	}
	if len(why) > 0 {
		fmt.Fprintf(&b, "skipped %s\n", strings.Join(why, " and "))
	}
	for _, h := range sc.hits {
		b.WriteString(h)
		b.WriteByte('\n')
	}
	return b.String()
}
