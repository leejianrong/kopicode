package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"

	"github.com/leejianrong/kopicode/internal/journal"
)

// This file is KOP-214: the fields every driven-run note used to copy out of a
// journal by hand with jq (docs/agent-driver/rubric.md, R1 to R6 and R10), as a
// digest the journal produces. It is derived from the record and from nothing
// else, so a number in a report is a number a reader can find in events.jsonl by
// its seq. It judges nothing: whether a rejection mattered, or the final message
// was honest, is the scorer's call, and the report only points at the events.

// SessionReport is one session's record boiled down to the rubric's mechanical
// fields. Every Seq is the event's own, for a note to cite.
type SessionReport struct {
	ID        string `json:"id"`
	ModelID   string `json:"model_id,omitempty"`
	Version   string `json:"version,omitempty"`
	StartedAt string `json:"started_at,omitempty"`

	// Ended is nil when the record has no SessionEnded: still running, or the
	// process was killed before it could close.
	Ended *ReportEnd `json:"ended,omitempty"`

	// Turns is the number of distinct turns that sent a provider request, and
	// Requests the number of requests, retries and repairs included.
	Turns    int `json:"turns"`
	Requests int `json:"requests"`
	// Retries are transport retries the provider client journalled.
	Retries int     `json:"retries"`
	Tokens  int     `json:"tokens"`
	CostUSD float64 `json:"cost_usd"`

	// Verification is every VerificationRun in order, and Last the final one.
	Verification ReportVerification `json:"verification"`

	Edits ReportEdits `json:"edits"`

	// Tools counts parsed calls by tool, and Routes by "route/encoding".
	Tools  map[string]int `json:"tools,omitempty"`
	Routes map[string]int `json:"routes,omitempty"`

	// SyntaxGate lists the runs that did not exit 0.
	SyntaxGate ReportSyntaxGate `json:"syntax_gate"`

	Permissions ReportPermissions `json:"permissions"`

	// Final is the last AssistantMessage, which R8 reads against the rest.
	Final *ReportMessage `json:"final_message,omitempty"`
}

// ReportEnd is the SessionEnded event.
type ReportEnd struct {
	Seq      uint64 `json:"seq"`
	Reason   string `json:"reason"`
	ExitCode int    `json:"exit_code"`
}

// ReportVerification summarises the VerificationRun events. A run with a Skip set
// checked nothing, whatever the session's own exit code says.
type ReportVerification struct {
	Runs    int            `json:"runs"`
	Skipped int            `json:"skipped"`
	Failed  int            `json:"failed"`
	Last    *ReportVerifyN `json:"last,omitempty"`
}

// ReportVerifyN is one VerificationRun.
type ReportVerifyN struct {
	Seq      uint64   `json:"seq"`
	Command  []string `json:"command,omitempty"`
	ExitCode int      `json:"exit_code"`
	Skip     string   `json:"skip,omitempty"`
}

// ReportEdits lists applied and rejected edits, and the rejections after which
// the same path never had an edit applied (rubric R3's "never followed by").
type ReportEdits struct {
	Applied     []ReportEdit `json:"applied,omitempty"`
	Rejected    []ReportEdit `json:"rejected,omitempty"`
	Unrecovered []uint64     `json:"unrecovered,omitempty"`
}

