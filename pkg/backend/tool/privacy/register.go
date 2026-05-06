package privacy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy/detector"
)

// allCategories is the canonical list of categories the v1
// detector covers. The output schema includes a `has_<cat>`
// boolean for every entry, so callers can route on the absence
// of detection without parsing arrays.
var allCategories = []string{"account_number", "email", "phone", "url", "secret"}

// Config wires the privacy tools to a per-process state. All
// fields are required.
type Config struct {
	// StoreDir is the iterion store root. Vaults live at
	// <StoreDir>/runs/<runID>/pii_vault.json.
	StoreDir string

	// Detector performs the regex+heuristic scan. New() builds
	// the v1 rule set; tests can inject a NewWithRules instance.
	Detector *detector.Detector

	// RunIDFromCtx returns the run ID for a given context. The
	// privacy package cannot import pkg/backend/model directly
	// (which would create an import cycle), so callers thread
	// model.RunIDFromContext in here at wiring time.
	RunIDFromCtx func(ctx context.Context) string

	// vaultCache memoizes opened Vault handles per runID so back-to-back
	// privacy_filter / privacy_unfilter calls in the same run reuse the
	// in-memory map instead of re-reading and re-decoding pii_vault.json
	// on every invocation.
	vaultMu    sync.Mutex
	vaultCache map[string]*Vault
}

// vaultFor returns the Vault handle for runID, opening it if not
// already cached. Concurrent calls in the same run wait on a
// single Mutex; calls in different runs touch different keys.
func (c *Config) vaultFor(runID string) (*Vault, error) {
	c.vaultMu.Lock()
	defer c.vaultMu.Unlock()
	if v, ok := c.vaultCache[runID]; ok {
		return v, nil
	}
	v, err := OpenOrCreate(runID, c.StoreDir)
	if err != nil {
		return nil, err
	}
	if c.vaultCache == nil {
		c.vaultCache = make(map[string]*Vault, 1)
	}
	c.vaultCache[runID] = v
	return v, nil
}

// FilterToolName and UnfilterToolName are the registered names of
// the two privacy tools. Exported so the runtime/model layers
// (which apply persistence-aware redaction at the OnToolNodeResult
// and node_finished seams) can match on the same constant rather
// than on raw strings.
const (
	FilterToolName   = "privacy_filter"
	UnfilterToolName = "privacy_unfilter"
)

// EventTextMarker is substituted for the `text` field of these
// tools' input/output before they reach the persisted event
// stream. Exported for the runtime and model packages to use the
// same string.
const EventTextMarker = "<redacted by privacy tool>"

// internal shorthands kept for local readability.
const (
	filterToolName   = FilterToolName
	unfilterToolName = UnfilterToolName
)

const filterDescription = `Detect or redact personally identifiable information (PII) in input text. Categories: account_number, email, phone, url, secret. In redact mode, returns placeholder tokens stable per (run, value, category) and persists the mapping to a per-run vault. In detect mode, returns spans with hashed values for audit. The detector is pure-Go (regex + entropy + Luhn/mod-97).`

const unfilterDescription = `Substitute privacy_filter placeholder tokens back to their original values using the per-run vault. Missing tokens follow the missing_policy: leave (default), error, or remove.`

const filterInputSchema = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "required": ["text"],
  "additionalProperties": false,
  "properties": {
    "text": { "type": "string" },
    "mode": { "type": "string", "enum": ["redact", "detect"], "default": "redact" },
    "categories": {
      "type": "array",
      "items": { "type": "string", "enum": ["account_number", "email", "phone", "url", "secret"] },
      "default": []
    },
    "min_score": { "type": "number", "minimum": 0, "maximum": 1, "default": 0.5 },
    "placeholder_format": {
      "type": "string",
      "default": "{token}",
      "description": "Substitutions: {token} = the full token (PII_ + 8 hex), {category} = UPPER category. Default emits the bare token so privacy_unfilter can round-trip cleanly. Wrap with delimiters at your own risk — they will remain in the unfiltered output."
    }
  }
}`

const unfilterInputSchema = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "required": ["text"],
  "additionalProperties": false,
  "properties": {
    "text": { "type": "string" },
    "missing_policy": { "type": "string", "enum": ["leave", "error", "remove"], "default": "leave" }
  }
}`

