package tools

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/leejianrong/kopicode/internal/anchor"
)

// ReplyAnchors is what one assistant reply's own applied edits re-anchored, per
// file (ADR-0038). The engine makes one per reply and hands it to every
// edit_file call in that reply; it is in memory only and is dropped when the
// reply's calls are done.
//
// It exists for one case. An anchor hashes a line with both neighbours, so an
// applied edit changes the anchor of the unedited line above and below it. A
// model that writes all its edits for a file in one reply wrote the later calls
// before the first result existed, so a call that names a neighbour holds an
// anchor this very reply made stale. [Set.EditFile] translates such an anchor to
// the line's current one, and only such an anchor: one that is not an `Old` of
// this reply's own map, on this file, is refused as drift exactly as before.
//
// Entries follow a line by position, not by hash, through every later edit, so
// the current anchor is derived from the file as it is. A nil *ReplyAnchors
// turns the rule off.
type ReplyAnchors struct {
	files map[string]*replyFile
}

// NewReplyAnchors returns an empty map for one reply.
func NewReplyAnchors() *ReplyAnchors { return &ReplyAnchors{files: map[string]*replyFile{}} }

// replyFile is one file's remembered lines and the digest of the bytes they
// describe. A file that no longer has those bytes was changed by something
// other than an applied edit_file in this reply, and its entries are void.
type replyFile struct {
	sum     [sha256.Size]byte
	entries []replyEntry
}

// replyEntry is one remembered line: the anchor the model may hold for it and
// where it is now (0-based).
type replyEntry struct {
	old  string
	line int
}

// Translation records one anchor [Set.EditFile] translated.
type Translation struct {
	// Which is "start" or "end".
	Which string
	// From is the anchor the model sent, To the one the line has now.
	From string
	To   string
	// Line is the 1-based line both name.
	Line int
}

// Reset empties the map: a write_file, delete_file, shell or fuzzy edit may
// have changed any file.
func (r *ReplyAnchors) Reset() {
	if r != nil {
		clear(r.files)
	}
}

// translate returns the anchor path's remembered line for a, as the line's
// current anchor. ok is false unless a is the `Old` of exactly one entry, the
// file still has the bytes the entries describe, and the current anchor names
// exactly that one line.
func (r *ReplyAnchors) translate(path string, src []byte, lines, anchors []string, a string) (to string, line int, ok bool) {
	if r == nil {
		return "", 0, false
	}
	f := r.files[path]
	if f == nil {
		return "", 0, false
	}
	if f.sum != sha256.Sum256(src) {
		delete(r.files, path)
		return "", 0, false
	}
	found := -1
	for _, e := range f.entries {
		if e.old != a {
			continue
		}
		if found >= 0 {
			return "", 0, false // two lines had this anchor: no single reading
		}
		found = e.line
	}
	if found < 0 || found >= len(lines) {
		return "", 0, false
	}
	now := anchors[found]
	if i, err := anchor.Resolve(anchors, now); err != nil || i != found {
		return "", 0, false
	}
	return now, found + 1, true
}

// record folds one applied edit into path's entries. old and oldAnchors are the
// file before, the replaced region the 0-based inclusive [start, end] that
// became n lines, and next the bytes written. re is what the edit re-anchored.
func (r *ReplyAnchors) record(path string, oldAnchors []string, start, end, n int, next []byte, re []Reanchor) {
	if r == nil {
		return
	}
	f := r.files[path]
	if f == nil {
		f = &replyFile{}
		r.files[path] = f
	}
	delta := n - (end - start + 1)
	kept := f.entries[:0]
	for _, e := range f.entries {
		switch {
		case e.line >= start && e.line <= end:
			continue // replaced or deleted: there is no same line to name
		case e.line > end:
			e.line += delta
		}
		kept = append(kept, e)
	}
	f.entries = kept

	unique := map[string]int{}
	for _, a := range oldAnchors {
		unique[a]++
	}
	for _, l := range re {
		line := l.Now.Line - 1
		// A line already remembered keeps its first Old: the model holds
		// that one, and the current anchor is derived from the file.
		known := false
		for _, e := range f.entries {
			if e.line == line {
				known = true
				break
			}
		}
		// Only a line whose anchor named it alone before the edit: an
		// anchor two lines shared never had a single reading to translate.
		if known || unique[l.Old] != 1 {
			continue
		}
		f.entries = append(f.entries, replyEntry{old: l.Old, line: line})
	}
	f.sum = sha256.Sum256(next)
}

// translateAnchors returns req with each anchor that matches no line but is a
// remembered one replaced by the line's current anchor, and what it did. The
// request is returned unchanged when there is nothing to translate.
func (r *ReplyAnchors) translateAnchors(path string, src []byte, lines, anchors []string, req EditRequest) (EditRequest, []Translation) {
	if r == nil {
		return req, nil
	}
	var out []Translation
	for _, p := range [...]struct {
		which string
		a     *string
	}{{"start", &req.AnchorStart}, {"end", &req.AnchorEnd}} {
		if !WellFormedAnchor(*p.a) {
			continue
		}
		if _, err := anchor.Resolve(anchors, *p.a); !errors.Is(err, anchor.ErrUnknownAnchor) {
			continue // a live match is never translated; ambiguity is refused as now
		}
		to, line, ok := r.translate(path, src, lines, anchors, *p.a)
		if !ok {
			continue
		}
		out = append(out, Translation{Which: p.which, From: *p.a, To: to, Line: line})
		*p.a = to
	}
	return req, out
}

// note is the sentence appended to an applied edit's result for each
// translation, so the model's next call in the reply can use current anchors.
func (t Translation) note() string {
	return fmt.Sprintf("anchor_%s %s was this reply's earlier anchor for line %d, now %s; used that\n",
		t.Which, t.From, t.Line, t.To)
}
