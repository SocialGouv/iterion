package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sdkClient wraps the official MCP go-sdk Client + ClientSession,
// implementing the protocolClient interface used by Manager.
type sdkClient struct {
	cfg  *ServerConfig
	info clientInfo

	// startMu guards started, startErr, startInFlight, session. It is
	// NOT held across the actual start() call — concurrent callers
	// either wait on startInFlight (if a start is already running) or
	// observe the cached result.
	startMu       sync.Mutex
	startInFlight chan struct{} // non-nil + open while a start is running
	started       bool
	startErr      error
	session       *mcp.ClientSession
}

func newSDKClient(cfg *ServerConfig, info clientInfo) *sdkClient {
	return &sdkClient{cfg: cloneServerConfig(cfg), info: info}
}

// protocolVersion20260728 is the first MCP revision that removed the
// "ping" method (SEP-2575). The SDK keeps the constant unexported, so we
// mirror it here; revisions are ISO dates, hence ordered by string compare
// exactly as the SDK does internally.
const protocolVersion20260728 = "2026-07-28"

// Ping probes that the session is alive.
//
// The MCP "ping" method was REMOVED in protocol revision 2026-07-28: a
// server speaking it answers MethodNotFound, and a stateless streamable-HTTP
// server rejects the call at the transport with "missing or invalid _meta
// field io.modelcontextprotocol/protocolVersion" (the SDK client does not
// attach the per-request `_meta` triple to a ping, precisely because the
// method no longer exists there). When the session negotiated that revision
// or later, probe with tools/list instead — a supported RPC that exercises
// the same round-trip, and the very call EnsureServers makes next.
func (c *sdkClient) Ping(ctx context.Context) error {
	if err := c.ensureStarted(ctx); err != nil {
		return err
	}
	if res := c.session.InitializeResult(); res != nil && res.ProtocolVersion >= protocolVersion20260728 {
		_, err := c.session.ListTools(ctx, nil)
		return err
	}
	return c.session.Ping(ctx, nil)
}

func (c *sdkClient) ListTools(ctx context.Context) ([]ToolInfo, error) {
	if err := c.ensureStarted(ctx); err != nil {
		return nil, err
	}
	result, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	tools := make([]ToolInfo, len(result.Tools))
	for i, t := range result.Tools {
		schema, _ := json.Marshal(t.InputSchema)
		tools[i] = ToolInfo{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		}
	}
	return tools, nil
}

func (c *sdkClient) CallTool(ctx context.Context, toolName string, args map[string]any) (*ToolCallResult, error) {
	if err := c.ensureStarted(ctx); err != nil {
		return nil, err
	}
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}
	return sdkResultToToolCallResult(result), nil
}

func (c *sdkClient) ListResources(ctx context.Context) ([]ResourceInfo, error) {
	if err := c.ensureStarted(ctx); err != nil {
		return nil, err
	}
	result, err := c.session.ListResources(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]ResourceInfo, 0, len(result.Resources))
	for _, r := range result.Resources {
		out = append(out, ResourceInfo{
			URI:         r.URI,
			Name:        r.Name,
			Description: r.Description,
			MimeType:    r.MIMEType,
		})
	}
	return out, nil
}

func (c *sdkClient) ReadResource(ctx context.Context, uri string) (ResourceContent, error) {
	if err := c.ensureStarted(ctx); err != nil {
		return ResourceContent{}, err
	}
	result, err := c.session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return ResourceContent{}, err
	}
	rc := ResourceContent{URI: uri}
	for _, item := range result.Contents {
		if item == nil {
			continue
		}
		if rc.URI == "" || rc.URI == uri {
			rc.URI = item.URI
		}
		if item.MIMEType != "" {
			rc.MimeType = item.MIMEType
		}
		if item.Text != "" {
			if rc.Text == "" {
				rc.Text = item.Text
			} else {
				rc.Text = rc.Text + "\n" + item.Text
			}
		}
	}
	return rc, nil
}

func (c *sdkClient) Close() error {
	c.startMu.Lock()
	session := c.session
	c.startMu.Unlock()
	if session == nil {
		return nil
	}
	return session.Close()
}