// init validates the schema constants at startup so any typo
// surfaces as a panic on package init rather than a silent
// runtime bug. Also asserts that the filter schema's category
// enum matches allCategories — adding a category to one and
// forgetting the other would silently break has_<cat> routing.
func init() {
	for _, raw := range []string{filterInputSchema, unfilterInputSchema} {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			panic(fmt.Sprintf("privacy: invalid schema constant: %v", err))
		}
	}
	for _, c := range allCategories {
		if !strings.Contains(filterInputSchema, `"`+c+`"`) {
			panic(fmt.Sprintf("privacy: filterInputSchema missing category %q from allCategories", c))
		}
	}
}

// registry abstracts the bits of *tool.Registry that the privacy
// package depends on. Defined as an interface so tests can
// substitute an in-memory fake without importing the production
// registry — and so the privacy package's own callers can avoid
// the awkward cross-package coupling.
type registry interface {
	RegisterBuiltin(name, desc string, schema json.RawMessage, exec func(ctx context.Context, input json.RawMessage) (string, error)) error
}

// RegisterFilter registers the privacy_filter tool against reg
// using cfg. Returns an error if cfg is incomplete.
func RegisterFilter(reg registry, cfg *Config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	exec := buildFilterExec(cfg)
	return reg.RegisterBuiltin(filterToolName, filterDescription, json.RawMessage(filterInputSchema), exec)
}

// RegisterUnfilter registers the privacy_unfilter tool against reg
// using cfg. Returns an error if cfg is incomplete.
func RegisterUnfilter(reg registry, cfg *Config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	exec := buildUnfilterExec(cfg)
	return reg.RegisterBuiltin(unfilterToolName, unfilterDescription, json.RawMessage(unfilterInputSchema), exec)
}

func validateConfig(cfg *Config) error {
	if cfg == nil {
		return errors.New("privacy: nil config")
	}
	if cfg.StoreDir == "" {
		return errors.New("privacy: Config.StoreDir is required")
	}
	if cfg.Detector == nil {
		return errors.New("privacy: Config.Detector is required")
	}
	if cfg.RunIDFromCtx == nil {
		return errors.New("privacy: Config.RunIDFromCtx is required")
	}
	return nil
}

// ---------------------------------------------------------------------------
// privacy_filter
// ---------------------------------------------------------------------------

type filterInput struct {
	Text              string   `json:"text"`
	Mode              string   `json:"mode"`
	Categories        []string `json:"categories"`
	MinScore          *float64 `json:"min_score"`
	PlaceholderFormat string   `json:"placeholder_format"`
}

type placeholder struct {
	Token    string  `json:"token"`
	Category string  `json:"category"`
	Score    float64 `json:"score"`
	Rule     string  `json:"rule"`
}

type detectSpanOut struct {
	Category  string  `json:"category"`
	Score     float64 `json:"score"`
	Start     int     `json:"start"`
	End       int     `json:"end"`
	Rule      string  `json:"rule"`
	ValueHash string  `json:"value_hash"`
}

func buildFilterExec(cfg *Config) func(ctx context.Context, in json.RawMessage) (string, error) {
	return func(ctx context.Context, in json.RawMessage) (string, error) {
		var args filterInput
		if len(in) > 0 {
			if err := json.Unmarshal(in, &args); err != nil {
				return "", fmt.Errorf("privacy_filter: decode input: %w", err)
			}
		}

		mode := strings.ToLower(strings.TrimSpace(args.Mode))
		if mode == "" {
			mode = "redact"
		}
		if mode != "redact" && mode != "detect" {
			return "", fmt.Errorf("privacy_filter: unknown mode %q (want redact or detect)", mode)
		}

		minScore := 0.5
		if args.MinScore != nil {
			minScore = *args.MinScore
		}
		placeholderFmt := args.PlaceholderFormat
		if placeholderFmt == "" {
			placeholderFmt = "{token}"
		}

		start := time.Now()

		// Empty/whitespace short-circuit — no detector call, no
		// vault open. Returns the empty structural response so
		// downstream consumers can rely on every field being
		// present.
		if detector.IsBlank(args.Text) {
			return marshalFilterEmpty(mode, start), nil
		}

		runID := cfg.RunIDFromCtx(ctx)
		if runID == "" {
			return "", errors.New("privacy_filter: no run ID in context (missing runtime wiring?)")
		}

		spans := cfg.Detector.Scan(args.Text, detector.Options{
			Categories: args.Categories,
			MinScore:   minScore,
		})

		if mode == "detect" {
			return marshalDetect(args.Text, spans, start), nil
		}

		// redact mode
		vault, err := cfg.vaultFor(runID)
		if err != nil {
			return "", fmt.Errorf("privacy_filter: open vault: %w", err)
		}

		redacted, placeholders, err := redactWithVault(args.Text, spans, runID, placeholderFmt, vault)
		if err != nil {
			return "", err
		}

		out := map[string]any{
			"mode":            "redact",
			"redacted":        redacted,
			"placeholders":    placeholders,
			"category_counts": countByCategory(placeholders),
			"engine":          "iterion-privacy-go-v1",
			"elapsed_ms":      time.Since(start).Milliseconds(),
		}
		addHasFlags(out, placeholders)
		buf, err := json.Marshal(out)
		if err != nil {
			return "", fmt.Errorf("privacy_filter: encode result: %w", err)
		}
		return string(buf), nil
	}
}

