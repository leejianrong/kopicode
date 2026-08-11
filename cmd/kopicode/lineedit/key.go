package lineedit

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
)

// key is one editing command, decoded from the bytes a terminal sends.
//
// Every constant here must satisfy three things at once, and
// key_internal_test.go parses this file to make sure a new one cannot land
// having satisfied only two: it needs a name, a byte sequence that produces it,
// and a binding in (*session).handle. A key that decodes but does nothing is
// the failure mode this guard exists for — it looks like a working feature
// until someone presses it.
type key int

const (
	// keyRune is an ordinary character. The decoded rune travels beside it;
	// every other key carries no rune.
	keyRune key = iota
	keyEnter
	keyInterrupt
	keyEOF
	keyBackspace
	keyDeleteForward
	keyLeft
	keyRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyKillToEnd
	keyKillToStart
	// keyIgnored is a sequence that was recognised, consumed in full, and
	// deliberately dropped: an unbound control byte, or an escape sequence
	// this editor has no use for. It is a distinct value rather than an
	// error because the important part is that the *whole* sequence was
	// eaten. A decoder that gives up halfway through F5 leaves "15~" to be
	// typed into the user's line.
	keyIgnored
)

// keyNames is the only place a key gets a human-readable name, so a test
// failure names the key rather than an integer.
var keyNames = map[key]string{
	keyRune:          "keyRune",
	keyEnter:         "keyEnter",
	keyInterrupt:     "keyInterrupt",
	keyEOF:           "keyEOF",
	keyBackspace:     "keyBackspace",
	keyDeleteForward: "keyDeleteForward",
	keyLeft:          "keyLeft",
	keyRight:         "keyRight",
	keyUp:            "keyUp",
	keyDown:          "keyDown",
	keyHome:          "keyHome",
	keyEnd:           "keyEnd",
	keyKillToEnd:     "keyKillToEnd",
	keyKillToStart:   "keyKillToStart",
	keyIgnored:       "keyIgnored",
}

func (k key) String() string {
	if name, ok := keyNames[k]; ok {
		return name
	}
	return "key(" + strconv.Itoa(int(k)) + ")"
}

// The control bytes this editor binds. Raw mode turns off ISIG, ICANON and
// IXON, so these arrive as ordinary bytes rather than as signals or as flow
// control — Ctrl-C is 0x03 in the stream, not a SIGINT.
const (
	ctrlA   = 0x01 // start of line
	ctrlC   = 0x03 // interrupt
	ctrlD   = 0x04 // end of input on an empty line, delete-forward otherwise
	ctrlE   = 0x05 // end of line
	ctrlH   = 0x08 // backspace, on terminals that send BS rather than DEL
	ctrlJ   = 0x0a // line feed
	ctrlK   = 0x0b // kill to end of line
	ctrlM   = 0x0d // carriage return — what Enter sends in raw mode
	ctrlU   = 0x15 // kill to start of line
	escByte = 0x1b // the introducer of every escape sequence
	del     = 0x7f // backspace, on terminals that send DEL
)

// keyReader decodes a terminal's byte stream into keys.
//
// **It reads one rune at a time from a buffered reader, and that is the whole
// answer to escape sequences split across reads.** An arrow key is three bytes
// (ESC [ A) and nothing guarantees they arrive together: a pipe, a slow serial
// line, or a terminal under load will hand over the ESC and make you wait for
// the rest. A decoder that inspects "whatever this Read returned" works
// interactively on a fast local terminal and mangles input everywhere else,
// which is the worst shape of bug to have — it passes every test you run by
// hand.
//
// Asking for the next rune instead of the next buffer makes the split a
// non-event: bufio blocks until the byte exists, and the decoder cannot tell
// how it was chunked. TestSplitEscapeSequences proves it at every possible
// split point rather than at the one someone thought of.
type keyReader struct {
	r *bufio.Reader
}

// next returns the next key. For keyRune the second result is the character;
// for every other key it is zero.
//
// An error is returned only when the underlying reader fails or ends. A
// sequence the editor does not understand is keyIgnored, not an error: a
// terminal sending a key we have not bound is normal, and refusing to read
// further would end the user's session over an F5.
func (kr *keyReader) next() (key, rune, error) {
	r, _, err := kr.r.ReadRune()
	if err != nil {
		return keyIgnored, 0, err
	}

	switch r {
	case ctrlA:
		return keyHome, 0, nil
	case ctrlC:
		return keyInterrupt, 0, nil
	case ctrlD:
		return keyEOF, 0, nil
	case ctrlE:
		return keyEnd, 0, nil
	case ctrlH, del:
		return keyBackspace, 0, nil
	case ctrlJ, ctrlM:
		return keyEnter, 0, nil
	case ctrlK:
		return keyKillToEnd, 0, nil
	case ctrlU:
		return keyKillToStart, 0, nil
	case escByte:
		return kr.escapeSequence()
	}

	// Every other C0 control, and DEL's neighbours, are unbound. Inserting
	// them would put an unprintable byte in the line the model is about to
	// be sent.
	if r < 0x20 {
		return keyIgnored, 0, nil
	}
	return keyRune, r, nil
}

