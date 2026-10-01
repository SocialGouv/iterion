// Package secretguard protects secret values from leaking through an
// agent run. It is the shared engine behind iterion's layered secrets
// defence:
//
//   - Layer 0 (redaction): Redact scrubs known secret values — in any
//     encoding — and token-shaped unknowns from every observability
//     sink (events.jsonl, artifacts, run.log, report, the studio/board
//     stream) before they are persisted.
//   - Layer 1 (placeholders): an agent only ever sees a Placeholder;
//     Materialize swaps it for the real value at the moment iterion
//     executes a tool or shell command. Only a materialisable secret's
//     placeholder resolves; a RedactOnly value is scrubbed and never
//     handed back.
//   - Layer 2 (egress DLP): ContainsSecret gates outbound traffic so a
//     real secret value cannot leave toward a non-approved host, and
//     MaterializeForHost performs the placeholder→secret swap at the proxy.
//
// Detection is two-tier. Known secret values (the run's resolved
// credentials plus declared ${secret.X} values) are matched
// DETERMINISTICALLY across all their encodings — this is the reliable
// answer to "also detect base64": we match the base64 form of a secret
// we hold, we do not guess. A heuristic pass (the gitleaks-derived
// detector + a recursive base64/hex decode) then catches UNKNOWN
// token-shaped secrets the agent may have read from a file we never
// registered.
//
// A nil *Guard is a valid no-op guard: every method behaves as if no
// secrets are registered, so callers on the "no credentials" path need
// no special-casing.
package secretguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy/detector"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Secret is one protected value. Value is the plaintext; Placeholder
// is the reversible token the agent sees in its place (defaulted from
// Name when empty). FilePath, when set, means {{secrets.NAME}} renders
// that mounted file path instead of the placeholder; the Value is still
// registered for redaction and egress DLP. Hosts, when set, are the only
// egress destinations the secret may be materialised toward (Layer 2
// scoping); empty means "no host restriction".
//
// RedactOnly marks a value iterion scrubs but never hands back: its
// placeholder materialises nowhere — not in agent tool input, a shell
// command, a script, nor egress traffic. Placeholders are deterministic
// (__ITERION_SECRET_<name>__), so a model that writes one would otherwise
// fetch the value; only a secret the workflow declares for its agents to
// use is materialisable.
type Secret struct {
	Name        string
	Value       string
	Placeholder string
	FilePath    string
	Env         string
	Hosts       []string
	RedactOnly  bool
}

type FileSecretHint struct {
	Name string
	Path string
	Env  string
}

// Config tunes Redact. The zero value is not useful — use
// DefaultConfig and override.
type Config struct {
	// RedactKnown gates the known-value redaction pass in Redact. The
	// ITERION_SECRETS_REDACT=off kill-switch clears this (and Heuristic),
	// disabling sink redaction while leaving Materialize/ResolveSecretRef
	// working so declared-secret placeholders still flow.
	RedactKnown bool
	// Heuristic enables the detector pass over UNKNOWN token shapes in
	// Redact. Known-value redaction is independent of this flag.
	Heuristic bool
	// RecurseDecode enables the recursive base64/hex decode pass that
	// peels one encoding layer off a blob and re-scans it.
	RecurseDecode bool
	// MinLen is the shortest raw secret value that is registered. Very
	// short values would over-redact, so they are skipped.
	MinLen int
	// MinScore drops low-confidence heuristic detections. The
	// score-0.6 generic high-entropy fallback is excluded by the
	// default so legitimate hashes/IDs in tool output survive.
	MinScore float64
	// Marker replaces heuristic (unknown) detections. Non-reversible.
	Marker string
	// DecodeDepth bounds the recursive-decode recursion.
	DecodeDepth int
	// Placeholders enables Layer 1 placeholder rendering for declared
	// secrets: when true, {{secrets.X}} renders the opaque placeholder
	// (materialised at exec); when false (kill-switch), it renders the
	// real value directly. Redaction is unaffected either way.
	Placeholders bool
}

