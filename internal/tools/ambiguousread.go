package tools

import "sync"

// ambiguityLedger remembers, per file, the anchors that named more than one line
// the last time the model read that file (KOP-225).
//
// It closes one fail-open path. An anchor that matched two lines when the model
// read the file could not be used then: it is refused as ambiguous. But an
// applied edit re-anchors the lines beside it, which can leave the old anchor
// matching only the other line, and a call that sends it would then apply there
// with no signal. The model never had a single reading of that anchor, so the
// call is refused as ambiguous, as it would have been before the edit.
//
// A read replaces the file's entry, so a fresh read, which shows the file as it
// is, clears it; so does any write to or deletion of the file. Held in memory by
// the [Set], so a session resumed in a new process starts empty and that case is
// not covered.
type ambiguityLedger struct {
	mu     sync.Mutex
	byPath map[string]map[string]bool
}

// noteRead replaces path's entry with the anchors that appear on more than one
// of anchors' lines.
func (l *ambiguityLedger) noteRead(path string, anchors []string) {
	count := make(map[string]int, len(anchors))
	for _, a := range anchors {
		count[a]++
	}
	var dup map[string]bool
	for a, n := range count {
		if n > 1 {
			if dup == nil {
				dup = map[string]bool{}
			}
			dup[a] = true
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if dup == nil {
		delete(l.byPath, path)
		return
	}
	if l.byPath == nil {
		l.byPath = map[string]map[string]bool{}
	}
	l.byPath[path] = dup
}

// forget drops path's entry.
func (l *ambiguityLedger) forget(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byPath, path)
}

// wasAmbiguous reports whether a named more than one line at path's last read.
func (l *ambiguityLedger) wasAmbiguous(path, a string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byPath[path][a]
}

// release removes anchors the tool itself just reported as a line's current
// anchor, so one that happens to equal an old ambiguous anchor is not refused.
func (l *ambiguityLedger) release(path string, anchors ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, a := range anchors {
		delete(l.byPath[path], a)
	}
}