func (c *sdkClient) ensureStarted(ctx context.Context) error {
	// Concurrent ListTools/CallTool callers must not serialise on a
	// mutex held across slow I/O (HTTP dial / process spawn). We also
	// must not permanently cache a context.DeadlineExceeded from one
	// caller's short-lived ctx (e.g. a 5s HealthCheck Ping) — the next
	// caller with a longer-lived ctx should get a fresh attempt.
	c.startMu.Lock()
	if c.started {
		c.startMu.Unlock()
		return nil
	}
	if c.startInFlight != nil {
		// Another goroutine is starting; wait for it.
		ch := c.startInFlight
		c.startMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		c.startMu.Lock()
		defer c.startMu.Unlock()
		// Rewrap a starter-side context error so the waiter doesn't
		// inherit a foreign deadline / cancellation. Otherwise a short
		// HealthCheck-ctx starter would poison every concurrent
		// CallTool waiter with the starter's DeadlineExceeded, hiding
		// what is actually a peer-induced failure under an error that
		// looks local to the waiter (errors.Is(err, context.Deadline)
		// would return true even though the waiter's own ctx is fine).
		if c.startErr != nil && isContextErr(c.startErr) {
			return fmt.Errorf("mcp: peer start aborted: %w", c.startErr)
		}
		return c.startErr
	}
	if c.startErr != nil && !isContextErr(c.startErr) {
		// Prior permanent failure — don't retry.
		err := c.startErr
		c.startMu.Unlock()
		return err
	}
	// We're the starter. Clear any cached context error so this attempt
	// is observed regardless of outcome.
	c.startErr = nil
	ch := make(chan struct{})
	c.startInFlight = ch
	c.startMu.Unlock()

	err := c.start(ctx)

	c.startMu.Lock()
	c.startErr = err
	if err == nil {
		c.started = true
	}
	c.startInFlight = nil
	c.startMu.Unlock()
	close(ch)
	return err
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (c *sdkClient) start(ctx context.Context) error {
	client := mcp.NewClient(&mcp.Implementation{
		Name:    c.info.Name,
		Version: c.info.Version,
	}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	})

	transport, err := c.buildTransport()
	if err != nil {
		return err
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("mcp: connect to %q: %w", c.cfg.Name, err)
	}
	c.session = session
	return nil
}

func (c *sdkClient) buildTransport() (mcp.Transport, error) {
	switch c.cfg.Transport {
	case TransportStdio:
		cmd := exec.Command(c.cfg.Command, c.cfg.Args...)
		if c.cfg.WorkDir != "" {
			cmd.Dir = c.cfg.WorkDir
		}
		if len(c.cfg.Env) > 0 {
			// Merge user env on top of the inherited environ so a
			// user-supplied override (e.g. PATH, PYTHONPATH) wins
			// instead of being a duplicate appended after the parent's
			// value — execve doesn't dedup and Go's docs leave dup
			// resolution implementation-defined.
			environ := os.Environ()
			merged := make(map[string]string, len(environ)+len(c.cfg.Env))
			for _, kv := range environ {
				if i := strings.IndexByte(kv, '='); i > 0 {
					merged[kv[:i]] = kv[i+1:]
				}
			}
			for k, v := range c.cfg.Env {
				merged[k] = v
			}
			cmd.Env = make([]string, 0, len(merged))
			for k, v := range merged {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		return &mcp.CommandTransport{
			Command:           cmd,
			TerminateDuration: 2 * time.Second,
		}, nil

	case TransportHTTP, TransportSSE:
		t := &mcp.StreamableClientTransport{
			Endpoint: c.cfg.URL,
		}
		if len(c.cfg.Headers) > 0 || c.cfg.AuthFunc != nil {
			t.HTTPClient = &http.Client{
				Timeout: 60 * time.Second,
				Transport: &headerRoundTripper{
					headers:  c.cfg.Headers,
					authFunc: c.cfg.AuthFunc,
				},
			}
		}
		return t, nil

	default:
		return nil, fmt.Errorf("mcp: unsupported transport %q for %s", c.cfg.Transport, c.cfg.Name)
	}
}

// headerRoundTripper injects custom headers into every HTTP request.
// When authFunc is set, it is invoked on every request to obtain a
// fresh "Authorization" header value, overriding any static
// Authorization in headers.
type headerRoundTripper struct {
	headers  map[string]string
	authFunc AuthFunc
	base     http.RoundTripper
}

func (rt *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range rt.headers {
		req.Header.Set(k, v)
	}
	if rt.authFunc != nil {
		token, err := rt.authFunc(req.Context())
		if err != nil {
			return nil, fmt.Errorf("mcp: oauth: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", token)
		}
	}
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// sdkResultToToolCallResult converts go-sdk CallToolResult to our ToolCallResult.
func sdkResultToToolCallResult(r *mcp.CallToolResult) *ToolCallResult {
	if r == nil {
		return nil
	}
	result := &ToolCallResult{
		IsError:           r.IsError,
		StructuredContent: r.StructuredContent,
	}
	for _, c := range r.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			result.Content = append(result.Content, ToolContent{
				Type: "text",
				Text: v.Text,
			})
		default:
			// For non-text content (image, audio, etc.), serialize to JSON
			// so it isn't silently dropped.
			if data, err := json.Marshal(v); err == nil {
				result.Content = append(result.Content, ToolContent{
					Type: "text",
					Text: string(data),
				})
			}
		}
	}
	return result
}