// ReportEdit is one EditApplied or EditRejected.
type ReportEdit struct {
	Seq    uint64 `json:"seq"`
	Path   string `json:"path"`
	Mode   string `json:"mode,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ReportSyntaxGate counts the gate's runs and lists the failing ones.
type ReportSyntaxGate struct {
	Runs   int          `json:"runs"`
	Failed []ReportGate `json:"failed,omitempty"`
}

// ReportGate is one SyntaxGateRun that exited non-zero.
type ReportGate struct {
	Seq      uint64 `json:"seq"`
	Path     string `json:"path"`
	ExitCode int    `json:"exit_code"`
}

// ReportPermissions counts requests and decisions, and lists the denials.
type ReportPermissions struct {
	Requested int            `json:"requested"`
	Decisions map[string]int `json:"decisions,omitempty"`
	Denied    []ReportDenial `json:"denied,omitempty"`
}

// ReportDenial is one PermissionDecided that was not an allow.
type ReportDenial struct {
	Seq    uint64 `json:"seq"`
	Source string `json:"source"`
	Reason string `json:"reason,omitempty"`
}

// ReportMessage is an AssistantMessage. Text is empty, with Spilled set, when the
// message was over the inline limit and lives in a blob: the report does not
// fetch blobs, the same choice [ListSessions] makes for a first message.
type ReportMessage struct {
	Seq     uint64 `json:"seq"`
	Text    string `json:"text,omitempty"`
	Spilled bool   `json:"spilled,omitempty"`
}

// ReportSession reads session id under dir and returns its report. An id with no
// record is an error; a record a crash left truncated reports what decoded before
// the break, as [ListSessions] does.
func ReportSession(ctx context.Context, dir, id string) (SessionReport, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return SessionReport{}, fmt.Errorf("engine: resolving %q: %w", dir, err)
	}
	r := SessionReport{
		ID:     id,
		Tools:  map[string]int{},
		Routes: map[string]int{},
	}
	r.Permissions.Decisions = map[string]int{}
	turns := map[int]bool{}
	n := 0
	// A rejected edit is unrecovered until an edit to the same path lands.
	pending := map[string][]uint64{}

	for ev, rerr := range journal.ReadSession(ctx, journal.SessionDir(abs, id)) {
		if rerr != nil {
			if n == 0 {
				return SessionReport{}, fmt.Errorf("engine: reading session %q: %w", id, rerr)
			}
			break
		}
		n++
		switch p := ev.Payload.(type) {
		case journal.SessionStarted:
			r.ModelID = p.ModelID
			r.Version = p.Build.Version
			r.StartedAt = ev.Time.UTC().Format("2006-01-02T15:04:05Z")
		case journal.SessionEnded:
			r.Ended = &ReportEnd{Seq: ev.Seq, Reason: p.Reason, ExitCode: p.ExitCode}
		case journal.ProviderRequest:
			r.Requests++
			turns[ev.Turn] = true
		case journal.ProviderRetried:
			r.Retries++
		case journal.ProviderResponse:
			r.Tokens += p.Tokens.Total
			if p.CostUSD != nil {
				r.CostUSD += *p.CostUSD
			}
		case journal.VerificationRun:
			v := &r.Verification
			v.Runs++
			if p.Skip != "" {
				v.Skipped++
			} else if p.ExitCode != 0 {
				v.Failed++
			}
			v.Last = &ReportVerifyN{Seq: ev.Seq, Command: p.Command, ExitCode: p.ExitCode, Skip: p.Skip}
		case journal.EditApplied:
			r.Edits.Applied = append(r.Edits.Applied, ReportEdit{Seq: ev.Seq, Path: p.Path, Mode: p.Mode})
			delete(pending, p.Path)
		case journal.EditRejected:
			r.Edits.Rejected = append(r.Edits.Rejected, ReportEdit{Seq: ev.Seq, Path: p.Path, Mode: p.Mode, Reason: p.Reason})
			pending[p.Path] = append(pending[p.Path], ev.Seq)
		case journal.ToolCallParsed:
			r.Tools[p.Tool]++
			r.Routes[p.Route+"/"+p.ArgEncoding]++
		case journal.SyntaxGateRun:
			r.SyntaxGate.Runs++
			if p.Ran && p.ExitCode != 0 {
				r.SyntaxGate.Failed = append(r.SyntaxGate.Failed, ReportGate{Seq: ev.Seq, Path: p.Path, ExitCode: p.ExitCode})
			}
		case journal.PermissionRequested:
			r.Permissions.Requested++
		case journal.PermissionDecided:
			r.Permissions.Decisions[p.Decision+"/"+p.Source]++
			if p.Decision != "allow" {
				r.Permissions.Denied = append(r.Permissions.Denied, ReportDenial{Seq: ev.Seq, Source: p.Source, Reason: p.Reason})
			}
		case journal.AssistantMessage:
			m := &ReportMessage{Seq: ev.Seq, Text: p.Text.Inline}
			if p.Text.Inline == "" && p.Text.Blob != "" {
				m.Spilled = true
			}
			r.Final = m
		}
	}
	r.Turns = len(turns)
	for _, seqs := range pending {
		r.Edits.Unrecovered = append(r.Edits.Unrecovered, seqs...)
	}
	slices.Sort(r.Edits.Unrecovered)
	return r, nil
}

// SortedCounts returns m's keys in order with their counts, for a stable listing.
func SortedCounts(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
