// Package notify delivers run-completion webhooks — a generic
// "this run reached a terminal state, here is its final answer"
// callback POSTed to a URL supplied at launch time.
//
// This is the engine-side primitive that lets an external integration
// (a chat adapter, a CI bridge, anything) trigger a run asynchronously
// and be told when it finished without polling. It is deliberately
// neutral: the payload is a plain JSON envelope; the platform-specific
// glue (Slack/Mattermost formatting, thread routing) lives entirely in
// the receiver.
//
// The callback URL is treated as attacker-influenced (it arrives over
// the launch API), so every delivery passes an SSRF guard: only
// http/https, and — unless explicitly opted in — never a loopback,
// link-local, RFC-1918, or cloud-metadata host. Resolution fails closed.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
	"github.com/SocialGouv/iterion/pkg/store"
)

// PayloadVersion is the schema version of CompletionPayload. Bumped on
// any breaking change so receivers can gate on it.
const PayloadVersion = 1

// DefaultAnswerField is the artifact-data key the notifier reads to
// populate CompletionPayload.FinalAnswer when no explicit field is
// configured on the run.
const DefaultAnswerField = "final_answer"

// CompletionPayload is the JSON body POSTed to a run's callback URL
// when the run reaches a terminal state. Stable wire contract — see
// docs/outbound-callbacks.md.
type CompletionPayload struct {
	V             int    `json:"v"`
	RunID         string `json:"run_id"`
	Status        string `json:"status"`
	WorkflowName  string `json:"workflow_name,omitempty"`
	FinalAnswer   string `json:"final_answer,omitempty"`
	FinalAnswerN  string `json:"final_answer_node,omitempty"`
	Error         string `json:"error,omitempty"`
	CallbackToken string `json:"callback_token,omitempty"`
}

// Notifier delivers CompletionPayloads. Construct once and share; it is
// safe for concurrent use. A nil *Notifier is a valid no-op so callers
// can hold one unconditionally.
type Notifier struct {
	client        *http.Client
	logger        *iterlog.Logger
	allowPrivate  bool
	signingSecret string
}

// Option configures a Notifier.
type Option func(*Notifier)

// WithAllowPrivate permits callback URLs that resolve to loopback,
// link-local, RFC-1918, or cloud-metadata addresses. Off by default
// (SSRF guard). Turn it on only for self-hosted deployments where the
// callback receiver genuinely lives on a private network alongside
// iterion.
func WithAllowPrivate(allow bool) Option {
	return func(n *Notifier) { n.allowPrivate = allow }
}

// WithSigningSecret sets the shared secret used to HMAC-sign every
// outbound completion payload (see Sign). When set, each delivery
// carries an X-Iterion-Signature header the receiver verifies to
// authenticate that iterion — not a forger who learned the callback
// URL — sent it. Empty (the default) disables signing: no header is
// added, and a receiver expecting one should reject the request.
func WithSigningSecret(secret string) Option {
	return func(n *Notifier) { n.signingSecret = secret }
}

