package permission

import (
	"errors"
	"fmt"
	"strings"
)

// This file is the tokenizer behind [AutoPolicy] (ADR-0017): it splits one
// shell command line into simple commands, with quoting removed and every
// construct whose value cannot be known statically marked as such.
//
// It is deliberately not a POSIX shell parser and does not try to be one. Its
// contract is narrower and one-sided: for every line it accepts, the commands
// it reports are a superset of what a real /bin/sh would run, and a construct
// it cannot account for is an error, which the policy turns into a denial. The
// failure direction is always "refuse", never "guess".
//
// What it recognises: single and double quotes, backslash escapes, the command
// separators ; & && || | |& and newline, subshell parentheses, $(...) and
// `...` and <(...) and >(...) substitutions (their inner text is returned for
// the caller to analyse as a line of its own), and the output/input
// redirections with their targets.
//
// What it refuses outright: an unterminated quote or substitution, a heredoc
// or here-string (the body of `sh <<EOF` is code and this tokenizer would have
// to guess where it ends), and a redirection with no target.

// shWord is one word of a command, quote-stripped.
type shWord struct {
	// text is the word with quoting removed. For a dynamic word it still holds
	// the literal "$" or backtick characters, which is what makes it
	// recognisable and never mistakable for a path.
	text string

	// dynamic is true when the word's value depends on something the
	// tokenizer cannot evaluate: a parameter expansion, a command or process
	// substitution, or an ANSI-C quote ($'...'), all outside single quotes. A
	// dynamic word in command position is a command whose name is unknown.
	dynamic bool

	// subs holds the inner text of every $(...), `...`, <(...) and >(...) in
	// the word, for the caller to analyse as command lines of their own.
	subs []string
}

// shRedirect is one redirection of a command.
type shRedirect struct {
	// op is the operator: ">", ">>", ">|", "&>", "&>>", ">&", "<", "<&", "<>".
	op string

	// target is the word after the operator.
	target shWord
}

// writes reports whether the redirection can create or truncate a file. An
// input redirection reads, and reads are not a consent question (see [Gate]).
func (r shRedirect) writes() bool {
	switch r.op {
	case ">", ">>", ">|", "&>", "&>>", "<>":
		return true
	case ">&":
		// ">&2" duplicates a descriptor; ">&file" writes one.
		return !isFDTarget(r.target)
	}
	return false
}

