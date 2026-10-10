package main

import (
	"encoding/json"
	"slices"

	"github.com/leejianrong/kopicode/internal/build"
)

// serveProtocol is the serve wire's version. It moves only when a change would
// break an existing client; an additive field or method adds a feature instead.
const serveProtocol = 1

// features names what this binary supports, as stable strings a client can test
// for instead of scraping `--help` or provoking a usage error.
//
// The rules that keep it useful (#175): a name is added in the change that ships
// the capability, is never renamed, and is removed only with a protocol bump.
// Names are lower-case dotted paths, and the list is sorted so two builds with
// the same capabilities print the same bytes.
var features = []string{
	"allow_commands",
	"ask.request",
	"consent.note",
	"consent_mode.auto",
	"consent_mode.remote_interactive",
	"consent_mode.unattended_policy",
	"consent_request.command",
	"consent_timeout.flag",
	"consent_timeout.session",
	"mcp",
	"provider_url.flag",
	"serve.listen",
	"server.hello",
	"server.sessions",
	"session.close",
	"session.events_since",
	"session.handoff",
	"session.limits",
	"session.read_only",
	"session.usage",
	"usage.context",
	"usage.context_window",
	"usage.cost",
	"usage.tokens_split",
}

// capabilities is what `kopicode version --json` prints and what serve answers
// `server.hello` with: the same object both ways, so a client can ask whichever
// it can reach first.
//
// Version is human-facing (a git describe) and must not be parsed; TreeState is
// the machine-readable dirty bit, exactly as internal/build documents.
type capabilities struct {
	Version   string   `json:"version"`
	Commit    string   `json:"commit"`
	TreeState string   `json:"tree_state"`
	Source    string   `json:"source"`
	Protocol  int      `json:"protocol"`
	Features  []string `json:"features"`
}

func currentCapabilities() capabilities {
	id := build.Current()
	return capabilities{
		Version:   id.Version,
		Commit:    id.Commit,
		TreeState: string(id.TreeState),
		Source:    id.Source,
		Protocol:  serveProtocol,
		Features:  slices.Clone(features),
	}
}

// marshalCapabilities renders c as one line of JSON.
func marshalCapabilities(c capabilities) ([]byte, error) {
	return json.Marshal(c)
}