// DefaultConfig returns the production defaults.
func DefaultConfig() Config {
	return Config{
		RedactKnown:   true,
		Heuristic:     true,
		RecurseDecode: true,
		MinLen:        5,
		MinScore:      0.7,
		Marker:        "[redacted]",
		DecodeDepth:   2,
		Placeholders:  true,
	}
}

// PlaceholderForName returns the deterministic placeholder token for a
// secret name — the agent-facing stand-in that Materialize swaps for the
// real value. Exported so the DSL template resolver renders
// {{secrets.NAME}} to the exact token the guard registers.
func PlaceholderForName(name string) string { return defaultPlaceholder(name) }

// Guard is an immutable, concurrency-safe scrubber built once per run.
// The one documented mutation seam is MaterializeHostFiles, which rewrites
// filePathByName / fileHints exactly once per run under a caller-provided
// sync.Once so the rewrite happens strictly before any concurrent Execute
// consults ResolveSecretRef.
type Guard struct {
	secrets            []Secret
	literalPlaceholder map[string]string // every encoding → its placeholder
	matcher            *literalMatcher   // every known encoding, longest first
	placeholderValue   map[string]string // placeholder → raw value (Materialize)
	filePathByName     map[string]string // secret name → mounted file path
	fileHints          []FileSecretHint
	fileValueByName    map[string]string // secret name → file plaintext (host materialisation)
	encodings          [][]string        // per entry of secrets: its value encodings (egress DLP)
	longestLiteral     int               // the longest registered encoding, in bytes
	det                *detector.Detector
	cfg                Config
}

var sanitizeName = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// defaultPlaceholder derives a distinctive, low-entropy, reversible
// token from a secret name.
func defaultPlaceholder(name string) string {
	clean := sanitizeName.ReplaceAllString(name, "_")
	clean = strings.Trim(clean, "_")
	if clean == "" {
		clean = "value"
	}
	return "__ITERION_SECRET_" + clean + "__"
}

// placeholderOf is a secret's placeholder before collision handling: the one
// it names, or the one its name gives it.
func placeholderOf(s Secret) string {
	if s.Placeholder != "" {
		return s.Placeholder
	}
	return defaultPlaceholder(s.Name)
}

// redactOnlyPlaceholder distinguishes the placeholder of a value that
// resolves nowhere from the identically-named one that resolves. It keeps the
// __ITERION_SECRET_…__ shape every downstream scrubber already recognises
// (pkg/errtrack, pkg/knowledge).
func redactOnlyPlaceholder(ph string) string {
	return strings.TrimSuffix(ph, "__") + "_redacted__"
}

