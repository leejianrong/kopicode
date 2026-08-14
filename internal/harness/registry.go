package harness

import (
	"sort"

	"github.com/leejianrong/kopicode/internal/provider"
)

// DefaultModelID is the built-in default, the bottom of ADR-0007 decision 2's
// precedence chain.
//
// It was checked against OpenRouter's catalogue under exactly this string on
// 2026-08-14 (docs/provider-pin.md), which matters more than it looks: an
// unrecognised id is a usage error, so a typo here would make the binary refuse
// to start out of the box. TestDefaultModelResolves is the standing check.
const DefaultModelID = "qwen/qwen3-coder-next"

// Entry is one registry row: a supported model, the harness configuration it
// gets by default, and its default provider pin (ADR-0007 decision 5).
type Entry struct {
	// ModelID is the provider's model identifier, canonical spelling only.
	// ADR-0007 rejects aliases: a report saying `qwen` is not reproducible in
	// eighteen months.
	ModelID string

	// HarnessConfig is the name of the configuration this model gets unless
	// --harness says otherwise.
	HarnessConfig string

	// Pin is the routing every request for this model declares. It is here
	// because ADR-0007 decision 5 puts it here — an arm is (model × harness
	// config × provider pin), and until this row existed the pin lived only in
	// fixture data and in docs/provider-pin.md, with nothing in code to
	// contradict.
	Pin provider.Pin

	// Validated is the date the mapping was last checked against reality, as
	// ADR-0007 §Consequences asks: "a row that claims a harness configuration
	// the model has not actually been run against is a claim the registry
	// cannot check". It records when a person last looked, which is the only
	// honest thing a row can say about that.
	Validated string
}

// registry is every model this binary supports.
//
// A slice and not a map, so listing it is deterministic without sorting and so
// the row order in the source is the row order in an error message.
//
// It has one row. The README names three further models — two A/B candidates
// and a frontier ceiling — and they are deliberately absent: a row carries a
// provider pin, only qwen/qwen3-coder-next has one that was chosen from observed
// traffic (docs/provider-pin.md), and inventing pins for the other three would
// be exactly the fabrication that document warns about. Adding a model is a row
// and a pull request.
var registry = []Entry{
	{
		// Spelled out rather than written as DefaultModelID, so that "this
		// model is supported" and "this model is the default" are two
		// statements a test can hold to each other. Sharing the constant would
		// make a typo in the default rename the row along with it, and
		// TestDefaultModelResolves would pass over a binary that cannot start.
		ModelID:       "qwen/qwen3-coder-next",
		HarnessConfig: DefaultConfigName,
		Pin: provider.Pin{
			Order:          []string{"parasail/bf16"},
			AllowFallbacks: false,
			Quantizations:  []string{"bf16"},
		},
		// The pin was chosen and argued on this date. The harness mapping is
		// trivial while there is one configuration; the date is honest about
		// what was actually checked.
		Validated: "2026-08-14",
	},
}

// Lookup returns the registry row for a model id, and whether it exists.
//
// Lookup is exact. A near miss is a finding the error message reports, never
// something resolved silently: ADR-0007 rejects an open registry precisely
// because a typo would become a plausible run rather than a refusal.
func Lookup(modelID string) (Entry, bool) {
	for _, e := range registry {
		if e.ModelID == modelID {
			return e, true
		}
	}
	return Entry{}, false
}

// SupportedModels lists every registered model id, sorted.
//
// Sorted rather than in registry order because this reaches a user-facing error
// message and a list whose order depends on where a row was inserted is one more
// thing that changes between two runs for no reason.
func SupportedModels() []string {
	out := make([]string, 0, len(registry))
	for _, e := range registry {
		out = append(out, e.ModelID)
	}
	sort.Strings(out)
	return out
}

// ConfigByName returns a compiled-in harness configuration, and whether it
// exists.
func ConfigByName(name string) (Config, bool) {
	c, ok := configs[name]
	return c, ok
}

// ConfigNames lists every compiled-in harness configuration, sorted.
func ConfigNames() []string {
	out := make([]string, 0, len(configs))
	for name := range configs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// nearest returns the candidate closest to want, when one is close enough to be
// worth suggesting, and "" otherwise.
//
// The bound is deliberate. With one registered model, a confident wrong
// suggestion is worse than no suggestion: "did you mean qwen/qwen3-coder-next?"
// in answer to `gpt-5` reads as though the harness knows something it does not,
// and the supported list — which is always printed — already says everything
// true. So a suggestion has to be a plausible typo of the candidate: at most
// three single-character edits, and at most a fifth of the candidate's length.
func nearest(want string, candidates []string) string {
	const maxEdits = 3

	best, bestDist := "", -1
	for _, c := range candidates {
		d := editDistance(want, c)
		limit := min(maxEdits, max(1, len(c)/5))
		if d > limit {
			continue
		}
		if bestDist < 0 || d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

// editDistance is Levenshtein distance over bytes, with two rows rather than a
// full matrix.
//
// Bytes and not runes: every candidate is a provider model id, which is ASCII,
// and a rune-aware version would be more code guarding a case that cannot
// arrive. A multi-byte typo simply scores further away, which errs towards no
// suggestion — the direction this function is supposed to err in.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
