package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strconv"

	"github.com/leejianrong/kopicode/internal/anchor"
)

// PreimageVersion identifies the *encoding* below, not the configuration.
//
// It is in the preimage so that a change to how a field is encoded — a field
// reordered, a separator changed, a digest swapped for its plaintext — cannot
// produce a hash that collides with one written by an older binary. Bump it when
// the encoding changes, which is a different event from a configuration value
// changing.
const PreimageVersion = 1

// Hash is the harness config hash journal.SessionStarted records: the
// comparison key ADR-0005's paired method pools on, and the mechanism that makes
// ADR-0006 §7's experiment-series boundary automatic, because [anchor.Version]
// is inside the preimage and a bump to it therefore moves every arm's hash.
//
// It is 64 hex characters of SHA-256. Nothing parses it; it is compared.
func (c Config) Hash() string { return c.hashWith(anchor.Version) }

// hashWith is Hash with the anchor version as a parameter.
//
// The parameter exists so a test can vary it. anchor.Version is a constant, so
// the coupling ADR-0006 §7 requires — an anchor-format bump invalidating every
// recorded result — cannot otherwise be driven, and an untested coupling is one
// somebody tidying the preimage deletes.
func (c Config) hashWith(anchorVersion string) string {
	p := preimage{h: sha256.New()}

	// Order is fixed by this function and by nothing else. There is no
	// reflection here, no struct tag, and no map: the encoding is what a reader
	// can check line by line against ADR-0007 decision 6.
	p.num("preimage_version", PreimageVersion)

	// Not a field of Config. ADR-0006 §7 requires it, and decision 6 of
	// ADR-0007 restates it: an anchor-format bump is an experiment-series
	// boundary, and mixing the version in here is what enforces that
	// mechanically rather than by anyone remembering.
	p.str("anchor_version", anchorVersion)

	p.str("name", c.Name)
	p.num("version", c.Version)

	// By digest, per decision 6. A prompt is large and a hash of it is the
	// whole of what the comparison needs.
	p.digest("system_prompt", c.SystemPrompt)

	// Names only, and narrower than ADR-0007 decision 6 asks for: it wants the
	// tool set "as presented to the model — names, schemas, descriptions", and
	// no rendered catalogue exists in this repository yet (provider.Request
	// carries none; KAN-776 owns that shape). Hashing an invented rendering
	// would put bytes in the preimage that nothing ever sent. So the gap is
	// stated rather than papered over: a description change is currently
	// invisible to this hash, and whoever lands the catalogue must extend
	// Config.ToolSet and bump PreimageVersion.
	p.strs("tool_set", c.ToolSet)

	p.strs("parse_routes", c.ParseRoutes)
	p.num("repair_budget", c.RepairBudget)
	p.num("max_turns", c.MaxTurns)
	p.num("token_budget", c.TokenBudget)

	p.bool("verification_forced", c.Verification.Forced)
	p.str("verification_source", string(c.Verification.Source))

	p.float("sampling_temperature", c.Sampling.Temperature)
	p.float("sampling_top_p", c.Sampling.TopP)
	p.num("sampling_max_tokens", c.Sampling.MaxTokens)
	p.str("sampling_seed_policy", string(c.Sampling.SeedPolicy))
	p.num64("sampling_seed", c.Sampling.Seed)

	return hex.EncodeToString(p.h.Sum(nil))
}

// preimage writes length-prefixed key/value pairs into a hash.
//
// Length prefixes rather than separators, for the reason internal/anchor uses
// them: a delimiter can appear inside a value, and two different configurations
// whose concatenations happen to agree would collide. A length cannot be forged
// by content.
type preimage struct{ h hash.Hash }

func (p preimage) write(key, value string) {
	var b []byte
	b = strconv.AppendInt(b, int64(len(key)), 10)
	b = append(b, ':')
	b = append(b, key...)
	b = strconv.AppendInt(b, int64(len(value)), 10)
	b = append(b, ':')
	b = append(b, value...)
	// hash.Hash's Write never returns an error, per its documented contract.
	_, _ = p.h.Write(b)
}

func (p preimage) str(key, value string) { p.write(key, value) }

func (p preimage) num(key string, value int) { p.write(key, strconv.Itoa(value)) }

func (p preimage) num64(key string, value int64) { p.write(key, strconv.FormatInt(value, 10)) }

func (p preimage) bool(key string, value bool) { p.write(key, strconv.FormatBool(value)) }

// float encodes with 'g' and -1 precision: the shortest representation that
// parses back to the same float64, so 0.7 hashes as "0.7" rather than as a
// rounding of it.
func (p preimage) float(key string, value float64) {
	p.write(key, strconv.FormatFloat(value, 'g', -1, 64))
}

// strs encodes a slice as its length followed by each element under an indexed
// key, so a reordering changes the hash. Order is meaningful in every slice
// [Config] holds — the parse routes are tried in order, the tools are presented
// in order — and a set-like encoding would erase that.
func (p preimage) strs(key string, values []string) {
	p.num(key+".len", len(values))
	for i, v := range values {
		p.write(key+"."+strconv.Itoa(i), v)
	}
}

// digest writes the SHA-256 of a value rather than the value.
func (p preimage) digest(key, value string) {
	sum := sha256.Sum256([]byte(value))
	p.write(key, hex.EncodeToString(sum[:]))
}