// New builds a Guard for the given secrets. Secrets whose value is
// shorter than cfg.MinLen are skipped (they cannot be tainted safely).
// Passing no usable secrets still returns a non-nil Guard that runs the
// heuristic pass (when enabled) but matches no known values.
func New(secrets []Secret, cfg Config) *Guard {
	if cfg.MinLen <= 0 {
		cfg.MinLen = DefaultConfig().MinLen
	}
	if cfg.Marker == "" {
		cfg.Marker = DefaultConfig().Marker
	}
	if cfg.DecodeDepth <= 0 {
		cfg.DecodeDepth = DefaultConfig().DecodeDepth
	}

	g := &Guard{
		literalPlaceholder: make(map[string]string),
		placeholderValue:   make(map[string]string),
		filePathByName:     make(map[string]string),
		cfg:                cfg,
	}
	if cfg.Heuristic {
		g.det = detector.New()
	}

	// A workflow is free to DECLARE a secret under a name a RedactOnly value
	// already carries (`forge_publish_token`, `env_<X>`, `provider_key_<n>`).
	// Their placeholders must stay distinct: "a RedactOnly placeholder
	// resolves nowhere" is the guarantee, and a shared one would hand an
	// agent reading it back the other credential — silently, since both are
	// secrets it is otherwise entitled to.
	resolvable := map[string]bool{}
	// trimmed: the forms of a value without its final newline, registered
	// after every whole value so they never take another secret's.
	type literal struct{ enc, ph string }
	var trimmed []literal
	for _, s := range secrets {
		if !s.RedactOnly && len([]rune(s.Value)) >= cfg.MinLen {
			resolvable[placeholderOf(s)] = true
		}
	}
	for _, s := range secrets {
		ph := placeholderOf(s)
		if s.RedactOnly && resolvable[ph] {
			ph = redactOnlyPlaceholder(ph)
		}
		s.Placeholder = ph
		if s.FilePath != "" && !s.RedactOnly {
			g.filePathByName[s.Name] = s.FilePath
			g.fileHints = append(g.fileHints, FileSecretHint{Name: s.Name, Path: s.FilePath, Env: s.Env})
			// Keep the plaintext for host materialisation, even for values
			// below MinLen (which the redaction/DLP path drops as unsafe to
			// taint). MaterializeHostFiles is the only reader; the exported
			// SecretFileHints never carries it.
			if s.Value != "" {
				if g.fileValueByName == nil {
					g.fileValueByName = make(map[string]string, 1)
				}
				g.fileValueByName[s.Name] = s.Value
			}
		}
		if len([]rune(s.Value)) < cfg.MinLen {
			continue
		}
		g.secrets = append(g.secrets, s)
		if !s.RedactOnly {
			g.placeholderValue[ph] = s.Value
		}
		encs := encodingsOf(s.Value)

		for _, enc := range encs {
			// First registration wins so a value shared by two names
			// keeps a stable placeholder — except that a materialisable
			// secret takes the value over from a RedactOnly one: the
			// placeholder an agent reads back must be one that resolves.
			prev, ok := g.literalPlaceholder[enc]
			_, prevResolves := g.placeholderValue[prev]
			if !ok || (!prevResolves && !s.RedactOnly) {
				g.literalPlaceholder[enc] = ph
			}
		}
		// A file's value ends with its newline; a tool printing it (cat, the
		// CLI trimming a result) drops it. Its placeholder still stands for
		// the whole value: materialised, it carries the newline back.
		if t := strings.TrimRight(s.Value, "\r\n"); t != s.Value && len([]rune(t)) >= cfg.MinLen {
			tencs := []string{t}
			for _, body := range jsonBodies(t) {
				if body != t {
					tencs = append(tencs, body)
				}
			}
			encs = append(encs, tencs...)
			for _, enc := range tencs {
				trimmed = append(trimmed, literal{enc, ph})
			}
		}
		g.encodings = append(g.encodings, encs)
	}
	for _, l := range trimmed {
		if _, ok := g.literalPlaceholder[l.enc]; !ok {
			g.literalPlaceholder[l.enc] = l.ph
		}
	}

	g.buildMatcher()
	return g
}

// buildMatcher compiles a single RE2 alternation over every known
// encoding, ordered longest-first so a longer encoding is preferred
// over a shorter substring at the same position.
func (g *Guard) buildMatcher() {
	if len(g.literalPlaceholder) == 0 {
		return
	}
	lits := make([]string, 0, len(g.literalPlaceholder))
	for lit := range g.literalPlaceholder {
		lits = append(lits, lit)
	}
	sort.Slice(lits, func(i, j int) bool {
		if len(lits[i]) != len(lits[j]) {
			return len(lits[i]) > len(lits[j])
		}
		return lits[i] < lits[j]
	})
	g.longestLiteral = len(lits[0])
	g.matcher = newLiteralMatcher(lits)
}

// literalMatcher matches a set of literals, the longest at the leftmost
// position: a literal of plainLiteralMin bytes or more as a plain string —
// compiled, each of its bytes costs tens of bytes of regexp program for no
// gain — and one holding U+FFFD or an invalid byte too (see
// newLiteralMatcher); the others in as few RE2 alternations as the regexp
// package accepts, and a literal none accepts as a plain string. It never
// panics and never keeps a compile error: the pattern it failed on is the
// secrets.
type literalMatcher struct {
	plain []string         // matched as plain strings
	res   []*regexp.Regexp // leftmost-longest alternations
	// resLits holds each alternation's literals by first byte, longest first.
	resLits []map[byte][]string
}

