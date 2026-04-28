// Package vertex implements the Google Cloud Vertex AI provider for Anthropic
// models. The wire format is Anthropic-compatible and is exposed under the
// streamRawPredict endpoint, e.g.
//
//	https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/publishers/anthropic/models/{model}:streamRawPredict
//
// Authentication uses Google Application Default Credentials (ADC) — typically
// `gcloud auth application-default login` for local development, or workload
// identity in production.
//
// Required environment variables:
//   - GOOGLE_CLOUD_PROJECT: GCP project ID.
//   - GOOGLE_CLOUD_REGION:  GCP region/location. Defaults to "us-east5" when unset.
//
// Models use Vertex's `@version` suffix convention, e.g. "claude-sonnet-4@20250514".
package vertex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/api/httputil"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	// defaultRegion is used when GOOGLE_CLOUD_REGION is unset. us-east5 has the
	// broadest set of Anthropic models on Vertex at the time of writing.
	defaultRegion = "us-east5"

	// anthropicVertexVersion is the literal value Vertex requires in the body.
	anthropicVertexVersion = "vertex-2023-10-16"

	// vertexScope is the OAuth scope ADC requests for Vertex AI.
	vertexScope = "https://www.googleapis.com/auth/cloud-platform"
)

// Provider implements api.Provider for Google Cloud Vertex AI.
type Provider struct{}

// New returns a new Vertex AI Provider.
func New() *Provider { return &Provider{} }

// Name returns the provider identifier.
func (p *Provider) Name() string { return "vertex" }

// AuthMethod returns the GCP Application Default Credentials auth method.
func (p *Provider) AuthMethod() api.AuthMethod { return api.AuthMethodADC }

// NewClient creates a Vertex AI client using ADC.
//
// It reads GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_REGION from the environment,
// returning a clear error when the project ID is missing. Region defaults to
// us-east5.
func (p *Provider) NewClient(cfg api.ProviderConfig) (api.APIClient, error) {
	project := strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	if project == "" {
		return nil, fmt.Errorf("vertex provider: GOOGLE_CLOUD_PROJECT is not set (run `gcloud config set project <id>` or export the env var)")
	}
	region := strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_REGION"))
	if region == "" {
		region = defaultRegion
	}

	model := MapModelID(cfg.Model)
	if model == "" {
		return nil, fmt.Errorf("vertex provider: cfg.Model is empty")
	}

	tokenSource, err := google.DefaultTokenSource(context.Background(), vertexScope)
	if err != nil {
		return nil, fmt.Errorf("vertex provider: failed to obtain Application Default Credentials: %w (run `gcloud auth application-default login`)", err)
	}

	return &Client{
		Project:     project,
		Region:      region,
		Model:       model,
		MaxTokens:   cfg.MaxTokens,
		TokenSource: tokenSource,
		HTTPClient:  &http.Client{},
		BaseURL:     cfg.BaseURL, // optional override; empty means compute from region
	}, nil
}