// isFDTarget reports whether w names a file descriptor (or "-", which closes
// one) rather than a file.
func isFDTarget(w shWord) bool {
	if w.dynamic || w.text == "" {
		return false
	}
	if w.text == "-" {
		return true
	}
	for _, r := range w.text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// shCommand is one simple command: its words and redirections.
type shCommand struct {
	words  []shWord
	redirs []shRedirect

	// pipeNext is true when this command's output is piped into the next one.
	pipeNext bool
}

var errHeredoc = errors.New("heredocs and here-strings cannot be analysed — write the file with write_file instead")

// parseShell tokenizes line. See the file comment for what it accepts.
func parseShell(line string) ([]shCommand, error) {
	p := &shParser{src: []rune(line)}
	if err := p.run(); err != nil {
		return nil, err
	}
	return p.cmds, nil
}

type shParser struct {
	src []rune
	pos int

	cmds []shCommand
	cur  shCommand

	buf     strings.Builder
	inWord  bool
	dynamic bool
	subs    []string

	pending *shRedirect // a redirection still waiting for its target word
}

func (p *shParser) peek(off int) rune {
	if p.pos+off < len(p.src) {
		return p.src[p.pos+off]
	}
	return 0
}

func (p *shParser) startWord() { p.inWord = true }

func (p *shParser) flushWord() {
	if !p.inWord {
		return
	}
	w := shWord{text: p.buf.String(), dynamic: p.dynamic, subs: p.subs}
	p.buf.Reset()
	p.inWord, p.dynamic, p.subs = false, false, nil
	if p.pending != nil {
		p.pending.target = w
		p.cur.redirs = append(p.cur.redirs, *p.pending)
		p.pending = nil
		return
	}
	p.cur.words = append(p.cur.words, w)
}

func (p *shParser) endCommand(pipe bool) error {
	p.flushWord()
	if p.pending != nil {
		return fmt.Errorf("redirection %q has no target", p.pending.op)
	}
	if len(p.cur.words) > 0 || len(p.cur.redirs) > 0 {
		p.cur.pipeNext = pipe
		p.cmds = append(p.cmds, p.cur)
	}
	p.cur = shCommand{}
	return nil
}

func (p *shParser) run() error {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case ' ', '\t', '\r':
			p.flushWord()
			p.pos++

		case '\n':
			if err := p.endCommand(false); err != nil {
				return err
			}
			p.pos++

		case '\\':
			if p.peek(1) == '\n' { // line continuation
				p.pos += 2
				continue
			}
			p.startWord()
			if p.pos+1 < len(p.src) {
				p.buf.WriteRune(p.src[p.pos+1])
				p.pos += 2
			} else {
				p.buf.WriteRune('\\')
				p.pos++
			}

		case '\'':
			end := indexRune(p.src, p.pos+1, '\'')
			if end < 0 {
				return errors.New("unterminated single quote")
			}
			p.startWord()
			p.buf.WriteString(string(p.src[p.pos+1 : end]))
			p.pos = end + 1

		case '"':
			if err := p.doubleQuoted(); err != nil {
				return err
			}

		case '$':
			if err := p.dollar(); err != nil {
				return err
			}

		case '`':
			inner, next, err := p.backtick(p.pos)
			if err != nil {
				return err
			}
			p.startWord()
			p.dynamic = true
			p.buf.WriteString("`")
			p.subs = append(p.subs, inner)
			p.pos = next

		case ';':
			if err := p.endCommand(false); err != nil {
				return err
			}
			p.pos++

		case '&':
			if p.peek(1) == '>' { // &> and &>>
				p.flushWord()
				op := "&>"
				p.pos += 2
				if p.peek(0) == '>' {
					op = "&>>"
					p.pos++
				}
				p.pending = &shRedirect{op: op}
				continue
			}
			if err := p.endCommand(false); err != nil {
				return err
			}
			p.pos++
			if p.peek(0) == '&' {
				p.pos++
			}

		case '|':
			p.pos++
			switch p.peek(0) {
			case '|':
				p.pos++
				if err := p.endCommand(false); err != nil {
					return err
				}
			case '&':
				p.pos++
				if err := p.endCommand(true); err != nil {
					return err
				}
			default:
				if err := p.endCommand(true); err != nil {
					return err
				}
			}

		case '(', ')':
			if err := p.endCommand(false); err != nil {
				return err
			}
			p.pos++

		case '<', '>':
			if err := p.redirect(); err != nil {
				return err
			}

		case '#':
			if p.inWord {
				p.buf.WriteRune(c)
				p.pos++
				continue
			}
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}

		default:
			p.startWord()
			p.buf.WriteRune(c)
			p.pos++
		}
	}
	return p.endCommand(false)
}

// doubleQuoted consumes a "..." string, collecting substitutions inside it.
func (p *shParser) doubleQuoted() error {
	p.startWord()
	p.pos++ // opening quote
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case '"':
			p.pos++
			return nil
		case '\\':
			next := p.peek(1)
			switch next {
			case '$', '`', '"', '\\':
				p.buf.WriteRune(next)
				p.pos += 2
			case '\n':
				p.pos += 2
			default:
				p.buf.WriteRune('\\')
				p.pos++
			}
		case '$':
			if err := p.dollar(); err != nil {
				return err
			}
		case '`':
			inner, next, err := p.backtick(p.pos)
			if err != nil {
				return err
			}
			p.dynamic = true
			p.buf.WriteString("`")
			p.subs = append(p.subs, inner)
			p.pos = next
		default:
			p.buf.WriteRune(c)
			p.pos++
		}
	}
	return errors.New("unterminated double quote")
}