const plainLiteralMin = 4 << 10

// newLiteralMatcher takes the literals longest first.
func newLiteralMatcher(lits []string) *literalMatcher {
	m := &literalMatcher{}
	for len(lits) > 0 && len(lits[0]) >= plainLiteralMin {
		m.plain = append(m.plain, lits[0])
		lits = lits[1:]
	}
	// A literal holding U+FFFD would match, in a regexp, any invalid byte at
	// that position: a span that is no registered literal, emitted as is,
	// hiding what it covers. Such a literal is matched as a plain string.
	exact := lits[:0:0]
	for _, lit := range lits {
		if strings.ContainsRune(lit, utf8.RuneError) {
			m.plain = append(m.plain, lit)
			continue
		}
		exact = append(exact, lit)
	}
	lits = exact
	for len(lits) > 0 {
		n := len(lits)
		for m.addAlternation(lits[:n]) != nil {
			if n == 1 {
				m.plain = append(m.plain, lits[0])
				break
			}
			n = (n + 1) / 2
		}
		lits = lits[n:]
	}
	return m
}

// addAlternation adds lits, longest first, as one alternation — with its
// literals by first byte, longest first — or returns why they do not compile.
func (m *literalMatcher) addAlternation(lits []string) error {
	re, err := compileAlternation(lits)
	if err != nil {
		return err
	}
	byFirst := make(map[byte][]string)
	for _, lit := range lits {
		byFirst[lit[0]] = append(byFirst[lit[0]], lit)
	}
	m.res = append(m.res, re)
	m.resLits = append(m.resLits, byFirst)
	return nil
}

// compileAlternation compiles lits as one leftmost-longest alternation, in
// lexical order: RE2 factors shared prefixes, which a length order defeats.
func compileAlternation(lits []string) (*regexp.Regexp, error) {
	sorted := append([]string(nil), lits...)
	sort.Strings(sorted)
	quoted := make([]string, len(sorted))
	for i, lit := range sorted {
		quoted[i] = regexp.QuoteMeta(lit)
	}
	re, err := regexp.Compile(strings.Join(quoted, "|"))
	if err != nil {
		return nil, err
	}
	re.Longest()
	return re, nil
}

