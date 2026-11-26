package model

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
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
//
// The policy's Acceptance is what the route's catalog states about image
// input (routeImageInput), under the operator's tool.EnvImageAcceptance
// override: a route known to refuse images turns a tool image into a
// recoverable tool error and an image of the conversation into a refusal
// or a marker, before any request goes out.
func withRouteImagePolicy(ctx context.Context, opts *GenerationOptions) (context.Context, error) {
	knobs := imageBoundKnobs()
	getKnob := func(name string) string { return knobs[name] }
	policy, err := tool.RouteImagePolicy(opts.Model, getKnob)
	if err != nil {
		return ctx, fmt.Errorf("model %s: image policy: %w", opts.Model, err)
	}
	policy.Acceptance, err = tool.ImageAcceptanceFor(routeImageInput(ctx, opts.Model), getKnob)
	if err != nil {
		return ctx, fmt.Errorf("model %s: image policy: %w", opts.Model, err)
	}
	opts.images = policy
	return context.WithValue(ctx, imagePolicyKey{}, policy), nil
}

// anthropicVisionProviders are the routing prefixes serving Anthropic's
// Messages API, where every Claude model takes images.
var anthropicVisionProviders = map[string]bool{"anthropic": true, "bedrock": true, "vertex": true}

// routeImageInput is what the catalogs state about a route taking images:
// nil when nothing states it, which sends images.
//
//   - A gateway route (openai_compatible/<id>) answers from the gateway's
//     catalog (compatgw.ResolveCatalogQuiet — the host's forwarded record in
//     the sandbox, the run's feed, the operator table, then the models.dev
//     provider the operator named, live then snapshot), keyed by the
//     provider-qualified id: a bare id's publishers disagree (glm-5.3 is
//     text-only at some, takes images at others), and a disagreement must
//     not refuse a working vision route.
//   - A Claude model on an Anthropic route takes images.
//   - Any other native spec answers from the models.dev row of the provider
//     that serves it (modelsDevProviders), the live registry's exact key
//     first, then the embedded snapshot's — never a cross-provider bare
//     index. A spec no models.dev provider is known for states nothing.
//
// Quiet: the dispatch seam already said an unknown gateway id out loud.
func routeImageInput(ctx context.Context, spec string) *bool {
	if route := modelroute.Parse(spec); route.Gateway() {
		getenv := func(name string) string { return envGetenv(ctx, name) }
		return compatgw.ResolveCatalogQuiet(route.Wire, modelspecs.Default(), getenv).Spec.ImageInput
	}
	return nativeImageInput(spec)
}

// nativeImageInput is routeImageInput for a spec that is not a gateway
// route; the capability views (capabilitiesForModel) read it too, so what
// they show is what the policy decides.
func nativeImageInput(spec string) *bool {
	route := modelroute.Parse(spec)
	if anthropicVisionProviders[route.Provider] && strings.Contains(strings.ToLower(route.Wire), "claude") {
		accepted := true
		return &accepted
	}
	providers := modelsDevProviders(spec, route)
	for _, p := range providers {
		if s, ok := modelspecs.Default().LookupExact(p, route.Wire); ok {
			return s.ImageInput
		}
	}
	for _, p := range providers {
		if s, ok := modelspecs.SnapshotLookupExact(p, route.Wire); ok {
			return s.ImageInput
		}
	}
	return nil
}

// modelsDevProviders lists, in order, the models.dev provider ids whose rows
// describe the model a native spec reaches. A GLM id on the anthropic wire
// (bare or anthropic/, GLMOnAnthropicWire) is z.ai's: its rows live under
// zai-coding-plan, then zai. The other routing prefixes map to the
// provider id models.dev files them under. foundry is absent: its ids are
// the operator's deployment names, which name no models.dev row. A bare
// spec that is not a GLM id names no provider.
func modelsDevProviders(spec string, route modelroute.Route) []string {
	if GLMOnAnthropicWire(spec) {
		return []string{"zai-coding-plan", "zai"}
	}
	switch route.Provider {
	case "anthropic", "openai", "xai":
		return []string{route.Provider}
	case "moonshot":
		return []string{"moonshotai"}
	case "vertex":
		return []string{"google-vertex"}
	case "bedrock":
		return []string{"amazon-bedrock"}
	}
	return nil
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
		tool.EnvImageAcceptance:            envtrust.Inherited(tool.EnvImageAcceptance),
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
	// requestImageRouteRefused: an image produced under another route and
	// replayed on one that takes no image at all.
	requestImageRouteRefused = "[image omitted from this request: model route %q does not accept images — it was produced under another route; calling the tool again will not help, get what you need another way, as text]"
)

// RequestImagesRefusedError is a request whose prompt carries images — the
// operator's attachments — on a model route known not to accept any: it is
// refused before it goes out, never sent for the provider to reject.
type RequestImagesRefusedError struct {
	Route string
	Count int
}

func (e *RequestImagesRefusedError) Error() string {
	return fmt.Sprintf("the prompt carries %d image(s), and model route %q does not accept images: route the node to a model that takes images, drop the attachments, or set %s=send if the route's catalog is wrong",
		e.Count, e.Route, tool.EnvImageAcceptance)
}

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
//
// On a route known to refuse images (tool.ImagesRefused) no image goes out:
// a prompt image refuses the request, every tool image becomes a marker
// (refuseRouteImages).
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
	if policy.Acceptance == tool.ImagesRefused {
		return refuseRouteImages(messages, refs, promptCount, policy.Route)
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
	return replaceToolImages(messages, refs, markers, fixes), nil
}

// refuseRouteImages is boundRequestImages on a route known to refuse
// images: a prompt image refuses the request, and every tool image of the
// conversation — produced under another route, replayed on this one by a
// fallback — goes out as a marker saying so.
func refuseRouteImages(messages []api.Message, refs []toolImageRef, promptCount int, route string) ([]api.Message, error) {
	if promptCount > 0 {
		return nil, &RequestImagesRefusedError{Route: route, Count: promptCount}
	}
	if len(refs) == 0 {
		return messages, nil
	}
	markers := make(map[int]string, len(refs))
	for idx := range refs {
		markers[idx] = fmt.Sprintf(requestImageRouteRefused, route)
	}
	return replaceToolImages(messages, refs, markers, nil), nil
}

// replaceToolImages returns a copy of messages in which each tool image refs
// indexes is replaced by its marker (a reduction note following a replaced
// image goes with it) or by its fixed block; messages itself is untouched.
func replaceToolImages(messages []api.Message, refs []toolImageRef, markers map[int]string, fixes map[int]api.ContentBlock) []api.Message {
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
	return out
}
