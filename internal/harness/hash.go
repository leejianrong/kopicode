package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strconv"

	"github.com/leejianrong/kopicode/internal/anchor"
	"github.com/leejianrong/kopicode/internal/provider"
)

// PreimageVersion identifies the *encoding* below, not the configuration.
//
// It is in the preimage so that a change to how a field is encoded — a field
// reordered, a separator changed, a digest swapped for its plaintext — cannot
// produce a hash that collides with one written by an older binary. Bump it when
// the encoding changes, which is a different event from a configuration value
// changing.
//
// 2, as of KAN-844: [Config.ToolCatalogue] and [Config.AdvertiseNativeTools]
// entered the preimage below, which is what "the encoding changes" means here
// — the preimage now *covers* something it did not before, as distinct from an
// existing field's *value* changing (KAN-843's system prompt landing moved the
// hash without moving this constant, and TestDefaultConfigHashIsStable's own
// comment gives that as the worked contrast). No bench run had happened under
// version 1's hash by the time this landed, so nothing pools incorrectly
// either side of the bump — see the PR that introduced this comment for the
// value the golden hash moved to.
const PreimageVersion = 2

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

	// Names only. The full rendering — schemas and descriptions — is
	// [Config.ToolCatalogue] below; this stays because [Catalogue] (and the
	// repair loop it serves) is keyed by name and a hash that could not tell
	// two configurations naming the same tools in a different order apart
	// would be hiding a real behavioural difference (ToolSet's order is
	// presentation order).
	p.strs("tool_set", c.ToolSet)

	// The wire's tool-definition catalogue, in full: names, schemas and
	// descriptions, which is what ADR-0007 decision 6 asks the tool set's
	// presence in the preimage to cover (KAN-844 is what made a rendering
	// exist to hash — see Config.ToolCatalogue's own doc comment). A
	// description edited without touching a name or a type now moves this
	// hash, which it could not before PreimageVersion 2.
	p.toolCatalogue("tool_catalogue", c.ToolCatalogue)

	// Whether ToolCatalogue reaches the wire at all. Two configurations
	// presenting an identical catalogue that one advertises and the other
	// withholds are different arms — SLICE-1 §3's three extraction routes
	// exist because not every model reliably uses the native one, and whether
	// an arm offers it is exactly the kind of thing a second arm might vary
	// (see the field's own doc comment).
	p.bool("advertise_native_tools", c.AdvertiseNativeTools)

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

// toolCatalogue encodes the wire's tool-definition array field by field, in
// order, the same length-prefixed discipline [strs] uses and for the same
// reason: an indexed key so a reordering (of tools, or of one tool's
// arguments) changes the hash rather than being absorbed by a set-like
// encoding. No field of [provider.ToolDefinition]'s type graph is a map — see
// [provider.ToolParameters]'s own doc comment — so there is nothing here that
// could range one.
func (p preimage) toolCatalogue(key string, defs []provider.ToolDefinition) {
	p.num(key+".len", len(defs))
	for i, d := range defs {
		base := key + "." + strconv.Itoa(i)
		p.str(base+".type", d.Type)
		p.str(base+".name", d.Function.Name)
		p.str(base+".description", d.Function.Description)

		// Parameters.Type is not a field: it is always "object" (see
		// provider.ToolParameters' own doc comment), so there is no value here
		// that could vary and nothing to hash.
		props := d.Function.Parameters.Properties
		p.num(base+".properties.len", len(props))
		for j, prop := range props {
			pbase := base + ".properties." + strconv.Itoa(j)
			p.str(pbase+".name", prop.Name)
			p.str(pbase+".type", prop.Type)
			p.str(pbase+".description", prop.Description)
			p.strs(pbase+".enum", prop.Enum)
		}

		p.strs(base+".required", d.Function.Parameters.Required)
	}
}