// redactWithVault rewrites text by substituting each detected
// span with a deterministic placeholder. The runes outside spans
// are copied once into the output buffer; the vault is persisted
// in a single batched save at the end so a redact call with N
// spans incurs one fsync, not N.
//
// Spans are expected to be non-overlapping (the detector merges
// overlaps) and emit rune offsets, so we can walk the source in a
// single forward pass.
func redactWithVault(text string, spans []detector.Span, runID, placeholderFmt string, vault *Vault) (string, []placeholder, error) {
	runes := []rune(text)

	work := make([]detector.Span, 0, len(spans))
	for _, s := range spans {
		if s.Start < 0 || s.End > len(runes) || s.Start >= s.End {
			continue
		}
		work = append(work, s)
	}
	sort.SliceStable(work, func(i, j int) bool { return work[i].Start < work[j].Start })

	var sb strings.Builder
	sb.Grow(len(text))
	cursor := 0
	entries := make([]Entry, 0, len(work))
	placeholders := make([]placeholder, 0, len(work))
	seen := make(map[string]bool, len(work))

	for _, s := range work {
		if s.Start > cursor {
			sb.WriteString(string(runes[cursor:s.Start]))
		}
		raw := string(runes[s.Start:s.End])
		token := makeToken(runID, raw, s.Category)
		sb.WriteString(formatPlaceholder(placeholderFmt, token, s.Category))
		cursor = s.End

		entries = append(entries, Entry{Token: token, Value: raw, Category: s.Category})
		if !seen[token] {
			seen[token] = true
			placeholders = append(placeholders, placeholder{
				Token:    token,
				Category: s.Category,
				Score:    s.Score,
				Rule:     s.Rule,
			})
		}
	}
	if cursor < len(runes) {
		sb.WriteString(string(runes[cursor:]))
	}

	if err := vault.AddBatch(entries); err != nil {
		return "", nil, fmt.Errorf("privacy_filter: vault add: %w", err)
	}

	sort.SliceStable(placeholders, func(i, j int) bool {
		return placeholders[i].Token < placeholders[j].Token
	})
	return sb.String(), placeholders, nil
}

func marshalFilterEmpty(mode string, start time.Time) string {
	out := map[string]any{
		"mode":            mode,
		"category_counts": map[string]int{},
		"engine":          "iterion-privacy-go-v1",
		"elapsed_ms":      time.Since(start).Milliseconds(),
	}
	if mode == "redact" {
		out["redacted"] = ""
		out["placeholders"] = []placeholder{}
	} else {
		out["spans"] = []detectSpanOut{}
	}
	addHasFlags(out, nil)
	buf, _ := json.Marshal(out)
	return string(buf)
}

func marshalDetect(text string, spans []detector.Span, start time.Time) string {
	runes := []rune(text)
	out := make([]detectSpanOut, 0, len(spans))
	placeholders := make([]placeholder, 0, len(spans))
	for _, s := range spans {
		var raw string
		if s.Start >= 0 && s.End <= len(runes) && s.Start < s.End {
			raw = string(runes[s.Start:s.End])
		}
		out = append(out, detectSpanOut{
			Category:  s.Category,
			Score:     s.Score,
			Start:     s.Start,
			End:       s.End,
			Rule:      s.Rule,
			ValueHash: shortHash(raw),
		})
		placeholders = append(placeholders, placeholder{Category: s.Category})
	}
	resp := map[string]any{
		"mode":            "detect",
		"spans":           out,
		"category_counts": countByCategory(placeholders),
		"engine":          "iterion-privacy-go-v1",
		"elapsed_ms":      time.Since(start).Milliseconds(),
	}
	addHasFlags(resp, placeholders)
	buf, _ := json.Marshal(resp)
	return string(buf)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}

