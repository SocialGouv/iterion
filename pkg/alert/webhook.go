package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// WebhookSink POSTs alerts to a generic incoming webhook. The body is
// the Slack/Discord-compatible {"text": ...} shape, which both
// platforms (and most generic receivers) accept unchanged.
//
// The webhook URL is treated as a secret: it is never logged, and error
// messages omit it.
type WebhookSink struct {
	url    string
	client *http.Client
	logger *iterlog.Logger
}

// NewWebhookSink builds a sink targeting url. Returns nil when url is
// empty so callers can unconditionally append the result to a sink list.
func NewWebhookSink(url string, logger *iterlog.Logger) *WebhookSink {
	if url == "" {
		return nil
	}
	return &WebhookSink{
		url: url,
		// The shared SSRF-guarded client: DNS pinned per connection, no
		// auto-redirects (each 3xx hop would re-target an unvalidated
		// host). Non-strict — the URL is operator-pinned deployment
		// config, and an internal (RFC-1918) Mattermost is a legitimate
		// receiver — so the guard closes rebinding + redirect games
		// without breaking private-network operators.
		client: httpdial.SafeClient(false, 15*time.Second),
		logger: logger,
	}
}

type webhookPayload struct {
	Text string `json:"text"`
}

// Notify implements Sink (fire-and-forget shape, kept for the in-process
// Manager). The OpsDispatcher path uses NotifyErr so a failed delivery can
// release its episode claim.
func (w *WebhookSink) Notify(ctx context.Context, a Alert) {
	_ = w.NotifyErr(ctx, a)
}

// NotifyErr implements ErrorReportingSink: a transport failure or a non-2xx
// status (including the 3xx the SSRF-guarded client refuses to follow) is a
// FAILED delivery the caller may retry.
func (w *WebhookSink) NotifyErr(ctx context.Context, a Alert) error {
	if w == nil {
		return nil
	}
	body, err := json.Marshal(webhookPayload{Text: a.WebhookText()})
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("alert webhook: marshal payload: %v", err)
		}
		return fmt.Errorf("alert webhook: marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		// Deliberately omit the URL — it is a secret.
		if w.logger != nil {
			w.logger.Warn("alert webhook: build request failed")
		}
		return fmt.Errorf("alert webhook: build request failed")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("alert webhook: delivery failed for %s alert (run %s)", a.Kind, a.RunID)
		}
		return fmt.Errorf("alert webhook: delivery failed for run %s", a.RunID)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		if w.logger != nil {
			w.logger.Warn("alert webhook: receiver returned %d for %s alert (run %s)", resp.StatusCode, a.Kind, a.RunID)
		}
		return fmt.Errorf("alert webhook: receiver returned %d for run %s", resp.StatusCode, a.RunID)
	}
	return nil
}
