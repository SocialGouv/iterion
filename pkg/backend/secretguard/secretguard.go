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
//     executes a tool or shell command.
//   - Layer 2 (egress DLP): ContainsSecret gates outbound traffic so a
//     real secret value cannot leave toward a non-approved host, and
//     Materialize performs the placeholder→secret swap at the proxy.
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
type Secret struct {
	Name        string
	Value       string
	Placeholder string
	FilePath    string
	Env         string
	Hosts       []string
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
	matcher            *regexp.Regexp    // alternation of known encodings
	placeholderValue   map[string]string // placeholder → raw value (Materialize)
	filePathByName     map[string]string // secret name → mounted file path
	fileHints          []FileSecretHint
	fileValueByName    map[string]string   // secret name → file plaintext (host materialisation)
	encodingsByName    map[string][]string // secret name → its value encodings (egress DLP)
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
		encodingsByName:    make(map[string][]string),
		cfg:                cfg,
	}
	if cfg.Heuristic {
		g.det = detector.New()
	}

	for _, s := range secrets {
		ph := s.Placeholder
		if ph == "" {
			ph = defaultPlaceholder(s.Name)
		}
		s.Placeholder = ph
		if s.FilePath != "" {
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
		g.placeholderValue[ph] = s.Value
		encs := encodingsOf(s.Value)
		g.encodingsByName[s.Name] = encs
		for _, enc := range encs {
			// First registration wins so a value shared by two names
			// keeps a stable placeholder.
			if _, ok := g.literalPlaceholder[enc]; !ok {
				g.literalPlaceholder[enc] = ph
			}
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
	quoted := make([]string, len(lits))
	for i, lit := range lits {
		quoted[i] = regexp.QuoteMeta(lit)
	}
	g.matcher = regexp.MustCompile(strings.Join(quoted, "|"))
}

// HasKnownSecrets reports whether any known value is registered.
func (g *Guard) HasKnownSecrets() bool {
	return g != nil && g.matcher != nil
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
	if g.matcher != nil && g.cfg.RedactKnown {
		s = g.matcher.ReplaceAllStringFunc(s, func(m string) string {
			if ph, ok := g.literalPlaceholder[m]; ok {
				return ph
			}
			return m
		})
	}
	if g.cfg.Heuristic && g.det != nil {
		s = g.heuristicRedact(s)
		if g.cfg.RecurseDecode {
			s = g.recurseDecode(s, g.cfg.DecodeDepth)
		}
	}
	return s
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
		b.WriteString(string(runes[prev:sp.Start]))
		b.WriteString(g.cfg.Marker)
		prev = sp.End
	}
	b.WriteString(string(runes[prev:]))
	return b.String()
}

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
