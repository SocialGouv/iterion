package alert

import (
	"bytes"
	"context"
	"encoding/json"
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

// Notify implements Sink.
func (w *WebhookSink) Notify(ctx context.Context, a Alert) {
	if w == nil {
		return
	}
	body, err := json.Marshal(webhookPayload{Text: a.WebhookText()})
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("alert webhook: marshal payload: %v", err)
		}
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		// Deliberately omit the URL — it is a secret.
		if w.logger != nil {
			w.logger.Warn("alert webhook: build request failed")
		}
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("alert webhook: delivery failed for %s alert (run %s)", a.Kind, a.RunID)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		if w.logger != nil {
			w.logger.Warn("alert webhook: receiver returned %d for %s alert (run %s)", resp.StatusCode, a.Kind, a.RunID)
		}
	}
}