// makeToken returns a deterministic 8-hex placeholder token for
// the (runID, value, category) tuple. Determinism is the
// foundation of resume idempotence: re-redacting the same input
// in a resumed run yields the same token, so the existing vault
// entry is reused.
func makeToken(runID, value, category string) string {
	h := sha256.Sum256([]byte(runID + "\x00" + value + "\x00" + category))
	return "PII_" + hex.EncodeToString(h[:4])
}

// formatPlaceholder substitutes {token} and {category} in the
// caller's template.
func formatPlaceholder(tmpl, token, category string) string {
	tmpl = strings.ReplaceAll(tmpl, "{token}", token)
	tmpl = strings.ReplaceAll(tmpl, "{category}", strings.ToUpper(category))
	return tmpl
}

func countByCategory(p []placeholder) map[string]int {
	out := map[string]int{}
	for _, ph := range p {
		out[ph.Category]++
	}
	return out
}

// addHasFlags writes a `has_<cat>` boolean for every category in
// allCategories (always-present contract from the schema).
func addHasFlags(out map[string]any, p []placeholder) {
	seen := map[string]bool{}
	for _, ph := range p {
		seen[ph.Category] = true
	}
	for _, c := range allCategories {
		out["has_"+c] = seen[c]
	}
}

// ---------------------------------------------------------------------------
// privacy_unfilter
// ---------------------------------------------------------------------------

type unfilterInput struct {
	Text          string `json:"text"`
	MissingPolicy string `json:"missing_policy"`
}

var tokenRE = regexp.MustCompile(`PII_[0-9a-f]{8}`)

func buildUnfilterExec(cfg *Config) func(ctx context.Context, in json.RawMessage) (string, error) {
	return func(ctx context.Context, in json.RawMessage) (string, error) {
		var args unfilterInput
		if len(in) > 0 {
			if err := json.Unmarshal(in, &args); err != nil {
				return "", fmt.Errorf("privacy_unfilter: decode input: %w", err)
			}
		}
		policy := strings.ToLower(strings.TrimSpace(args.MissingPolicy))
		if policy == "" {
			policy = "leave"
		}
		switch policy {
		case "leave", "error", "remove":
		default:
			return "", fmt.Errorf("privacy_unfilter: unknown missing_policy %q", policy)
		}

		runID := cfg.RunIDFromCtx(ctx)
		if runID == "" {
			return "", errors.New("privacy_unfilter: no run ID in context (missing runtime wiring?)")
		}

		// Empty text: no-op response.
		if args.Text == "" {
			out := map[string]any{
				"text":        "",
				"substituted": []string{},
				"missing":     []string{},
			}
			buf, _ := json.Marshal(out)
			return string(buf), nil
		}

		vault, err := cfg.vaultFor(runID)
		if err != nil {
			return "", fmt.Errorf("privacy_unfilter: open vault: %w", err)
		}

		var (
			substituted []string
			missing     []string
			seenSub     = map[string]bool{}
			seenMiss    = map[string]bool{}
			anyMissing  bool
		)

		restored := tokenRE.ReplaceAllStringFunc(args.Text, func(token string) string {
			val, _, ok := vault.Get(token)
			if ok {
				if !seenSub[token] {
					seenSub[token] = true
					substituted = append(substituted, token)
				}
				return val
			}
			anyMissing = true
			if !seenMiss[token] {
				seenMiss[token] = true
				missing = append(missing, token)
			}
			switch policy {
			case "remove":
				return ""
			default: // leave + error both keep the token in-text
				return token
			}
		})

		if anyMissing && policy == "error" {
			return "", fmt.Errorf("privacy_unfilter: %d token(s) not in vault (first: %s)", len(missing), missing[0])
		}

		out := map[string]any{
			"text":        restored,
			"substituted": orEmptyStrings(substituted),
			"missing":     orEmptyStrings(missing),
		}
		buf, err := json.Marshal(out)
		if err != nil {
			return "", fmt.Errorf("privacy_unfilter: encode result: %w", err)
		}
		return string(buf), nil
	}
}

func orEmptyStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