// MapModelID converts a canonical Claude model ID to the Vertex AI format.
//
// Vertex uses an "@<version>" suffix instead of "-<version>" before the date
// segment. Example: "claude-sonnet-4-20250514" → "claude-sonnet-4@20250514".
//
// The first "-YYYYMMDD" run found in the string (after stripping any "vertex/"
// routing prefix) is rewritten to "@YYYYMMDD". Anything that follows the date —
// for instance a trailing version suffix like "-v1" — is preserved verbatim, so
// "claude-haiku-4-5-20251001-v1" maps to "claude-haiku-4-5@20251001-v1".
//
// Inputs that already contain "@" are returned unchanged.
func MapModelID(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	// Strip "vertex/" routing prefix when present.
	if idx := strings.Index(model, "/"); idx >= 0 {
		if model[:idx] == "vertex" {
			model = model[idx+1:]
		}
	}
	if strings.Contains(model, "@") {
		return model
	}
	// Find a "-YYYYMMDD" run anywhere in the string (followed by either end
	// of string or another "-"). The first match wins; trailing suffixes
	// such as "-v1" are preserved.
	for i := 0; i+9 <= len(model); i++ {
		if model[i] != '-' {
			continue
		}
		if i == 0 {
			continue
		}
		// Require 8 digits at positions i+1..i+8.
		allDigits := true
		for k := 1; k <= 8; k++ {
			c := model[i+k]
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if !allDigits {
			continue
		}
		// The 8-digit run must be terminated by end-of-string or '-'
		// (otherwise it's part of a longer numeric run we don't recognize).
		if i+9 != len(model) && model[i+9] != '-' {
			continue
		}
		return model[:i] + "@" + model[i+1:]
	}
	return model
}

// ----- Client ----------------------------------------------------------------

// Client is the Vertex AI HTTP client. It implements api.APIClient.
type Client struct {
	Project     string
	Region      string
	Model       string
	MaxTokens   int
	TokenSource oauth2.TokenSource
	HTTPClient  *http.Client
	// BaseURL optionally overrides the computed
	// "https://{region}-aiplatform.googleapis.com" host (useful for tests).
	BaseURL string
}

// endpoint returns the full streamRawPredict URL for this client.
func (c *Client) endpoint() string {
	host := c.BaseURL
	if host == "" {
		host = fmt.Sprintf("https://%s-aiplatform.googleapis.com", c.Region)
	}
	return fmt.Sprintf(
		"%s/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:streamRawPredict",
		host, c.Project, c.Region, c.Model,
	)
}

// vertexRequest is the JSON body shape Vertex expects. It mirrors the
// Anthropic /v1/messages request but (a) requires anthropic_version and
// (b) does NOT carry the model field — that's encoded in the URL path.
type vertexRequest struct {
	AnthropicVersion string          `json:"anthropic_version"`
	MaxTokens        int             `json:"max_tokens"`
	System           json.RawMessage `json:"system,omitempty"`
	Messages         []api.Message   `json:"messages"`
	Tools            []api.Tool      `json:"tools,omitempty"`
	ToolChoice       *api.ToolChoice `json:"tool_choice,omitempty"`
	Stream           bool            `json:"stream"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	Stop             []string        `json:"stop,omitempty"`
}

func (c *Client) buildRequest(req api.CreateMessageRequest) ([]byte, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = c.MaxTokens
	}
	if maxTokens == 0 {
		maxTokens = 8096
	}

	v := vertexRequest{
		AnthropicVersion: anthropicVertexVersion,
		MaxTokens:        maxTokens,
		Messages:         req.Messages,
		Tools:            req.Tools,
		ToolChoice:       req.ToolChoice,
		Stream:           true,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		FrequencyPenalty: req.FrequencyPenalty,
		PresencePenalty:  req.PresencePenalty,
		Stop:             req.Stop,
	}

	switch {
	case len(req.SystemBlocks) > 0:
		systemJSON, err := json.Marshal(req.SystemBlocks)
		if err != nil {
			return nil, fmt.Errorf("marshal system blocks: %w", err)
		}
		v.System = systemJSON
	case req.System != "":
		systemJSON, err := json.Marshal(req.System)
		if err != nil {
			return nil, fmt.Errorf("marshal system: %w", err)
		}
		v.System = systemJSON
	}

	return json.Marshal(v)
}

// StreamResponse sends a streaming streamRawPredict request to Vertex and
// returns a channel of api.StreamEvent values. The wire format is identical to
// Anthropic's SSE, so we reuse api.SseParser.
func (c *Client) StreamResponse(ctx context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	body, err := c.buildRequest(req)
	if err != nil {
		return nil, fmt.Errorf("vertex: build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vertex: create request: %w", err)
	}

	tok, err := c.TokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("vertex: refresh ADC token: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("vertex: do request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		bodyStr := string(errBody)
		return nil, &api.APIError{
			Provider:   "vertex",
			StatusCode: resp.StatusCode,
			Message:    extractVertexErrorMessage(bodyStr),
			Body:       httputil.TruncateBody(bodyStr, httputil.BodyTruncateForLog),
			Retryable:  api.IsRetryableStatus(resp.StatusCode),
		}
	}

	ch := make(chan api.StreamEvent, 64)
	go c.streamEvents(ctx, resp, ch)
	return ch, nil
}

// streamEvents reuses the Anthropic SSE parser since Vertex returns the same
// event shape.
func (c *Client) streamEvents(ctx context.Context, resp *http.Response, ch chan<- api.StreamEvent) {
	defer close(ch)
	defer resp.Body.Close()

	parser := api.NewSseParser().WithContext("vertex", c.Model)
	buf := make([]byte, 64*1024)

	send := func(ev api.StreamEvent) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			events, parseErr := parser.Push(buf[:n])
			if parseErr != nil {
				send(api.StreamEvent{Type: api.EventError, ErrorMessage: fmt.Sprintf("parse SSE: %v", parseErr)})
				return
			}
			for _, ev := range events {
				if !send(ev) {
					return
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				send(api.StreamEvent{Type: api.EventError, ErrorMessage: fmt.Sprintf("read stream: %v", readErr)})
			}
			break
		}
	}

	events, parseErr := parser.Finish()
	if parseErr != nil {
		send(api.StreamEvent{Type: api.EventError, ErrorMessage: fmt.Sprintf("parse SSE finish: %v", parseErr)})
		return
	}
	for _, ev := range events {
		if !send(ev) {
			return
		}
	}
}

// extractVertexErrorMessage best-effort parses the Google API error envelope
// ({"error":{"code":int,"message":"...","status":"..."}}) and returns just the
// message. Falls back to the truncated raw body.
func extractVertexErrorMessage(body string) string {
	var parsed struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return httputil.TruncateBody(body, httputil.BodyTruncateForMessage)
}
