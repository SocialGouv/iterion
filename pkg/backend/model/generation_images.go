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

// The markers a tool image dropped from a request carries in its place.
const (
	// requestImageOverPerImage: an image over the route's per-image limits,
	// produced under a route with larger ones and replayed on this one.
	requestImageOverPerImage = "[image omitted from this request: it is over this model route's limit per image (%s) — produced under another route; call the tool again to get it within this route's limits]"
	// requestImageOmitted: an older image dropped to keep the request
	// within the route's per-request budget.
	requestImageOmitted = "[image omitted from this request: the images of the conversation exceed this model route's budget per request (%s, %s) — call the tool again to see it]"
	// requestImageNeverFits: an image of the last tool run that does not fit
	// the budget beside the prompt's own images, whatever else is dropped.
	requestImageNeverFits = "[image omitted from this request: it does not fit this model route's budget per request beside the prompt's own images (%s, %s) — calling the tool again will not help]"
)

// requestBudgetHysteresis is the share of the per-request budget a request
// is brought down to once it goes over: the cut point among the tool images
// — the prompt-cache boundary — then moves every few turns, not every one.
const requestBudgetHysteresis = 0.75

// RequestImagesTooLargeError is a request whose prompt images alone — the
// operator's attachments, which no tool can produce again — exceed the
// route's per-request image budget, in bytes or in count.
type RequestImagesTooLargeError struct {
	Route           string
	Bytes, MaxBytes int
	Count, MaxCount int
}

func (e *RequestImagesTooLargeError) Error() string {
	return fmt.Sprintf("the prompt's %d image(s) total %d bytes, over what model route %q accepts per request (%s): attach fewer or smaller images, or raise %s / %s",
		e.Count, e.Bytes, e.Route, requestBudget(e.MaxBytes, e.MaxCount), tool.EnvRequestImageMaxBytes, tool.EnvRequestImageMaxCount)
}

// requestBudget describes a per-request budget, 0 reading as no bound.
func requestBudget(maxBytes, maxCount int) string {
	b, c := "no byte bound", "no count bound"
	if maxBytes > 0 {
		b = fmt.Sprintf("%d bytes", maxBytes)
	}
	if maxCount > 0 {
		c = fmt.Sprintf("%d images", maxCount)
	}
	return b + ", " + c
}

// imageBytes is what an image block weighs in a request body.
func imageBytes(b api.ContentBlock) int {
	if b.Type != "image" || b.Source == nil {
		return 0
	}
	return len(b.Source.Data) + len(b.Source.URL)
}

// toolImageRef locates one image inside a tool_result of the conversation.
type toolImageRef struct {
	msg, block, inner int
	bytes             int
	lastRun           bool
}

// boundRequestImages returns the conversation one request carries under the
// policy's image bounds. Within them, messages are returned as they are.
// Otherwise tool images are replaced by explicit markers, in the request
// only — the history keeps every image and each request re-applies the
// bounds to it:
//
//   - a tool image over the per-image limits (bytes, or the longer side read
//     from its header), produced under a route with larger ones;
//   - over the per-request budget (bytes or count), the oldest tool images.
//     The cut is replayed over the history oldest first: when an image
//     takes the request over the budget, the oldest go until it is back
//     under requestBudgetHysteresis of it, so the cut point moves every few
//     turns. An image of the last tool run goes only when it cannot fit
//     beside the prompt's images, under a marker saying so.
//
// Prompt images are never dropped: when they alone exceed the budget, the
// request is refused with a *RequestImagesTooLargeError.
func boundRequestImages(messages []api.Message, policy tool.ImagePolicy) ([]api.Message, error) {
	maxB, maxC := policy.MaxRequestImageBytes, policy.MaxRequestImages
	lastAssistant := -1
	for i, m := range messages {
		if m.Role == "assistant" {
			lastAssistant = i
		}
	}
	promptBytes, promptCount := 0, 0
	var refs []toolImageRef
	for i, m := range messages {
		for j, b := range m.Content {
			if n := imageBytes(b); b.Type == "image" {
				promptBytes += n
				promptCount++
			}
			if b.Type != "tool_result" {
				continue
			}
			for k, c := range b.Content {
				if c.Type == "image" {
					refs = append(refs, toolImageRef{msg: i, block: j, inner: k, bytes: imageBytes(c), lastRun: i > lastAssistant})
				}
			}
		}
	}
	if (maxB > 0 && promptBytes > maxB) || (maxC > 0 && promptCount > maxC) {
		return nil, &RequestImagesTooLargeError{Route: policy.Route, Bytes: promptBytes, MaxBytes: maxB, Count: promptCount, MaxCount: maxC}
	}

	budget := requestBudget(maxB, maxC)
	envs := tool.EnvRequestImageMaxBytes + ", " + tool.EnvRequestImageMaxCount
	markers := map[int]string{}
	var window []int
	sum, count := promptBytes, promptCount
	over := func(b, c int, fb, fc float64) bool {
		return (maxB > 0 && float64(b) > fb*float64(maxB)) || (maxC > 0 && float64(c) > fc*float64(maxC))
	}
	for idx, r := range refs {
		c := messages[r.msg].Content[r.block].Content[r.inner]
		if per := policy.MaxImageBase64Bytes; per > 0 && r.bytes > per {
			markers[idx] = fmt.Sprintf(requestImageOverPerImage, fmt.Sprintf("%d bytes of base64, %s", per, tool.EnvImageMaxBase64Bytes))
			continue
		}
		if side := policy.MaxImageSide; side > 0 {
			if w, h, _, ok := tool.ImageHeader(c.Source); ok && (w > side || h > side) {
				markers[idx] = fmt.Sprintf(requestImageOverPerImage, fmt.Sprintf("%d pixels a side, %s", side, tool.EnvImageMaxSide))
				continue
			}
		}
		window = append(window, idx)
		sum, count = sum+r.bytes, count+1
		if !over(sum, count, 1, 1) {
			continue
		}
		for len(window) > 0 && !refs[window[0]].lastRun && over(sum, count, requestBudgetHysteresis, requestBudgetHysteresis) {
			head := window[0]
			window = window[1:]
			sum, count = sum-refs[head].bytes, count-1
			markers[head] = fmt.Sprintf(requestImageOmitted, budget, envs)
		}
		for len(window) > 0 && over(sum, count, 1, 1) {
			head := window[0]
			window = window[1:]
			sum, count = sum-refs[head].bytes, count-1
			markers[head] = fmt.Sprintf(requestImageNeverFits, budget, envs)
		}
	}
	if len(markers) == 0 {
		return messages, nil
	}

	out := append([]api.Message(nil), messages...)
	copied := map[[2]int]bool{}
	for idx, text := range markers {
		r := refs[idx]
		if !copied[[2]int{r.msg, -1}] {
			out[r.msg].Content = append([]api.ContentBlock(nil), out[r.msg].Content...)
			copied[[2]int{r.msg, -1}] = true
		}
		if !copied[[2]int{r.msg, r.block}] {
			out[r.msg].Content[r.block].Content = append([]api.ContentBlock(nil), out[r.msg].Content[r.block].Content...)
			copied[[2]int{r.msg, r.block}] = true
		}
		out[r.msg].Content[r.block].Content[r.inner] = api.ContentBlock{Type: "text", Text: text}
	}
	return out, nil
}
