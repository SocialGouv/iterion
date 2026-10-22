package compatgw

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

// ReasoningCapture collects, for ONE gateway request, the two things an
// OpenAI-compatible gateway streams that claw's chat-completions translator
// does not carry to iterion:
//
//   - the model's reasoning: `choices[].delta.reasoning_content` (LiteLLM,
//     vLLM, DeepSeek, Moonshot) or, when a delta has none, `reasoning`
//     (vLLM ≥ 0.10, OpenRouter). claw's chat-wire Delta decodes `content` and
//     `tool_calls` only, so a model whose answer — or whose whole output
//     budget — went to reasoning reads as an empty turn;
//   - the raw `finish_reason`: claw maps every value but "tool_calls" to
//     end_turn, so a turn cut at max_tokens ("length") reads as a clean stop.
//
// The capture OBSERVES the response body claw reads: the bytes pass through
// unchanged, and a frame the capture cannot decode is skipped, never an
// error. It is safe for concurrent use: claw reads the body on its own
// goroutine, and a caller that abandons a stream may read the capture while
// that goroutine still writes it.
type ReasoningCapture struct {
	mu        sync.Mutex
	reasoning strings.Builder
	truncated bool
	finish    string
}

// maxCapturedReasoning bounds what one capture keeps. A turn's reasoning is
// tens of kilobytes; past the bound the capture stops growing and says so
// (Truncated), and its text no longer ends where the model's did.
const maxCapturedReasoning = 8 << 20

// maxObservedLine bounds one SSE line the observer buffers — claw's own
// scanner cap, so the observer never holds a line claw would refuse. A
// longer line is skipped to its end, unparsed.
const maxObservedLine = 16 << 20

// NewReasoningCapture returns an empty capture.
func NewReasoningCapture() *ReasoningCapture { return &ReasoningCapture{} }

type reasoningCaptureKey struct{}

// WithReasoningCapture returns ctx carrying c: a request a gateway client
// makes under it feeds c. A nil c returns ctx unchanged.
func WithReasoningCapture(ctx context.Context, c *ReasoningCapture) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, reasoningCaptureKey{}, c)
}

func reasoningCaptureFrom(ctx context.Context) *ReasoningCapture {
	c, _ := ctx.Value(reasoningCaptureKey{}).(*ReasoningCapture)
	return c
}

// Reasoning is the reasoning text the gateway streamed, in arrival order.
func (c *ReasoningCapture) Reasoning() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reasoning.String()
}

// Truncated reports whether the reasoning outgrew maxCapturedReasoning, so
// its tail is missing.
func (c *ReasoningCapture) Truncated() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// FinishReason is the last non-empty finish_reason the gateway streamed, as
// the gateway wrote it ("stop", "length", "tool_calls", …); "" when none
// arrived.
func (c *ReasoningCapture) FinishReason() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finish
}

func (c *ReasoningCapture) add(reasoning, finish string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if finish != "" {
		c.finish = finish
	}
	if reasoning == "" || c.truncated {
		return
	}
	if c.reasoning.Len()+len(reasoning) > maxCapturedReasoning {
		c.truncated = true
		return
	}
	c.reasoning.WriteString(reasoning)
}

// observingTransport is the gateway client's transport: next does the
// request; a successful response to a request whose context carries a
// ReasoningCapture is observed on its way to claw.
type observingTransport struct {
	next http.RoundTripper
}

func (t observingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK || resp.Body == nil {
		return resp, err
	}
	if c := reasoningCaptureFrom(req.Context()); c != nil {
		resp.Body = &sseObserver{body: resp.Body, capture: c}
	}
	return resp, nil
}

// CloseIdleConnections forwards to next, so http.Client.CloseIdleConnections
// still reaches the pool behind the wrapper.
func (t observingTransport) CloseIdleConnections() {
	if ci, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		ci.CloseIdleConnections()
	}
}

// sseObserver hands every byte to its reader unchanged and decodes, on the
// side, the `data:` lines of an SSE chat-completions stream.
type sseObserver struct {
	body     io.ReadCloser
	capture  *ReasoningCapture
	line     []byte
	overlong bool
}

func (o *sseObserver) Read(p []byte) (int, error) {
	n, err := o.body.Read(p)
	if n > 0 {
		o.observe(p[:n])
	}
	return n, err
}

func (o *sseObserver) Close() error { return o.body.Close() }

func (o *sseObserver) observe(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			o.buffer(b)
			return
		}
		o.buffer(b[:i])
		if !o.overlong {
			o.frame(o.line)
		}
		o.line = o.line[:0]
		o.overlong = false
		b = b[i+1:]
	}
}

func (o *sseObserver) buffer(b []byte) {
	if o.overlong {
		return
	}
	if len(o.line)+len(b) > maxObservedLine {
		o.overlong = true
		o.line = o.line[:0]
		return
	}
	o.line = append(o.line, b...)
}

// frame decodes one SSE line. The `data:` prefix takes an optional space
// (the SSE spec, and the claw translator, accept both).
func (o *sseObserver) frame(line []byte) {
	line = bytes.TrimRight(line, "\r")
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
			} `json:"delta"`
			FinishReason json.RawMessage `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &chunk) != nil {
		return
	}
	for _, ch := range chunk.Choices {
		// One field per delta: a gateway mid-way through the rename sends
		// the same text under both names.
		text := jsonString(ch.Delta.ReasoningContent)
		if text == "" {
			text = jsonString(ch.Delta.Reasoning)
		}
		o.capture.add(text, jsonString(ch.FinishReason))
	}
}

// jsonString is raw's value when raw is a JSON string, "" otherwise (null,
// absent, or a shape no gateway this code knows sends there).
func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
