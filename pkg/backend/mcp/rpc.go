package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sdkClient wraps the official MCP go-sdk Client + ClientSession,
// implementing the protocolClient interface used by Manager.
type sdkClient struct {
	cfg  *ServerConfig
	info clientInfo

	startMu  sync.Mutex
	started  bool
	startErr error
	session  *mcp.ClientSession
}

func newSDKClient(cfg *ServerConfig, info clientInfo) *sdkClient {
	return &sdkClient{cfg: cloneServerConfig(cfg), info: info}
}

func (c *sdkClient) Ping(ctx context.Context) error {
	if err := c.ensureStarted(ctx); err != nil {
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

func (c *sdkClient) CallTool(ctx context.Context, toolName string, args map[string]interface{}) (*ToolCallResult, error) {
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
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.started {
		return c.startErr
	}
	c.startErr = c.start(ctx)
	if c.startErr == nil {
		c.started = true
	}
	return c.startErr
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
			cmd.Env = os.Environ()
			for k, v := range c.cfg.Env {
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
