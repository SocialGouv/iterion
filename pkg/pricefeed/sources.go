package pricefeed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/secrets"
	httpdial "github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// Source is one source type's adapter: it reads the operator's tariffs
// from wherever they live and answers them as a normalized id → spec
// table. The Nth-variant test lives here — a new operator tariff shape
// is one adapter plus one RegisterSource line; the fold, the stores, the
// reconciler and the catalog never learn its name (ADR-163 §4).
type Source interface {
	// ValidateParams checks the feed's parameters for this source type:
	// what is required, what is read, in operator-readable words.
	ValidateParams(params map[string]string) error
	// Fetch reads the tariffs. The caller bounds the context with the
	// feed's own timeout; the adapter never sleeps, retries or manages
	// one itself.
	Fetch(ctx context.Context, params map[string]string) (map[string]modelspecs.Spec, error)
}

var (
	sourceMu sync.RWMutex
	sources  = map[string]Source{}
)

// RegisterSource registers one source type adapter. Intended for this
// package's own init plus tests; a source type nobody registered is a
// config refusal, never a crash.
func RegisterSource(typ string, s Source) {
	sourceMu.Lock()
	defer sourceMu.Unlock()
	sources[typ] = s
}

// SourceFor answers the registered adapter for a source type.
func SourceFor(typ string) (Source, bool) {
	sourceMu.RLock()
	defer sourceMu.RUnlock()
	s, ok := sources[typ]
	return s, ok
}

// SourceTypes lists the registered source types, sorted — the API and
// the CLI render the vocabulary from the registry, never a hand-written
// list that drifts from it.
func SourceTypes() []string {
	sourceMu.RLock()
	defer sourceMu.RUnlock()
	out := make([]string, 0, len(sources))
	for typ := range sources {
		out = append(out, typ)
	}
	sortStrings(out)
	return out
}

// execWaitDelay bounds how long cmd.Wait tolerates a descendant still
// holding the stdout pipe after the command itself is gone (context
// killed or exited): the fetch fails then, instead of parking the
// reconciler's one sequential pass forever.
const execWaitDelay = 5 * time.Second

// fetchLimit bounds one HTTP fetch of a tariff table: the operator's
// source is a config knob, and a hostile or broken endpoint must not
// stream unbounded into the server's memory.
const fetchLimit = 16 << 20

// httpClient is the adapters' shared client — a package variable so the
// suite can point the fetch adapters at a test server (the modelspecs
// registry's Options.Client discipline, without a per-feed plumbing
// parameter the reconciler has no reason to vary).
//
// The transport is httpdial.SafeTransport over the LLM private-endpoint
// hatch: a feed's URL is tenant-admin configuration fetched SERVER-side,
// so the dial resolves and refuses loopback, private and metadata targets
// exactly as the gateway's own client does — re-validated on every
// connection, redirects included — unless the operator exports the hatch
// (ITERION_LLM_ENDPOINT_ALLOW_PRIVATE=1, the one switch every LLM-endpoint
// reader asks). A proxy is pinned nil for the same reason SafeTransport
// pins it: the guard must be the only voice on where a guarded byte goes.
var httpClient = &http.Client{Transport: feedTransport{}}

// feedTransport picks the guarded or permissive dial per request: the
// hatch is read as the process INHERITED it at request time, so a
// deployment exporting it takes effect without a rebuild order and a
// suite arming it mid-run is honoured.
type feedTransport struct{}

var (
	feedDialStrict  = httpdial.SafeTransport(true)
	feedDialPrivate = httpdial.SafeTransport(false)
)

func (feedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if secrets.LLMEndpointAllowPrivate() {
		return feedDialPrivate.RoundTrip(req)
	}
	return feedDialStrict.RoundTrip(req)
}

func init() {
	RegisterSource(TypeModelsDev, modelsDevSource{})
	RegisterSource(TypeScaleway, modelsDevSource{provider: "scaleway"})
	RegisterSource(TypeLiteLLM, liteLLMSource{})
	RegisterSource(TypeExec, execSource{})
	RegisterSource(TypeDeclared, declaredSource{})
}

