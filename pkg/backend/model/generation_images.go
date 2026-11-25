package model

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// imagePolicyKey carries the generation's resolved image policy to the tool
// door built from its context (newToolDispatcher snapshots it).
type imagePolicyKey struct{}

// withRouteImagePolicy resolves the image policy of opts.Model ONCE, at the
// entry of a generation: the request builder reads it from opts, the tool
// door from the returned context. The escape hatches are read from the
// carried environment; a malformed one fails the generation.
func withRouteImagePolicy(ctx context.Context, opts *GenerationOptions) (context.Context, error) {
	policy, err := tool.RouteImagePolicy(opts.Model, func(name string) string { return envGetenv(ctx, name) })
	if err != nil {
		return ctx, fmt.Errorf("model %s: image policy: %w", opts.Model, err)
	}
	opts.images = policy
	return context.WithValue(ctx, imagePolicyKey{}, policy), nil
}

// imagePolicyFrom reads the policy withRouteImagePolicy pinned. A context
// without one — a door built outside a generation — gets the zero policy:
// images pass unbounded, as the route's own request then decides.
func imagePolicyFrom(ctx context.Context) tool.ImagePolicy {
	policy, _ := ctx.Value(imagePolicyKey{}).(tool.ImagePolicy)
	return policy
}

// requestImageOmitted replaces a tool image dropped from a request over the
// route's per-request image budget.
const requestImageOmitted = "[image omitted from this request: the images of the conversation exceed this model route's budget of %d bytes per request (%s) — call the tool again to see it]"

// RequestImagesTooLargeError is a request whose prompt images alone — the
// operator's attachments, which no tool can produce again — exceed the
// route's per-request image budget.
type RequestImagesTooLargeError struct {
	Route string
	Bytes int
	Max   int
}

func (e *RequestImagesTooLargeError) Error() string {
	return fmt.Sprintf("the prompt's images total %d bytes, over the %d bytes model route %q accepts per request: attach fewer or smaller images, or raise %s",
		e.Bytes, e.Max, e.Route, tool.EnvRequestImageMaxBytes)
}

// imageBytes is what an image block weighs in a request body.
func imageBytes(b api.ContentBlock) int {
	if b.Type != "image" || b.Source == nil {
		return 0
	}
	return len(b.Source.Data) + len(b.Source.URL)
}

// boundRequestImages returns the conversation one request carries under the
// policy's per-request image budget. Under it, messages are returned as
// they are. Over it, the oldest tool images are replaced by an explicit
// marker — the model can call the tool again — until the request fits; the
// conversation itself is not touched, so the history keeps every image and
// each request re-applies the bound to it. Prompt images are never dropped:
// when they alone exceed the budget, the request is refused with a
// *RequestImagesTooLargeError.
func boundRequestImages(messages []api.Message, policy tool.ImagePolicy) ([]api.Message, error) {
	max := policy.MaxRequestImageBytes
	if max <= 0 {
		return messages, nil
	}
	total, prompt := 0, 0
	for _, m := range messages {
		for _, b := range m.Content {
			n := imageBytes(b)
			prompt += n
			total += n
			for _, inner := range b.Content {
				total += imageBytes(inner)
			}
		}
	}
	if total <= max {
		return messages, nil
	}
	if prompt > max {
		return nil, &RequestImagesTooLargeError{Route: policy.Route, Bytes: prompt, Max: max}
	}
	marker := api.ContentBlock{Type: "text", Text: fmt.Sprintf(requestImageOmitted, max, tool.EnvRequestImageMaxBytes)}
	out := append([]api.Message(nil), messages...)
	for i := 0; i < len(out) && total > max; i++ {
		var content []api.ContentBlock
		for j, b := range out[i].Content {
			if b.Type != "tool_result" || total <= max {
				continue
			}
			var inner []api.ContentBlock
			for k, c := range b.Content {
				n := imageBytes(c)
				if n == 0 || total <= max {
					continue
				}
				if inner == nil {
					inner = append([]api.ContentBlock(nil), b.Content...)
				}
				inner[k] = marker
				total -= n
			}
			if inner == nil {
				continue
			}
			if content == nil {
				content = append([]api.ContentBlock(nil), out[i].Content...)
			}
			b.Content = inner
			content[j] = b
		}
		if content != nil {
			out[i].Content = content
		}
	}
	return out, nil
}
