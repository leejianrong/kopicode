package lineedit

// line is the text being edited and where the cursor sits in it.
//
// **Runes, not bytes.** The cursor is an index into a []rune, so every motion
// and every deletion lands on a character boundary by construction rather than
// by remembering to check. A byte cursor would let Left land inside the two
// bytes of "é", and the line handed to the engine would contain a broken
// sequence that no test of ASCII input would ever catch.
//
// What this deliberately does not model is *display width*. A CJK ideograph
// occupies two terminal columns and a combining mark occupies none, and the
// redraw below counts runes. Those inputs edit correctly — the returned string
// is right — but the drawn cursor sits in the wrong column. Width tables are
// out of scope for this card and are stated here rather than discovered later.
type line struct {
	runes []rune
	// pos is the cursor, in runes, from 0 to len(runes) inclusive.
	pos int
}

func (l *line) String() string { return string(l.runes) }

func (l *line) empty() bool { return len(l.runes) == 0 }

// set replaces the whole line and parks the cursor at the end, which is where
// recalling a history entry should leave it.
func (l *line) set(s string) {
	l.runes = []rune(s)
	l.pos = len(l.runes)
}

func (l *line) insert(r rune) {
	l.runes = append(l.runes, 0)
	copy(l.runes[l.pos+1:], l.runes[l.pos:])
	l.runes[l.pos] = r
	l.pos++
}

// backspace deletes the character before the cursor.
func (l *line) backspace() {
	if l.pos == 0 {
		return
	}
	l.runes = append(l.runes[:l.pos-1], l.runes[l.pos:]...)
	l.pos--
}

// deleteForward deletes the character under the cursor.
func (l *line) deleteForward() {
	if l.pos >= len(l.runes) {
		return
	}
	l.runes = append(l.runes[:l.pos], l.runes[l.pos+1:]...)
}

func (l *line) left() {
	if l.pos > 0 {
		l.pos--
	}
}

func (l *line) right() {
	if l.pos < len(l.runes) {
		l.pos++
	}
}

func (l *line) home() { l.pos = 0 }

func (l *line) end() { l.pos = len(l.runes) }

// killToEnd discards everything from the cursor to the end of the line.
//
// There is no kill ring and no yank. Ctrl-K and Ctrl-U delete; the text is
// gone. A kill ring needs Ctrl-Y to be worth anything, and the card asks for
// four bindings rather than an emacs.
func (l *line) killToEnd() {
	l.runes = l.runes[:l.pos]
}

// killToStart discards everything before the cursor.
func (l *line) killToStart() {
	l.runes = append([]rune{}, l.runes[l.pos:]...)
	l.pos = 0
}

// history is the recall list plus this session's position in it.
//
// pos indexes entries, and pos == len(entries) means "the line the user is
// typing", which is not an entry. The line in progress is stashed the first
// time navigation leaves it, so walking up and back down returns what was
// there instead of an empty prompt.
type history struct {
	entries []string
	pos     int
	stashed string
}

// session is the mutable state of one ReadLine call: the line, the cursor and
// the history cursor. It holds no io, which is what makes the editing logic
// testable by feeding it keys and reading the resulting string.
type session struct {
	line line
	hist history
}

func newSession(entries []string) *session {
	return &session{hist: history{entries: entries, pos: len(entries)}}
}

// historyPrev recalls the entry before the current position.
func (s *session) historyPrev() {
	if s.hist.pos == 0 {
		return
	}
	if s.hist.pos == len(s.hist.entries) {
		s.hist.stashed = s.line.String()
	}
	s.hist.pos--
	s.line.set(s.hist.entries[s.hist.pos])
}

// historyNext walks back towards the line being typed, and restores it on
// arrival.
func (s *session) historyNext() {
	if s.hist.pos >= len(s.hist.entries) {
		return
	}
	s.hist.pos++
	if s.hist.pos == len(s.hist.entries) {
		s.line.set(s.hist.stashed)
		return
	}
	s.line.set(s.hist.entries[s.hist.pos])
}

// outcome is what one key did to the read in progress.
type outcome int

const (
	// outcomeContinue: the line changed (or deliberately did not), keep reading.
	outcomeContinue outcome = iota
	// outcomeAccept: the user pressed Enter; the line is the result.
	outcomeAccept
	// outcomeInterrupt: Ctrl-C at the prompt.
	outcomeInterrupt
	// outcomeEOF: Ctrl-D on an empty line.
	outcomeEOF
	// outcomeUnhandled: no binding for this key. It is unreachable in
	// product code, and key_internal_test.go is what makes that true — a key
	// constant added without a case below returns this and fails the suite,
	// rather than silently doing nothing forever.
	outcomeUnhandled
)

// handle applies one key. It is the entire editing model: no io, no terminal,
// no escape sequences, so a test drives it with keys and asserts on the
// resulting line.
func (s *session) handle(k key, r rune) outcome {
	switch k {
	case keyRune:
		s.line.insert(r)
	case keyEnter:
		return outcomeAccept
	case keyInterrupt:
		return outcomeInterrupt
	case keyEOF:
		// Ctrl-D means two things and the line decides which, exactly as
		// readline has since forever: end of input on an empty line, delete
		// the character under the cursor on a line with text in it. Picking
		// one would break either "exit" or "delete", and users expect both.
		if s.line.empty() {
			return outcomeEOF
		}
		s.line.deleteForward()
	case keyBackspace:
		s.line.backspace()
	case keyDeleteForward:
		s.line.deleteForward()
	case keyLeft:
		s.line.left()
	case keyRight:
		s.line.right()
	case keyUp:
		s.historyPrev()
	case keyDown:
		s.historyNext()
	case keyHome:
		s.line.home()
	case keyEnd:
		s.line.end()
	case keyKillToEnd:
		s.line.killToEnd()
	case keyKillToStart:
		s.line.killToStart()
	case keyIgnored:
		// Recognised, consumed in full, and dropped on purpose. This case
		// exists so "ignored" is a decision with a line of code behind it
		// rather than something that falls through to the guard below.
	default:
		return outcomeUnhandled
	}
	return outcomeContinue
}
