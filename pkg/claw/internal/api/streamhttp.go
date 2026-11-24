package api

import (
	"net"
	"net/http"
	"time"
)

// Streaming HTTP timeouts.
//
// These timeouts apply to the connection-establishment / pre-headers stage
// only — they MUST NOT cap the total request duration, because LLM streaming
// responses commonly run for many minutes. Setting http.Client.Timeout
// would kill long streams; instead we bound each stage on the Transport.
//
// The numbers are chosen to fail fast on half-open / non-responsive peers
// (a common cause of goroutine + FD leaks under provider incidents) while
// leaving plenty of headroom for legitimate slow-start streams.
const (
	streamDialTimeout       = 10 * time.Second
	streamTLSTimeout        = 10 * time.Second
	streamResponseHeaderTTL = 60 * time.Second
	streamIdleConnTimeout   = 90 * time.Second
)

// NewStreamingHTTPClient returns an *http.Client tuned for long-lived SSE
// streaming responses. It bounds the connect / TLS / response-header stages
// (so a half-open peer can't pin a goroutine + FD forever) but does NOT
// bound the body read, leaving per-request cancellation to the caller's
// context.
//
// Used by every HTTP-based provider (Anthropic, OpenAI, Vertex, Foundry).
// Bare &http.Client{} would inherit zero timeouts on the default transport
// — which translates into goroutine + FD pressure under provider trouble.
func NewStreamingHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   streamDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   streamTLSTimeout,
		ResponseHeaderTimeout: streamResponseHeaderTTL,
		IdleConnTimeout:       streamIdleConnTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		// Intentionally no Timeout: streaming responses can run for many
		// minutes. Cancellation is up to the caller's context.
	}
}