func (m *literalMatcher) MatchString(s string) bool {
	for _, p := range m.plain {
		if strings.Contains(s, p) {
			return true
		}
	}
	for _, re := range m.res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// ReplaceAllStringFunc replaces every match in one pass over s: the matches
// of every literal, the longest at the leftmost position — one inside it is
// covered by it, and one that runs past it is replaced too, from where it
// starts, so no part of either shows. A replacement is never scanned again
// (a placeholder holds its secret's name, which may be another secret's
// value).
func (m *literalMatcher) ReplaceAllStringFunc(s string, f func(string) string) string {
	// Each search resumes a byte past the last match's start, not at its
	// end — a match that starts inside it is found too — or, past a run, at
	// the run's end (add).
	var spans [][3]int // start, covered end, the literal's end
	// add records the match s[a:b] of a literal from lits (by first byte,
	// longest first) and returns where the search resumes. A literal found
	// again before its end repeats by that shift: the span covers every whole
	// shift the text keeps repeating, and the literals that start in its last
	// bytes and run past it are found by prefix — restarting the search at
	// each byte of a run costs the run's length times the literal's.
	add := func(a, b int, lits map[byte][]string, covered *int) int {
		if b <= *covered {
			return a + 1
		}
		lit := s[a:b]
		j := strings.Index(s[a+1:min(len(s), b+len(lit)-1)], lit)
		if j < 0 {
			spans = append(spans, [3]int{a, b, b})
			*covered = b
			return a + 1
		}
		p := j + 1
		e := b
		for e < len(s) && s[e] == s[e-p] {
			e++
		}
		end := b + (e-b)/p*p
		spans = append(spans, [3]int{a, end, b})
		longest := 0
		for _, ls := range lits {
			longest = max(longest, len(ls[0]))
		}
		for q := max(a+1, end-longest+1); q < end; q++ {
			for _, l := range lits[s[q]] {
				if q+len(l) <= end {
					break
				}
				if strings.HasPrefix(s[q:], l) {
					spans = append(spans, [3]int{q, q + len(l), q + len(l)})
					break
				}
			}
		}
		*covered = end
		return end
	}
	for _, p := range m.plain {
		lits := map[byte][]string{p[0]: {p}}
		covered := 0
		for off := 0; off < len(s); {
			i := strings.Index(s[off:], p)
			if i < 0 {
				break
			}
			off = add(off+i, off+i+len(p), lits, &covered)
		}
	}
	for k, re := range m.res {
		covered := 0
		for off := 0; off < len(s); {
			loc := re.FindStringIndex(s[off:])
			if loc == nil {
				break
			}
			off = add(off+loc[0], off+loc[1], m.resLits[k], &covered)
		}
	}
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i][0] != spans[j][0] {
			return spans[i][0] < spans[j][0]
		}
		return spans[i][1] > spans[j][1]
	})
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		if sp[1] <= last {
			continue
		}
		if sp[0] > last {
			b.WriteString(s[last:sp[0]])
		}
		// A span over a repeating run stands for as many of its literal as
		// the run holds, the last one partly.
		ph := f(s[sp[0]:sp[2]])
		for n := (sp[1] - max(sp[0], last) + sp[2] - sp[0] - 1) / (sp[2] - sp[0]); n > 0; n-- {
			b.WriteString(ph)
		}
		last = sp[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// LongestLiteral is the length in bytes of the longest text Unmaterialize
// (and Redact) recognises as one known value: a caller that redacts a window
// of a longer text must read at least that far past what it keeps. 0 on a nil
// Guard or one with no known value.
func (g *Guard) LongestLiteral() int {
	if g == nil {
		return 0
	}
	return g.longestLiteral
}

// HasKnownSecrets reports whether any known value is registered.
func (g *Guard) HasKnownSecrets() bool {
	return g != nil && g.matcher != nil
}

// Materializes reports whether any registered placeholder resolves to a value
// — a secret that is not RedactOnly.
func (g *Guard) Materializes() bool {
	return g != nil && len(g.placeholderValue) > 0
}

// Secrets returns the registered secrets (with defaulted placeholders).
func (g *Guard) Secrets() []Secret {
	if g == nil {
		return nil
	}
	return g.secrets
}

// Redact scrubs s for persistence: known secret values (any encoding)
// become their placeholder, and heuristic token-shaped unknowns become
// the marker. Safe on a nil Guard (returns s unchanged).
func (g *Guard) Redact(s string) string {
	if g == nil || s == "" {
		return s
	}
	if g.cfg.RedactKnown {
		s = g.Unmaterialize(s)
	}
	if g.cfg.Heuristic && g.det != nil {
		s = g.heuristicRedact(s)
		if g.cfg.RecurseDecode {
			s = g.recurseDecode(s, g.cfg.DecodeDepth)
		}
	}
	return s
}

// Unmaterialize maps every known secret value (in any registered encoding)
// back to its placeholder, and nothing else — Materialize's mirror, for text
// iterion carries from the far side of the materialisation boundary into a
// prompt or the run store. Unlike Redact it is not a sink pass: the
// ITERION_SECRETS_REDACT kill switch leaves it on, the agent only ever sees
// placeholders. Safe on a nil Guard.
func (g *Guard) Unmaterialize(s string) string {
	if g == nil || s == "" || g.matcher == nil {
		return s
	}
	return g.matcher.ReplaceAllStringFunc(s, func(m string) string {
		if ph, ok := g.literalPlaceholder[m]; ok {
			return ph
		}
		return m
	})
}

// RedactBytes is a convenience wrapper for []byte sinks.
func (g *Guard) RedactBytes(b []byte) []byte {
	if g == nil || len(b) == 0 {
		return b
	}
	return []byte(g.Redact(string(b)))
}

// RedactValue deep-copies v, scrubbing secret values from every string
// leaf. It handles the concrete shapes structured event/output payloads
// use (nested maps, []interface{}, []map[string]interface{}, []string);
// other types pass through unchanged. The returned value never aliases
// the input's maps/slices, so callers can persist it without mutating
// live data (node outputs feed downstream nodes and the checkpoint).
func (g *Guard) RedactValue(v any) any {
	if g == nil {
		return v
	}
	switch t := v.(type) {
	case string:
		return g.Redact(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = g.RedactValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = g.RedactValue(vv)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(t))
		for i, vv := range t {
			out[i], _ = g.RedactValue(vv).(map[string]any)
		}
		return out
	case []string:
		out := make([]string, len(t))
		for i, vv := range t {
			out[i] = g.Redact(vv)
		}
		return out
	default:
		return v
	}
}

// RedactMap returns a redacted deep copy of m. Nil-safe; never mutates
// the input.
func (g *Guard) RedactMap(m map[string]any) map[string]any {
	if g == nil || m == nil {
		return m
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = g.RedactValue(v)
	}
	return out
}

// heuristicRedact replaces detector-found secret spans with the marker.
// Spans use rune offsets and are non-overlapping, ascending by Start.
func (g *Guard) heuristicRedact(s string) string {
	spans := g.det.Scan(s, detector.Options{
		Categories: []string{"secret"},
		MinScore:   g.cfg.MinScore,
	})
	if len(spans) == 0 {
		return s
	}
	runes := []rune(s)
	var b strings.Builder
	prev := 0
	for _, sp := range spans {
		if sp.Start < prev || sp.End > len(runes) || sp.Start > sp.End {
			continue
		}
		// A placeholder is a secret's safe form, never one: redacted, the
		// reference is lost to whoever reads the text back.
		if placeholderToken.MatchString(strings.TrimRight(string(runes[sp.Start:sp.End]), ".,;:!?)]}\"'")) {
			continue
		}
		b.WriteString(string(runes[prev:sp.Start]))
		b.WriteString(g.cfg.Marker)
		prev = sp.End
	}
	b.WriteString(string(runes[prev:]))
	return b.String()
}

// placeholderToken matches a span made of placeholders only.
var placeholderToken = regexp.MustCompile(`^__ITERION_SECRET_[A-Za-z0-9_]+__$`)

// b64ish matches a run that could be base64/hex-encoded data.
var b64ish = regexp.MustCompile(`[A-Za-z0-9+/_\-]{16,}={0,2}`)

// recurseDecode peels one encoding layer off each blob and re-scans the
// decoded bytes for a token shape; a hit redacts the ORIGINAL blob.
func (g *Guard) recurseDecode(s string, depth int) string {
	if depth <= 0 || g.det == nil {
		return s
	}
	return b64ish.ReplaceAllStringFunc(s, func(tok string) string {
		dec := tryDecode(tok)
		if dec == "" {
			return tok
		}
		spans := g.det.Scan(dec, detector.Options{
			Categories: []string{"secret"},
			MinScore:   g.cfg.MinScore,
		})
		if len(spans) > 0 {
			return g.cfg.Marker
		}
		if depth > 1 && strings.Contains(g.recurseDecode(dec, depth-1), g.cfg.Marker) {
			return g.cfg.Marker
		}
		return tok
	})
}

// ContainsSecret reports whether s contains a KNOWN secret value in any
// encoding. This is the deterministic egress DLP gate (Layer 2) — it
// never fires on heuristics, so blocking is decided only on values we
// are certain about.
func (g *Guard) ContainsSecret(s string) bool {
	if g == nil || g.matcher == nil || s == "" {
		return false
	}
	return g.matcher.MatchString(s)
}

// Materialize replaces every placeholder with its real secret value.
// Used at tool/shell exec (Layer 1) and at the egress proxy (Layer 2).
// Safe on a nil Guard (returns s unchanged).
func (g *Guard) Materialize(s string) string {
	if g == nil || s == "" || len(g.placeholderValue) == 0 {
		return s
	}
	// Placeholders all carry the __ITERION_SECRET_<name>__ shape, so none
	// is a substring of another — iteration order is irrelevant, and
	// ReplaceAll already no-ops when the token is absent.
	for ph, val := range g.placeholderValue {
		s = strings.ReplaceAll(s, ph, val)
	}
	return s
}

// MaterializeShell is Materialize for a POSIX single-quoted shell context. The
// template layer shell-escapes every secret REF by wrapping its placeholder in
// single quotes ('__ITERION_SECRET_X__'), so substituting the RAW value (plain
// Materialize) lets a value containing a single quote break OUT of that quoting
// — shell injection / RCE on the runner, reachable in multi-tenant cloud where
// the principal that SETS a secret-binding value differs from the bot author.
// Escape each value for inside-single-quote use (each single quote becomes
// close-quote + backslash-quote + reopen-quote) so the surrounding quotes stay
// balanced and the value can never be interpreted as shell syntax.
// Use this at every shell/tool-node exec site; the (raw) Materialize is for
// non-shell materialization (e.g. the egress proxy).
func (g *Guard) MaterializeShell(s string) string {
	if g == nil || s == "" || len(g.placeholderValue) == 0 {
		return s
	}
	for ph, val := range g.placeholderValue {
		s = strings.ReplaceAll(s, ph, strings.ReplaceAll(val, "'", `'\''`))
	}
	return s
}

// MaterializeShellEnv is MaterializeShell's env-indirected counterpart: for
// every placeholder that appears in its single-quoted form
// ('__ITERION_SECRET_X__' — the shape the template layer produces for a
// normal {{secrets.X}} ref, see resolveTemplateWith), it swaps the whole
// quoted token for a double-quoted shell variable reference
// ("$__ITERION_SECRET_X__") instead of inlining the raw value, and returns
// the real values in a name->value map the caller must export into the
// CHILD PROCESS's environment (never the current process's, and never a
// value visible to the parent's own os.Environ()).
//
// This keeps the secret out of the exec'd command's own argv: a command
// line is visible to any co-resident local process via `ps`/
// `/proc/<pid>/cmdline` for the entire lifetime of the subprocess, whereas
// envp is only readable via /proc/<pid>/environ by the owning user or
// root — a materially smaller disclosure surface. The placeholder text is
// already a valid POSIX environment-variable name (see
// defaultPlaceholder: `__ITERION_SECRET_<NAME>__`, `[A-Za-z0-9_]` only),
// so it doubles as the variable name — no separate naming scheme needed.
//
// A placeholder that survives outside its single-quoted form (e.g. an
// explicit {{!secrets.X}} raw/bang ref, which intentionally opts out of
// quoting so the author's own shell snippet controls interpretation)
// falls back to plain MaterializeShell's inline substitution for that
// occurrence — env-indirection only replaces the common, quoted case this
// targets, never silently drops a reference. The same fallback applies,
// conservatively, when a placeholder appears BOTH quoted and bare in the
// same command (an unusual mix of {{secrets.X}} and {{!secrets.X}} for
// the same secret): converting only the quoted occurrence would leave a
// blind substitution for the bare one that could corrupt the
// just-inserted "$PLACEHOLDER" token, since the placeholder name is a
// substring of it — so that placeholder is left entirely to the inline
// path instead.
func (g *Guard) MaterializeShellEnv(s string) (string, map[string]string) {
	if g == nil || s == "" || len(g.placeholderValue) == 0 {
		return s, nil
	}
	var env map[string]string
	for ph, val := range g.placeholderValue {
		quoted := "'" + ph + "'"
		quotedCount := strings.Count(s, quoted)
		if quotedCount == 0 || strings.Count(s, ph) > quotedCount {
			continue
		}
		if env == nil {
			env = make(map[string]string, len(g.placeholderValue))
		}
		env[ph] = val
		s = strings.ReplaceAll(s, quoted, `"$`+ph+`"`)
	}
	for ph, val := range g.placeholderValue {
		if _, converted := env[ph]; converted {
			continue
		}
		if strings.Contains(s, ph) {
			s = strings.ReplaceAll(s, ph, strings.ReplaceAll(val, "'", `'\''`))
		}
	}
	return s, env
}

// ResolveSecretRef renders a {{secrets.NAME}} reference. With
// placeholders enabled (default) it returns the opaque placeholder —
// the agent never sees the real value, which Materialize swaps in at
// exec. With the kill-switch off it returns the real value directly.
// Returns "" on a nil guard or an unknown/unregistered name.
func (g *Guard) ResolveSecretRef(name string) string {
	if g == nil {
		return ""
	}
	if path := g.SecretFilePath(name); path != "" {
		return path
	}
	ph := defaultPlaceholder(name)
	if g.cfg.Placeholders {
		return ph
	}
	return g.placeholderValue[ph]
}

// SecretFilePath returns the mounted path for a file secret, or "" for
// value secrets / unknown names.
func (g *Guard) SecretFilePath(name string) string {
	if g == nil || name == "" {
		return ""
	}
	return g.filePathByName[name]
}

func (g *Guard) SecretFileHints() []FileSecretHint {
	if g == nil || len(g.fileHints) == 0 {
		return nil
	}
	out := make([]FileSecretHint, len(g.fileHints))
	copy(out, g.fileHints)
	return out
}

// MaterializeHostFiles writes each file secret's plaintext to
// dir/<sanitized-name> (files 0600) and rewrites the guard so
// ResolveSecretRef + SecretFileHints return the HOST path instead of the
// sandbox mount path. It is the non-sandbox counterpart to the sandbox
// driver's SecretFiles bind-mounts: on a host (non-sandbox) run nothing
// else writes the mount path, so a {{secrets.X.path}} reference would
// otherwise resolve to /run/iterion/secrets/X and fail on read.
//
// File secrets with no resolved value (Optional + unbound) are skipped
// verbatim — the guard keeps the sandbox mount path so the tool sees the
// same "no such file" it would in a sandbox, mirroring
// pkg/runtime/sandbox_secret_files.go's skip.
//
// Concurrency: the caller MUST serialise this method against any concurrent
// ResolveSecretRef / SecretFileHints reader (the executor guards it with a
// sync.Once fired before dispatching any node). The returned cleanup removes
// each written file; callers typically wrap it to also remove `dir`. Nil-safe.
func (g *Guard) MaterializeHostFiles(dir string) (func(), error) {
	if g == nil || len(g.fileHints) == 0 {
		return func() {}, nil
	}
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("secretguard: host materialisation dir is empty")
	}
	var written []string
	cleanup := func() {
		for _, p := range written {
			_ = os.Remove(p)
		}
	}
	for i, h := range g.fileHints {
		val, ok := g.fileValueByName[h.Name]
		if !ok || val == "" {
			continue
		}
		// A file already present at the DECLARED mount path is the cloud
		// runner's own materialisation (materializeFileSecretsNoSandbox),
		// which the runner's mid-run refresh loop keeps LIVE as the store
		// record rotates. Keep the hint pointing there instead of taking a
		// per-run tempdir snapshot: the snapshot freezes the launch-time
		// value, so an agent reading the hinted path after the token's
		// lifetime (a GitHub App installation token lives ~1h; the forge
		// review post is the run's LAST action) would 401 — the live prod
		// failure this closes. Local host runs have nothing at the mount
		// path (creating /run/iterion/secrets needs root) and keep the
		// tempdir path below.
		if h.Path != "" {
			if _, err := os.Stat(h.Path); err == nil {
				g.filePathByName[h.Name] = h.Path
				continue
			}
		}
		hostPath := filepath.Join(dir, secrets.SanitizeFileName(h.Name))
		if err := os.WriteFile(hostPath, []byte(val), 0o600); err != nil {
			cleanup()
			return nil, fmt.Errorf("secretguard: write host secret file %q: %w", h.Name, err)
		}
		written = append(written, hostPath)
		g.fileHints[i].Path = hostPath
		g.filePathByName[h.Name] = hostPath
	}
	return cleanup, nil
}