// dollar consumes a "$..." construct at p.pos.
func (p *shParser) dollar() error {
	p.startWord()
	p.dynamic = true
	switch p.peek(1) {
	case '(':
		inner, next, err := p.balanced(p.pos+2, '(', ')')
		if err != nil {
			return err
		}
		p.buf.WriteString("$(")
		p.subs = append(p.subs, inner)
		p.pos = next
	case '{':
		inner, next, err := p.balanced(p.pos+2, '{', '}')
		if err != nil {
			return err
		}
		p.buf.WriteString("${")
		if strings.Contains(inner, "$(") || strings.Contains(inner, "`") {
			p.subs = append(p.subs, inner)
		}
		p.pos = next
	default:
		// $name, $1, $?, $$, and $'ANSI-C' — the latter is why the following
		// quote is not consumed here: it is read as an ordinary single-quoted
		// string, and the word stays dynamic because its escapes are not
		// decoded.
		p.buf.WriteRune('$')
		p.pos++
	}
	return nil
}

// balanced reads from start to the delimiter closing an already-open one,
// honouring nesting of the same pair and skipping quoted regions. It returns
// the inner text and the position after the closer.
func (p *shParser) balanced(start int, open, closeR rune) (string, int, error) {
	depth := 1
	for i := start; i < len(p.src); i++ {
		switch p.src[i] {
		case '\\':
			i++
		case '\'':
			if open == '(' { // inside $(...) a single quote is a real quote
				end := indexRune(p.src, i+1, '\'')
				if end < 0 {
					return "", 0, errors.New("unterminated quote inside a substitution")
				}
				i = end
			}
		case '"':
			end := p.skipDouble(i + 1)
			if end < 0 {
				return "", 0, errors.New("unterminated quote inside a substitution")
			}
			i = end
		case open:
			depth++
		case closeR:
			depth--
			if depth == 0 {
				return string(p.src[start:i]), i + 1, nil
			}
		}
	}
	return "", 0, errors.New("unterminated substitution")
}

// skipDouble returns the index of the closing quote of a double-quoted region
// beginning at start, or -1.
func (p *shParser) skipDouble(start int) int {
	for i := start; i < len(p.src); i++ {
		switch p.src[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// backtick reads a `...` substitution opening at start.
func (p *shParser) backtick(start int) (string, int, error) {
	var b strings.Builder
	for i := start + 1; i < len(p.src); i++ {
		switch p.src[i] {
		case '\\':
			if i+1 < len(p.src) {
				i++
				if n := p.src[i]; n != '`' && n != '\\' && n != '$' {
					b.WriteRune('\\')
				}
				b.WriteRune(p.src[i])
			}
		case '`':
			return b.String(), i + 1, nil
		default:
			b.WriteRune(p.src[i])
		}
	}
	return "", 0, errors.New("unterminated backtick substitution")
}

// redirect consumes a < or > operator, or a <(...) / >(...) substitution.
func (p *shParser) redirect() error {
	c := p.src[p.pos]

	if p.peek(1) == '(' { // process substitution: part of the current word
		inner, next, err := p.balanced(p.pos+2, '(', ')')
		if err != nil {
			return err
		}
		p.startWord()
		p.dynamic = true
		p.buf.WriteRune(c)
		p.buf.WriteRune('(')
		p.subs = append(p.subs, inner)
		p.pos = next
		return nil
	}

	if p.pending != nil && !p.inWord {
		return fmt.Errorf("redirection %q has no target", p.pending.op)
	}

	// A bare descriptor number directly before the operator ("2>") belongs to
	// the operator, not to the command's arguments.
	if p.inWord && !p.dynamic && p.pending == nil && allDigits(p.buf.String()) {
		p.buf.Reset()
		p.inWord = false
		p.subs = nil
	} else {
		p.flushWord()
	}

	p.pos++
	var op string
	switch c {
	case '>':
		switch p.peek(0) {
		case '>':
			op = ">>"
			p.pos++
		case '|':
			op = ">|"
			p.pos++
		case '&':
			op = ">&"
			p.pos++
		default:
			op = ">"
		}
	case '<':
		switch p.peek(0) {
		case '<':
			return errHeredoc
		case '&':
			op = "<&"
			p.pos++
		case '>':
			op = "<>"
			p.pos++
		default:
			op = "<"
		}
	}
	p.pending = &shRedirect{op: op}
	return nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func indexRune(src []rune, from int, r rune) int {
	for i := from; i < len(src); i++ {
		if src[i] == r {
			return i
		}
	}
	return -1
}