// modelsDevSource reads a models.dev-shaped JSON table
// (`{provider: {models: {name: {limit, cost, …}}}}`) — the aggregator's
// own format, at the operator's URL or the public one. `scaleway` is
// the same adapter with the provider qualifier pinned: a Scaleway-fed
// gateway's tariffs ARE the aggregator's `scaleway/*` entries, and a
// preset is configuration, not a second parser.
//
// Keys: the aggregator normalizes to `provider/name`; a gateway serving
// provider-first ids (`scaleway/glm-5.2`, LiteLLM's naming) matches the
// qualified key, a bare-id gateway matches the bare name — both spell
// one entry, so neither attaches another model's numbers.
type modelsDevSource struct{ provider string }

func (s modelsDevSource) ValidateParams(params map[string]string) error {
	if s.provider != "" {
		return nil // the preset carries its own qualifier
	}
	if strings.TrimSpace(params["provider"]) == "" {
		return fmt.Errorf("params: provider is required (the models.dev provider key the gateway's ids live under, e.g. scaleway)")
	}
	return nil
}

func (s modelsDevSource) Fetch(ctx context.Context, params map[string]string) (map[string]modelspecs.Spec, error) {
	provider := s.provider
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(params["provider"]))
	}
	if provider == "" {
		return nil, fmt.Errorf("pricefeed: provider is required for a %s feed", TypeModelsDev)
	}
	url := strings.TrimSpace(params["url"])
	if url == "" {
		url = modelspecs.Source
	}
	body, err := httpGet(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("pricefeed: fetch %s: %w", url, err)
	}
	parsed, err := modelspecs.ParseModelsDev(body)
	if err != nil {
		return nil, fmt.Errorf("pricefeed: parse %s: %w", url, err)
	}
	prefix := provider + "/"
	table := map[string]modelspecs.Spec{}
	for key, spec := range parsed {
		name, ok := strings.CutPrefix(key, prefix)
		if !ok || name == "" {
			continue
		}
		table[key] = spec
		if _, taken := table[name]; !taken {
			table[name] = spec
		}
	}
	if len(table) == 0 {
		return nil, fmt.Errorf("pricefeed: %s carries no entries under %q", url, provider)
	}
	return table, nil
}

// liteLLMSource reads a LiteLLM proxy's model-info listing
// (`{"data": [{"model_name": …, "model_info": {…}}]}`) — the shape a
// LiteLLM gateway publishes its OWN served models and costs through.
// Per-token costs arrive in USD and are scaled to per-mtok here, once.
type liteLLMSource struct{}

func (liteLLMSource) ValidateParams(params map[string]string) error {
	if strings.TrimSpace(params["url"]) == "" {
		return fmt.Errorf("params: url is required (the LiteLLM model-info endpoint, e.g. https://gw.example.com/model/info)")
	}
	return nil
}

