package model

import (
	"context"
	"fmt"
	"math"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// imagePolicyKey carries the generation's resolved image policy to the tool
// door built from its context (newToolDispatcher snapshots it).
type imagePolicyKey struct{}

// clawImageBoundEnvVars are the route image bounds' escape hatches
// (tool.RouteImagePolicy). They are read like the stream-silence watchdog
// knobs — planted-as-unset (envtrust.Inherited), so a project .env neither
// sets nor breaks them — and cross into the sandbox runner as the host's
// own answer, an explicit empty included (forwardableProviderEnv).
var clawImageBoundEnvVars = tool.ImagePolicyEnvVars

// withRouteImagePolicy resolves the image policy of opts.Model ONCE, at the
// entry of a generation: the request builder reads it from opts, the tool
// door from the returned context. A malformed operator value fails the
// generation, naming the knob.
func withRouteImagePolicy(ctx context.Context, opts *GenerationOptions) (context.Context, error) {
	knobs := imageBoundKnobs()
	policy, err := tool.RouteImagePolicy(opts.Model, func(name string) string { return knobs[name] })
	if err != nil {
		return ctx, fmt.Errorf("model %s: image policy: %w", opts.Model, err)
	}
	opts.images = policy
	return context.WithValue(ctx, imagePolicyKey{}, policy), nil
}

// imageBoundKnobs reads the image bounds' escape hatches the way the
// stream-silence watchdog knobs are read: planted-as-unset, so a value a
// project .env planted neither sets a bound nor breaks a generation. One
// read per name of tool.ImagePolicyEnvVars, spelled here so the sandbox
// inventory sees each.
func imageBoundKnobs() map[string]string {
	return map[string]string{
		tool.EnvImageMaxBase64Bytes:        envtrust.Inherited(tool.EnvImageMaxBase64Bytes),
		tool.EnvImageMaxSide:               envtrust.Inherited(tool.EnvImageMaxSide),
		tool.EnvImageMaxDecodePixels:       envtrust.Inherited(tool.EnvImageMaxDecodePixels),
		tool.EnvRequestImageMaxBytes:       envtrust.Inherited(tool.EnvRequestImageMaxBytes),
		tool.EnvRequestImageMaxCount:       envtrust.Inherited(tool.EnvRequestImageMaxCount),
		tool.EnvRequestManyImagesThreshold: envtrust.Inherited(tool.EnvRequestManyImagesThreshold),
		tool.EnvRequestManyImagesMaxSide:   envtrust.Inherited(tool.EnvRequestManyImagesMaxSide),
	}
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
	// requestImageBatchTooLarge: an image of the last tool run, dropped
	// because that run's images together exceed the budget.
	requestImageBatchTooLarge = "[image omitted from this request: the last tool calls' images together exceed this model route's budget per request (%s, %s) — ask for fewer images at a time]"
	// requestImageNeverFits: an image of the last tool run that does not fit
	// the budget beside the prompt's own images, whatever else is dropped.
	requestImageNeverFits = "[image omitted from this request: it does not fit this model route's budget per request beside the prompt's own images (%s, %s) — calling the tool again will not help]"
	// requestImageManyImages: an image over the side every image of a
	// request carrying many images is held to.
	requestImageManyImages = "[image omitted from this request: a request of more than %d images holds each to %d pixels a side on this model route (%s, %s); the newest images are kept — call the tool again to see this one]"
	// requestImageManyImagesLastRun: an image of the last tool run, dropped
	// because the prompt's images and that run's carry more than the
	// threshold with one over the side.
	requestImageManyImagesLastRun = "[image omitted from this request: with the prompt's images and the last tool calls' other images, the request carries more than %d images, one over %d pixels a side, which this model route refuses (%s, %s) — calling the tool again will not help; ask for fewer images at a time]"
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

// ManyImagesTooLargeError is a request whose prompt alone carries more
// images than the route's many-image threshold, one of them over the side
// such a request holds each image to.
type ManyImagesTooLargeError struct {
	Route            string
	Count, Threshold int
	MaxSide          int
}

func (e *ManyImagesTooLargeError) Error() string {
	return fmt.Sprintf("the prompt carries %d images, over the %d after which model route %q holds each to %d pixels a side, and one is larger: attach fewer or smaller images, or raise %s / %s",
		e.Count, e.Threshold, e.Route, e.MaxSide, tool.EnvRequestManyImagesThreshold, tool.EnvRequestManyImagesMaxSide)
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

// headerSide is the longer side an image's header declares: math.MaxInt for
// a known format whose size cannot be read (an image the route may refuse
// counts as over any side bound), 0 for an unknown format.
func headerSide(src *api.ImageSource) int {
	w, h, format, ok := tool.ImageHeader(src)
	switch {
	case ok:
		return max(w, h)
	case format != "":
		return math.MaxInt
	}
	return 0
}

// toolImageRef locates one image inside a tool_result of the conversation.
type toolImageRef struct {
	msg, block, inner int
	bytes             int
	side              int
	format            string
	lastRun           bool
}

// boundRequestImages returns the conversation one request carries under the
// policy's image bounds. Within them, messages are returned as they are.
// Otherwise tool images are replaced by explicit markers, in the request
// only — the history keeps every image and each request re-applies the
// bounds to it; a replaced reduced capture takes its reduction note along:
//
//   - a tool image over the per-image limits (bytes, held to the
//     per-request budget too, or the longer side read from its header),
//     produced under a route with larger ones;
//   - over the per-request budget (bytes or count), the oldest tool images.
//     The cut is replayed over the history oldest first: when an image
//     takes the request over the budget, the oldest go until it is back
//     under requestBudgetHysteresis of it, so the cut point moves every few
//     turns. An image of the last tool run goes only to get back under the
//     budget itself; its marker says calling again will not help when it
//     cannot fit beside the prompt's images, and asks for fewer at a time
//     otherwise;
//   - over the many-image threshold, the oldest tool images over the
//     many-image side, then — when a prompt image is over it — the oldest
//     tool images, until the request is back at the threshold.
//
// A tool image whose media type contradicts its header goes out under the
// header's. Prompt images are never dropped: when they alone break the
// budget or the many-image rule, the request is refused with a typed error.
func boundRequestImages(messages []api.Message, policy tool.ImagePolicy) ([]api.Message, error) {
	maxB, maxC := policy.MaxRequestImageBytes, policy.MaxRequestImages
	thr, manySide := policy.ManyImagesThreshold, policy.ManyImagesMaxSide
	lastAssistant := -1
	for i, m := range messages {
		if m.Role == "assistant" {
			lastAssistant = i
		}
	}
	promptBytes, promptCount, promptMaxSide := 0, 0, 0
	var refs []toolImageRef
	for i, m := range messages {
		for j, b := range m.Content {
			if b.Type == "image" {
				promptBytes += imageBytes(b)
				promptCount++
				if thr > 0 && manySide > 0 {
					promptMaxSide = max(promptMaxSide, headerSide(b.Source))
				}
			}
			if b.Type != "tool_result" {
				continue
			}
			for k, c := range b.Content {
				if c.Type != "image" {
					continue
				}
				r := toolImageRef{msg: i, block: j, inner: k, bytes: imageBytes(c), lastRun: i > lastAssistant}
				r.side = headerSide(c.Source)
				_, _, r.format, _ = tool.ImageHeader(c.Source)
				refs = append(refs, r)
			}
		}
	}
	if (maxB > 0 && promptBytes > maxB) || (maxC > 0 && promptCount > maxC) {
		return nil, &RequestImagesTooLargeError{Route: policy.Route, Bytes: promptBytes, MaxBytes: maxB, Count: promptCount, MaxCount: maxC}
	}
	if thr > 0 && manySide > 0 && promptCount > thr && promptMaxSide > manySide {
		return nil, &ManyImagesTooLargeError{Route: policy.Route, Count: promptCount, Threshold: thr, MaxSide: manySide}
	}

	budget := requestBudget(maxB, maxC)
	envs := tool.EnvRequestImageMaxBytes + ", " + tool.EnvRequestImageMaxCount
	markers := map[int]string{}
	var window []int
	sum, count := promptBytes, promptCount
	over := func(b, c int, fb, fc float64) bool {
		return (maxB > 0 && float64(b) > fb*float64(maxB)) || (maxC > 0 && float64(c) > fc*float64(maxC))
	}
	perMax, perKnobs := policy.EffectiveMaxImageBytes()
	for idx, r := range refs {
		if perMax > 0 && r.bytes > perMax {
			markers[idx] = fmt.Sprintf(requestImageOverPerImage, fmt.Sprintf("%d bytes of base64, %s", perMax, perKnobs))
			continue
		}
		if side := policy.MaxImageSide; side > 0 && r.side > side {
			markers[idx] = fmt.Sprintf(requestImageOverPerImage, fmt.Sprintf("%d pixels a side, %s", side, tool.EnvImageMaxSide))
			continue
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
			if over(promptBytes+refs[head].bytes, promptCount+1, 1, 1) {
				markers[head] = fmt.Sprintf(requestImageNeverFits, budget, envs)
			} else {
				markers[head] = fmt.Sprintf(requestImageBatchTooLarge, budget, envs)
			}
		}
	}

	if thr > 0 && manySide > 0 && count > thr {
		older := fmt.Sprintf(requestImageManyImages, thr, manySide, tool.EnvRequestManyImagesThreshold, tool.EnvRequestManyImagesMaxSide)
		lastRun := fmt.Sprintf(requestImageManyImagesLastRun, thr, manySide, tool.EnvRequestManyImagesThreshold, tool.EnvRequestManyImagesMaxSide)
		dropped := map[int]bool{}
		overSideKept := func() bool {
			if promptMaxSide > manySide {
				return true
			}
			for _, idx := range window {
				if !dropped[idx] && refs[idx].side > manySide {
					return true
				}
			}
			return false
		}
		drop := func(keep func(idx int) bool, marker string) {
			for _, idx := range window {
				if count <= thr || !overSideKept() {
					return
				}
				if dropped[idx] || keep(idx) {
					continue
				}
				dropped[idx] = true
				markers[idx] = marker
				count--
			}
		}
		// The newest screen the model asked for goes last: older over-side
		// images first, then older images whatever their side, and only then
		// the last run's — over-side first — under a marker that promises no
		// smaller image, which no image tool can produce.
		drop(func(idx int) bool { return refs[idx].lastRun || refs[idx].side <= manySide }, older)
		drop(func(idx int) bool { return refs[idx].lastRun }, older)
		drop(func(idx int) bool { return refs[idx].side <= manySide }, lastRun)
		drop(func(int) bool { return false }, lastRun)
	}

	// The media type a kept image goes out under is its header's.
	fixes := map[int]api.ContentBlock{}
	for idx, r := range refs {
		if _, dropped := markers[idx]; dropped || r.format == "" {
			continue
		}
		c := messages[r.msg].Content[r.block].Content[r.inner]
		if mt := "image/" + r.format; c.Source.MediaType != mt {
			fixed := *c.Source
			fixed.MediaType = mt
			c.Source = &fixed
			fixes[idx] = c
		}
	}
	if len(markers) == 0 && len(fixes) == 0 {
		return messages, nil
	}

	type blockKey struct{ msg, block int }
	byInner := map[blockKey]map[int]int{}
	for idx, r := range refs {
		_, dropped := markers[idx]
		_, fixed := fixes[idx]
		if !dropped && !fixed {
			continue
		}
		k := blockKey{r.msg, r.block}
		if byInner[k] == nil {
			byInner[k] = map[int]int{}
		}
		byInner[k][r.inner] = idx
	}
	out := append([]api.Message(nil), messages...)
	copiedMsg := map[int]bool{}
	for k, inners := range byInner {
		if !copiedMsg[k.msg] {
			out[k.msg].Content = append([]api.ContentBlock(nil), out[k.msg].Content...)
			copiedMsg[k.msg] = true
		}
		orig := messages[k.msg].Content[k.block].Content
		rebuilt := make([]api.ContentBlock, 0, len(orig))
		droppedPrev := false
		for i, c := range orig {
			idx, touched := inners[i]
			switch {
			case touched && markers[idx] != "":
				rebuilt = append(rebuilt, api.ContentBlock{Type: "text", Text: markers[idx]})
				droppedPrev = true
				continue
			case touched:
				rebuilt = append(rebuilt, fixes[idx])
			case droppedPrev && c.Type == "text" && tool.IsReductionNote(c.Text):
				// The note of the image just replaced goes with it.
			default:
				rebuilt = append(rebuilt, c)
			}
			droppedPrev = false
		}
		out[k.msg].Content[k.block].Content = rebuilt
	}
	return out, nil
}