// New builds a Notifier. timeout bounds each delivery attempt; zero
// applies a 15s default.
func New(logger *iterlog.Logger, timeout time.Duration, opts ...Option) *Notifier {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	n := &Notifier{
		client: &http.Client{Timeout: timeout},
		logger: logger,
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

// FireForRun loads the run, and if it carries a callback URL and has
// reached a terminal-for-notification state, resolves its final answer
// and POSTs a CompletionPayload. Best-effort: every failure is logged,
// none is returned — a webhook delivery must never affect run outcome.
//
// Paused states (paused_waiting_human / paused_operator) are skipped:
// the run is not done, it is waiting. Resume will call FireForRun again
// when the run actually terminates.
func (n *Notifier) FireForRun(ctx context.Context, st store.RunStore, runID string) {
	if n == nil || st == nil {
		return
	}
	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		if n.logger != nil {
			n.logger.Warn("completion webhook: load run %s: %v", runID, err)
		}
		return
	}
	if run.CallbackURL == "" {
		return // no callback requested — the common case
	}
	if !shouldNotify(run.Status) {
		return
	}

	answer, node := n.resolveFinalAnswer(ctx, st, run)
	payload := CompletionPayload{
		V:             PayloadVersion,
		RunID:         run.ID,
		Status:        string(run.Status),
		WorkflowName:  run.WorkflowName,
		FinalAnswer:   answer,
		FinalAnswerN:  node,
		Error:         run.Error,
		CallbackToken: run.CallbackToken,
	}
	n.deliver(ctx, run.CallbackURL, payload)
}

// shouldNotify reports whether a status warrants a completion callback:
// exactly the canonical terminal set (failed_resumable counts as final
// for delivery; the paused states do not).
func shouldNotify(s store.RunStatus) bool {
	return s.IsTerminal()
}

// resolveFinalAnswer extracts the run's user-facing answer string.
//
//   - When the run names CallbackAnswerNode, the latest artifact for
//     that node is read and its DefaultAnswerField returned.
//   - Otherwise every node in Run.ArtifactIndex is probed (latest
//     version) and the first DefaultAnswerField string wins.
//
// Returns ("", "") when no answer artifact is present — a legitimate
// outcome (e.g. a facilitator bot that chose to stay silent), which the
// receiver interprets as "post nothing".
func (n *Notifier) resolveFinalAnswer(ctx context.Context, st store.RunStore, run *store.Run) (answer, node string) {
	if run.CallbackAnswerNode != "" {
		// Pinned: read ONLY the named node. The hard-stop here is
		// deliberate — a caller that named a node wants that node's
		// answer or none. Falling back to scanning other nodes could
		// post an answer the caller never intended (e.g. an internal
		// reasoning artifact), so a named-but-empty node correctly
		// yields silence.
		if s, ok := n.answerFromNode(ctx, st, run.ID, run.CallbackAnswerNode); ok {
			return s, run.CallbackAnswerNode
		}
		return "", ""
	}
	for nodeID := range run.ArtifactIndex {
		if s, ok := n.answerFromNode(ctx, st, run.ID, nodeID); ok {
			return s, nodeID
		}
	}
	return "", ""
}

// answerFromNode reads the node's latest artifact and returns its
// DefaultAnswerField as a string when present and non-empty.
func (n *Notifier) answerFromNode(ctx context.Context, st store.RunStore, runID, nodeID string) (string, bool) {
	art, err := st.LoadLatestArtifact(ctx, runID, nodeID)
	if err != nil || art == nil || art.Data == nil {
		return "", false
	}
	raw, ok := art.Data[DefaultAnswerField]
	if !ok {
		return "", false
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// deliver POSTs the payload after vetting the URL. All paths are
// best-effort with logging; the callback URL itself is never logged
// (it can embed a secret token in its query string).
func (n *Notifier) deliver(ctx context.Context, rawURL string, payload CompletionPayload) {
	u, err := url.Parse(rawURL)
	if err != nil {
		if n.logger != nil {
			n.logger.Warn("completion webhook: rejected callback URL for run %s: %v", payload.RunID, err)
		}
		return
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		if n.logger != nil {
			n.logger.Warn("completion webhook: rejected callback URL for run %s: scheme %q not allowed", payload.RunID, u.Scheme)
		}
		return
	}
	// Resolve + validate the host ONCE and pin the resulting IP into the
	// dialer below, so the address we vetted is exactly the address we
	// connect to. Without this, the http.Client would re-resolve the
	// host independently — a DNS-rebinding window an attacker who
	// controls the callback host's DNS could use to redirect the
	// (token-bearing) delivery at a private/metadata address after it
	// passed validation.
	pinnedIP, err := n.resolveCallbackIP(ctx, u.Hostname())
	if err != nil {
		if n.logger != nil {
			n.logger.Warn("completion webhook: rejected callback URL for run %s: %v", payload.RunID, err)
		}
		return
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	pinnedAddr := net.JoinHostPort(pinnedIP.String(), port)

	body, err := json.Marshal(payload)
	if err != nil {
		if n.logger != nil {
			n.logger.Warn("completion webhook: marshal payload for run %s: %v", payload.RunID, err)
		}
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		if n.logger != nil {
			n.logger.Warn("completion webhook: build request for run %s failed", payload.RunID)
		}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "iterion-completion-webhook/1")
	// Authenticate the payload so the receiver can distinguish a genuine
	// iterion delivery from a forged POST to a known callback URL. The
	// MAC is computed over the exact bytes sent. No-op when no signing
	// secret is configured.
	if sig := Sign(n.signingSecret, body); sig != "" {
		req.Header.Set(SignatureHeader, sig)
	}

	// Per-delivery client: dial only the pinned IP (so the TLS handshake
	// still uses the URL's host for SNI/verification while the TCP target
	// is fixed) and never auto-follow redirects — a 3xx to a fresh host
	// would re-open the rebinding window.
	client := &http.Client{
		Timeout: n.client.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}
				return d.DialContext(ctx, network, pinnedAddr)
			},
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		if n.logger != nil {
			n.logger.Warn("completion webhook: delivery failed for run %s", payload.RunID)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && n.logger != nil {
		n.logger.Warn("completion webhook: receiver returned %d for run %s", resp.StatusCode, payload.RunID)
	}
}

// vetURL enforces the SSRF guard: http/https only and a host present.
// The host/IP validation + resolution is shared with deliver via
// resolveCallbackIP so the address that is validated is exactly the one
// the delivery dials. Kept as a standalone validator for tests.
func (n *Notifier) vetURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed (want http/https)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("missing host")
	}
	_, err = n.resolveCallbackIP(context.Background(), host)
	return err
}

// resolveCallbackIP resolves host to a single validated IP, returned so the
// caller can PIN it into the dialer — closing the DNS-rebinding window that
// exists when validation and the actual request each resolve the host
// independently. Delegates to the shared SSRF guard (pkg/secure/httpdial);
// allowPrivate inverts strict mode (set in local/dev so a callback to a
// loopback receiver is permitted). Resolution fails closed.
func (n *Notifier) resolveCallbackIP(ctx context.Context, host string) (net.IP, error) {
	return httpdial.ResolvePublicHost(ctx, host, !n.allowPrivate)
}