func (liteLLMSource) Fetch(ctx context.Context, params map[string]string) (map[string]modelspecs.Spec, error) {
	url := strings.TrimSpace(params["url"])
	body, err := httpGetAuth(ctx, url, params["token"])
	if err != nil {
		return nil, fmt.Errorf("pricefeed: fetch %s: %w", url, err)
	}
	var payload struct {
		Data []struct {
			ModelName string `json:"model_name"`
			ModelInfo *struct {
				MaxTokens         *int64  `json:"max_tokens"`
				MaxOutputTokens   *int64  `json:"max_output_tokens"`
				InputCostPerToken float64 `json:"input_cost_per_token"`
				OutputCostPerTok  float64 `json:"output_cost_per_token"`
			} `json:"model_info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("pricefeed: parse %s: %w", url, err)
	}
	table := map[string]modelspecs.Spec{}
	for _, m := range payload.Data {
		id := NormalizeID(m.ModelName)
		if id == "" || m.ModelInfo == nil {
			continue
		}
		spec := modelspecs.Spec{
			InputCostPerM:  m.ModelInfo.InputCostPerToken * 1e6,
			OutputCostPerM: m.ModelInfo.OutputCostPerTok * 1e6,
		}
		if m.ModelInfo.MaxTokens != nil {
			spec.ContextWindow = int(*m.ModelInfo.MaxTokens)
		}
		if m.ModelInfo.MaxOutputTokens != nil {
			spec.MaxOutputTokens = int(*m.ModelInfo.MaxOutputTokens)
		}
		table[id] = spec
	}
	if len(table) == 0 {
		return nil, fmt.Errorf("pricefeed: %s lists no model_info entries", url)
	}
	return table, nil
}

// execSource runs an operator-supplied command and reads a feed's entry
// JSON on stdout (ParseFeedEntries: the operator table's shape plus
// reasoning_efforts) — the adapter that makes a new operator
// "a small script, never an engine if" (ADR-163 §4). No shell, no
// quoting: `command` is the binary, `args` splits on spaces (POSIX
// simple-word spelling); a script that needs quoting is a script that
// wraps itself.
type execSource struct{}

func (execSource) ValidateParams(params map[string]string) error {
	if strings.TrimSpace(params["command"]) == "" {
		return fmt.Errorf("params: command is required (prints the operator-table JSON on stdout; args splits on spaces, no shell)")
	}
	return nil
}

func (execSource) Fetch(ctx context.Context, params map[string]string) (map[string]modelspecs.Spec, error) {
	command := strings.TrimSpace(params["command"])
	if command == "" {
		return nil, fmt.Errorf("pricefeed: command is required for an %s feed", TypeExec)
	}
	args := []string{}
	if raw := strings.TrimSpace(params["args"]); raw != "" {
		args = strings.Fields(raw)
	}
	// Bounded like the HTTP fetches (fetchLimit), and bounded in TIME by
	// our own deadline, not exec's bookkeeping: round 3 measured that
	// cmd.WaitDelay does NOT close the parent's stdout read when a
	// descendant holds the pipe with its stderr closed (the copier's
	// park wins the select in os/exec's watchCtx) — the read parked past
	// ctx+WaitDelay until the descendant died. So the read runs in its
	// own goroutine and OUR timer closes the pipe: whatever holds it,
	// the fetch fails by the deadline.
	// A ctx with no deadline would leave cmd.Wait unbounded (the floor
	// below bounds only the READ — round 4): synthesize the config's
	// maximum so the whole fetch is bounded whatever the caller passed.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, MaxTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = execWaitDelay
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pricefeed: %s: %w", command, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pricefeed: %s %s: %w", command, strings.Join(args, " "), err)
	}
	type readOutcome struct {
		data []byte
		err  error
	}
	readDone := make(chan readOutcome, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(pipe, fetchLimit+1))
		readDone <- readOutcome{data, err}
	}()
	hardBound := time.NewTimer(hardReadBound(ctx))
	defer hardBound.Stop()
	var read readOutcome
	select {
	case read = <-readDone:
	case <-hardBound.C:
		// A descendant holds the pipe past the deadline. Closing it
		// unblocks the reader; the fetch refuses by name.
		_ = pipe.Close()
		read = <-readDone
		waitErr := cmd.Wait()
		if cause := firstErr(read.err, waitErr); cause != nil {
			return nil, fmt.Errorf("pricefeed: %s stdout did not close within its bound (a descendant holds the pipe): %w", command, cause)
		}
		return nil, fmt.Errorf("pricefeed: %s stdout did not close within its bound (a descendant holds the pipe)", command)
	}
	stdout, readErr := read.data, read.err
	waitErr := cmd.Wait()
	if len(stdout) > fetchLimit {
		return nil, fmt.Errorf("pricefeed: %s stdout exceeds %d bytes — a feed table is bounded", command, fetchLimit)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("pricefeed: %s %s: %w", command, strings.Join(args, " "), waitErr)
	}
	if readErr != nil {
		return nil, fmt.Errorf("pricefeed: %s stdout: %w", command, readErr)
	}
	table, err := ParseFeedEntries(string(stdout), command+" stdout")
	if err != nil {
		return nil, fmt.Errorf("pricefeed: %w", err)
	}
	if len(table) == 0 {
		return nil, fmt.Errorf("pricefeed: %s printed an empty table", command)
	}
	return table, nil
}

// hardReadBound answers the instant the stdout read gives up: the ctx's
// own deadline (the process is killed there) plus the pipe grace. A ctx
// with no deadline gets the config's maximum timeout as its floor — a
// fetch without any bound is the thing round 2 measured parked forever.
func hardReadBound(ctx context.Context) time.Duration {
	grace := execWaitDelay + time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline) + grace; d > 0 {
			return d
		}
		return grace
	}
	return MaxTimeout + grace
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	return httpGetAuth(ctx, url, "")
}

func httpGetAuth(ctx context.Context, url, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, fetchLimit))
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// RefreshIntervalBounds keep a feed's cadence inside what the reconciler
// can honour: a period shorter than the pass tick would spin, one longer
// than the freshness arithmetic's sane range is a misprint.
const (
	MinInterval = time.Minute
	MaxInterval = 30 * 24 * time.Hour
	MinTimeout  = time.Second
	MaxTimeout  = 10 * time.Minute
)

// RedactedParamValue is the marker a member-readable feed view renders
// in place of a credential-shaped param's value (pkg/server
// redactFeedParams).
const RedactedParamValue = "(redacted)"

// ValidateFeed checks one feed config: the name's charset (it lands in
// provenance strings and document ids), a registered source type, no
// param holding RedactedParamValue, that type's own params, and the
// cadence bounds. The timeout must not exceed the interval — a fetch
// allowed to outlive its own period would overlap the next attempt on
// the same feed.
func ValidateFeed(feed FeedConfig) error {
	name := strings.TrimSpace(feed.Name)
	if name == "" {
		return fmt.Errorf("a feed needs a name")
	}
	// A name that is not its own trimmed form passes the charset check on
	// its trimmed self while keying documents on the RAW one — two feeds
	// one state document apart. The raw spelling is refused instead.
	if name != feed.Name {
		return fmt.Errorf("feed name %q: trailing or leading whitespace — write %q", feed.Name, name)
	}
	if len(name) > 64 || !validFeedName(name) {
		return fmt.Errorf("feed name %q: lower-case letters, digits and dashes only, starting with a letter or digit", name)
	}
	src, ok := SourceFor(feed.Type)
	if !ok {
		return fmt.Errorf("feed %q: unknown source type %q (registered: %s)", name, feed.Type, strings.Join(SourceTypes(), ", "))
	}
	// A list read from the view and written back carries the marker where
	// the credential was; stored, it would replace the credential.
	var marked []string
	for k, v := range feed.Params {
		if v == RedactedParamValue {
			marked = append(marked, k)
		}
	}
	if len(marked) > 0 {
		sortStrings(marked)
		return fmt.Errorf("feed %q: params %s hold the view's redaction marker %q, not a value — a list read from the view and written back; restate the stored values", name, strings.Join(marked, ", "), RedactedParamValue)
	}
	if err := src.ValidateParams(feed.Params); err != nil {
		return fmt.Errorf("feed %q: %w", name, err)
	}
	if feed.Interval < D(MinInterval) || feed.Interval > D(MaxInterval) {
		return fmt.Errorf("feed %q: interval must be between %ds and %ds", name, int(MinInterval.Seconds()), int(MaxInterval.Seconds()))
	}
	if feed.Timeout < D(MinTimeout) || feed.Timeout > D(MaxTimeout) {
		return fmt.Errorf("feed %q: timeout must be between %ds and %ds", name, int(MinTimeout.Seconds()), int(MaxTimeout.Seconds()))
	}
	if feed.Timeout > feed.Interval {
		return fmt.Errorf("feed %q: timeout (%ds) cannot exceed the interval (%ds) — a fetch must not outlive its own period", name, int(time.Duration(feed.Timeout).Seconds()), int(time.Duration(feed.Interval).Seconds()))
	}
	return nil
}

// validFeedName accepts `[a-z0-9][a-z0-9-]{0,63}`.
func validFeedName(name string) bool {
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ValidateConfig checks a tenant's whole feed list: every feed valid,
// names unique (they key the state documents and the provenance
// strings).
func ValidateConfig(rec *ConfigRecord) error {
	if rec == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, feed := range rec.Feeds {
		if err := ValidateFeed(feed); err != nil {
			return err
		}
		if seen[feed.Name] {
			return fmt.Errorf("feed %q: declared twice", feed.Name)
		}
		seen[feed.Name] = true
	}
	return nil
}
