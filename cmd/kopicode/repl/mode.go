package repl

import "strings"

// ModeControl is what /mode needs from the session (ADR-0023): which mode it is
// in and a way to change it. It is passed in so this package never holds the
// session itself.
type ModeControl struct {
	// Auto reports whether the session is answering consent itself.
	Auto func() bool
	// Set switches into auto (true) or default (false) mode. It returns false
	// when the session cannot switch.
	Set func(auto bool) bool
}

// Mode names, as typed after /mode and given to --mode.
const (
	ModeDefault = "default"
	ModeAuto    = "auto"
)

const autoWarning = "auto mode runs shell commands inside this directory without asking. " +
	"A fixed never-allow list still refuses sudo, rm outside the tree, forced pushes, global " +
	"installs and the like, but it is not a sandbox."

// switchMode handles /mode [default|auto].
func (l *Loop) switchMode(arg string) {
	if l.mode.Auto == nil || l.mode.Set == nil {
		l.Notice("/mode is not available in this session")
		return
	}
	switch strings.TrimSpace(arg) {
	case "":
		l.Notice("mode: " + modeName(l.mode.Auto()) + " (/mode default or /mode auto to change)")
	case ModeAuto:
		if !l.mode.Set(true) {
			l.Notice("this session cannot switch to auto mode")
			return
		}
		l.Notice("mode: auto. " + autoWarning)
	case ModeDefault:
		if !l.mode.Set(false) {
			l.Notice("this session cannot switch modes")
			return
		}
		l.Notice("mode: default. You will be asked before shell commands and writes outside the tree.")
	default:
		l.Notice("unknown mode " + `"` + strings.TrimSpace(arg) + `"` + ": the modes are default and auto")
	}
}

func modeName(auto bool) string {
	if auto {
		return ModeAuto
	}
	return ModeDefault
}

// AutoWarning is the line printed when a session starts in auto mode, so the
// headline front end can say it once, in the same words /mode auto does.
func AutoWarning() string { return autoWarning }