// escapeSequence decodes what follows an ESC.
//
// Two introducers matter. CSI (ESC [) is what a terminal sends for arrows and
// navigation in its normal cursor mode; SS3 (ESC O) is what the same keys send
// in *application* cursor mode, which is what many terminals switch to inside a
// full-screen program and what some send unconditionally. Handling only CSI
// works until it doesn't, on someone else's terminal.
//
// Anything else after ESC — including ESC on its own followed by a letter,
// which is how Alt-key combinations arrive — puts the following rune back and
// reports the ESC as ignored. So Alt-a inserts "a" rather than swallowing it,
// and a bare Escape does nothing.
func (kr *keyReader) escapeSequence() (key, rune, error) {
	r, _, err := kr.r.ReadRune()
	if err != nil {
		// ESC at the very end of the stream. Report the ESC as ignored and
		// let the next call surface the error, so a stream ending in ESC
		// does not lose the line that came before it.
		if errors.Is(err, io.EOF) {
			return keyIgnored, 0, nil
		}
		return keyIgnored, 0, err
	}

	switch r {
	case '[':
		return kr.csi()
	case 'O':
		return kr.ss3()
	default:
		if unreadErr := kr.r.UnreadRune(); unreadErr != nil {
			return keyIgnored, 0, unreadErr
		}
		return keyIgnored, 0, nil
	}
}

// csi consumes a full CSI sequence and maps its final byte.
//
// The shape is fixed by ECMA-48: zero or more parameter bytes (0x30-0x3F),
// zero or more intermediates (0x20-0x2F), then exactly one final byte
// (0x40-0x7E). Parsing it structurally rather than matching known strings is
// what lets an unrecognised sequence be consumed *whole* — the difference
// between F5 doing nothing and F5 typing "15~" into the prompt.
//
// Modifier parameters are read and discarded. Ctrl-Left arrives as
// ESC [ 1 ; 5 D and moves one character rather than one word; that is a
// deliberate degradation, not an oversight. Word motion is not in this card.
func (kr *keyReader) csi() (key, rune, error) {
	var params strings.Builder
	for {
		r, _, err := kr.r.ReadRune()
		if err != nil {
			return keyIgnored, 0, err
		}
		switch {
		case r >= 0x30 && r <= 0x3f: // parameter bytes
			params.WriteRune(r)
		case r >= 0x20 && r <= 0x2f: // intermediate bytes
		case r >= 0x40 && r <= 0x7e: // final byte
			return csiKey(params.String(), r), 0, nil
		default:
			// Not a valid CSI byte at all. The sequence was truncated by
			// something; stop here rather than eating the rest of the line.
			return keyIgnored, 0, nil
		}
	}
}

// csiKey maps a parsed CSI sequence to a key.
func csiKey(params string, final rune) key {
	switch final {
	case 'A':
		return keyUp
	case 'B':
		return keyDown
	case 'C':
		return keyRight
	case 'D':
		return keyLeft
	case 'H':
		return keyHome
	case 'F':
		return keyEnd
	case '~':
		// The tilde finals are distinguished by their first parameter, and
		// terminals disagree about which number Home and End use.
		switch leadingNumber(params) {
		case 1, 7:
			return keyHome
		case 3:
			return keyDeleteForward
		case 4, 8:
			return keyEnd
		}
	}
	return keyIgnored
}

// ss3 consumes an SS3 sequence — ESC O plus one final byte — as sent by a
// terminal in application cursor mode.
func (kr *keyReader) ss3() (key, rune, error) {
	r, _, err := kr.r.ReadRune()
	if err != nil {
		return keyIgnored, 0, err
	}
	switch r {
	case 'A':
		return keyUp, 0, nil
	case 'B':
		return keyDown, 0, nil
	case 'C':
		return keyRight, 0, nil
	case 'D':
		return keyLeft, 0, nil
	case 'H':
		return keyHome, 0, nil
	case 'F':
		return keyEnd, 0, nil
	}
	return keyIgnored, 0, nil
}

// leadingNumber returns the first numeric parameter of a CSI parameter string,
// or -1 when there is none. "1;5" yields 1; "" yields -1.
func leadingNumber(params string) int {
	end := 0
	for end < len(params) && params[end] >= '0' && params[end] <= '9' {
		end++
	}
	if end == 0 {
		return -1
	}
	n, err := strconv.Atoi(params[:end])
	if err != nil {
		return -1
	}
	return n
}
