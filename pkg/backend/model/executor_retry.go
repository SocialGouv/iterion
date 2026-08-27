package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// ---------------------------------------------------------------------------
// Retry classifiers + the delegate-retry loop.
//
// Carved out of executor.go to keep the file's bulk focused on Execute
// flow control. Lives in the same package so the helpers stay private.
// ---------------------------------------------------------------------------

// isRetryable returns true if err is a transient LLM API error that should be
// retried. Recognises both iterion's local *APIError (used for stream-decoded
// errors) and claw-code-go's *clawapi.APIError (returned by provider HTTP
// clients on non-2xx responses, e.g. 429 / 5xx).
func isRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.IsRetryable
	}
	var clawErr *api.APIError
	if errors.As(err, &clawErr) {
		return clawErr.IsRetryable()
	}
	// Raw transport failures the typed API errors don't cover: a mid-call
	// connection reset / DNS flap / i/o timeout reaches the in-process claw
	// backend as a bare net error, not an *APIError.
	return delegate.IsNetworkError(err)
}

// statusCodeOf extracts the HTTP status code from a recognised API error
// type, or 0 when the error is not an API error.
func statusCodeOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	var clawErr *api.APIError
	if errors.As(err, &clawErr) {
		return clawErr.StatusCode
	}
	return 0
}

// isDelegateRetryable determines whether a backend execution error is transient
// and worth retrying. Only signal-based kills (exit codes 128+, indicating
// OOM, SIGTERM, etc.) and I/O errors are retried. Permanent failures like
// exit 1 (application error), exit 2 (misuse), or exit 127 (command not
// found) are not retried.
//
// Typed-error fast paths come first: an *ErrTransient or *ErrRateLimited
// raised explicitly by a backend bypasses the legacy stderr-string
// heuristics, which are kept as a fallback for backends that haven't
// been migrated yet (and for SDK-internal errors we don't own).
func isDelegateRetryable(err error) bool {
	if err == nil {
		return false
	}
	var transient *delegate.ErrTransient
	if errors.As(err, &transient) {
		return true
	}
	var rateLimited *delegate.ErrRateLimited
	if errors.As(err, &rateLimited) {
		// Except when iterion raised it: re-issuing the call would spend
		// exactly the quota the operator's cap exists to protect, and the
		// provider — still under its own wall — would serve it. The run
		// parks instead and a durable retry brings it back once the window
		// has really reopened (pkg/runner/usage_retry.go). A refusal that
		// came FROM the provider keeps its historical in-place budget.
		return !rateLimited.SelfImposed
	}
	// Transient connectivity failure (DNS / TCP / TLS / upstream 5xx) — a
	// net.Error, a wrapped syscall errno, or a stringified marker bubbled up
	// from a CLI delegate ("fetch failed", "ECONNRESET", "overloaded"). A
	// brief internet outage must not abort a whole multi-node run.
	if delegate.IsNetworkError(err) {
		return true
	}
	msg := err.Error()
	// Subprocess killed by signal (OOM, timeout, etc.).
	if strings.Contains(msg, "signal:") {
		return true
	}
	// Exit status: only retry signal-based exits (128+). Lower exit codes
	// indicate permanent failures that retrying won't fix.
	if strings.Contains(msg, "exit status") {
		code := extractExitCode(msg)
		// exit 128+ means the process was killed by a signal (128+N).
		// These are typically transient (OOM killer, timeout, etc.).
		return code >= 128
	}
	// Process could not start (resource exhaustion).
	if strings.Contains(msg, "failed to start") {
		return true
	}
	// Stdout reading failure (broken pipe, etc.).
	if strings.Contains(msg, "reading stdout") {
		return true
	}
	// claude_code SDK fell silent for too long (we observed sessions
	// hanging in ep_poll without any propagated error). The runSession
	// watchdog aborts and surfaces this — retrying usually picks up
	// where the previous attempt left off because the resumed session
	// gets a fresh subprocess.
	if strings.Contains(msg, "session idle for") {
		return true
	}
	return false
}

// extractExitCode parses an exit code from an error message containing
// "exit status N". Returns -1 if no valid code is found.
func extractExitCode(msg string) int {
	const prefix = "exit status "
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return -1
	}
	rest := msg[idx+len(prefix):]
	// Parse the integer, stopping at first non-digit.
	n := 0
	found := false
	for _, c := range rest {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
			found = true
		} else {
			break
		}
	}
	if !found {
		return -1
	}
	return n
}

// retryReason renders a short human label for the retry log, distinguishing
// a connectivity blip from other transient failures (OOM signal, idle hang)
// so the operator can tell "internet hiccup" from "subprocess died".
func retryReason(err error) string {
	if delegate.IsNetworkError(err) {
		return "network connectivity issue"
	}
	return "transient backend error"
}

// shouldRetryInPlace decides whether a failed delegate call is worth
// re-issuing against the SAME chain element. It is isDelegateRetryable
// with one carve-out: a provider usage WINDOW (the subscription 5h or
// weekly cap) cannot be cured by waiting a few seconds, so when the node
// still has a fallback element left to try, the in-place budget is
// skipped entirely and the chain advances immediately.
//
// Without the carve-out every element pays a full backed-off budget
// inside a window where success is impossible by construction. That is
// not merely slow: the whole chain runs under ONE per-node timeout
// context, so a multi-element chain can hit the node deadline mid-walk
// and surface context.DeadlineExceeded instead of the typed
// *ErrRateLimited that the run-level usage-window retry and the
// credential-pool donor cooldown both key on.
//
// The carve-out applies only when the next route would ACTUALLY take
// this failure — not merely when one exists. A node whose only route
// declares `on: [unavailable]` would otherwise lose its in-place retry
// budget on a usage window AND then stop at the filter, ending up worse
// off than before the chain existed. On the last element, likewise:
// with nowhere better to go, retrying and surfacing the typed error
// upward is exactly right.
func shouldRetryInPlace(err error, fallbackAccepts func(error) bool) bool {
	if delegate.IsUsageWindow(err) && fallbackAccepts != nil && fallbackAccepts(err) {
		return false
	}
	return isDelegateRetryable(err)
}

// retryDelegateLoop retries a backend execution call with exponential backoff.
// The attempt budget is error-adaptive: a network/connectivity failure gets
// the larger transient budget (rides out a multi-second outage), while a
// deterministic-but-retryable error (signal kill, idle hang) keeps the
// standard budget.
//
// This is the no-fallback form: callers with no further chain element
// (the schema-validation retry, direct one-shot dispatch) use it and get
// the historical behaviour unchanged.
func (e *ClawExecutor) retryDelegateLoop(ctx context.Context, nodeID string, backendName string, fn func() (delegate.Result, error)) (delegate.Result, error) {
	return e.retryDelegateLoopChain(ctx, nodeID, backendName, nil, fn)
}

// retryDelegateLoopChain is retryDelegateLoop with knowledge of whether
// the caller has a fallback route that would take THIS failure, which
// lets it skip a budget that cannot succeed (see shouldRetryInPlace).
// A nil predicate means "no route will take it".
func (e *ClawExecutor) retryDelegateLoopChain(ctx context.Context, nodeID string, backendName string, fallbackAccepts func(error) bool, fn func() (delegate.Result, error)) (delegate.Result, error) {
	result, err := fn()
	for attempt := 1; err != nil && shouldRetryInPlace(err, fallbackAccepts); attempt++ {
		maxAttempts := e.retry.effectiveMaxAttempts(err)
		if attempt >= maxAttempts {
			break
		}
		delay := e.retry.backoff(attempt - 1)

		e.logger.Warn("[%s#%d/%s] %s — delegate retry %d/%d after error: %v (backoff %s)",
			nodeID, LoopIterationFromContext(ctx), backendName, retryReason(err), attempt, maxAttempts-1, err, delay.Round(time.Millisecond))

		if e.hooks.OnDelegateRetry != nil {
			e.hooks.OnDelegateRetry(nodeID, DelegateInfo{
				BackendName: backendName,
				Attempt:     attempt,
				Error:       err,
				Delay:       delay,
			})
		}

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return delegate.Result{}, ctx.Err()
		}

		result, err = fn()
	}
	return result, err
}

// ---------------------------------------------------------------------------
// Provider fallback chain.
//
// Generalises the single-node RESCUE_PROVIDER escape hatch into a
// declarative, ordered chain (DSL `provider: "anthropic,zai,openai"`).
// The chain wraps retryDelegateLoop: each provider gets the full retry
// budget; only a hard failure *beyond* that budget falls through to the
// next provider. See docs/adr for the credential-hint-vs-cross-model
// scope decision.
// ---------------------------------------------------------------------------

// providerLabel renders a provider hint for logs, mapping the empty
// "auto" hint to a readable token.
func providerLabel(p string) string {
	if p == "" {
		return "auto"
	}
	return p
}

// stepLabel renders a chain element for logs. A `fallbacks:` element is
// named by its entry name — the whole reason entries are named, so a
// report says `fell through to "api"` rather than an ordinal. A legacy
// `provider:` element has no name and renders as "zai" (inherits the
// node model) or "zai/glm-5.2" (pins its own).
func stepLabel(s chainElement) string {
	if s.Label != "" {
		return s.Label
	}
	if s.Model == "" {
		return providerLabel(s.Provider)
	}
	return providerLabel(s.Provider) + "/" + s.Model
}

// chainLabel renders a whole chain in its source form for the
// exhausted-chain error, e.g. "zai:glm-5.2,anthropic:claude-opus-4-8"
// for a legacy chain or "primary,api,gpt" for a named one.
func chainLabel(chain []chainElement) string {
	parts := make([]string, len(chain))
	for i, s := range chain {
		if s.Label != "" {
			parts[i] = s.Label
			continue
		}
		p := providerLabel(s.Provider)
		if s.Model != "" {
			p += ":" + s.Model
		}
		parts[i] = p
	}
	return strings.Join(parts, ",")
}

// providerFallbackEligible reports whether a backend actually consumes
// the per-node provider hint, and therefore whether walking a
// HINT-ONLY chain is meaningful. Only claude_code honours ProviderHint
// (anthropic ↔ z.ai ↔ Anthropic-compatible facades); claw derives its
// provider from the model-spec prefix and codex ignores the hint
// entirely, so for those a multi-provider chain would re-run an
// identical call and waste a second retry budget. Compile-time C088
// warns the author; collapseHintOnlyChain trims the chain so the run
// never pays for a no-op fall-through.
//
// This says nothing about a chain whose elements pin their OWN backend
// (ADR-087 `fallbacks:`): there, every element is a different call by
// construction, so the collapse must not apply. chainIsHintOnly is the
// discriminator.
func providerFallbackEligible(backendName string) bool {
	return backendName == delegate.BackendClaudeCode
}

// chainIsHintOnly reports whether every element defers to the node's
// resolved backend — i.e. the chain can only swap a credential, never
// re-shape the task. That is exactly the legacy `provider:` chain
// (ADR-004), and exactly the case where re-running an identical call on
// a hint-ignoring backend is pure waste.
// elementIsHintOnly reports whether an element is only a `provider:`
// credential hint — no named backend/label of its own. Shared by
// chainIsHintOnly and collapseHintOnlyChain so the two cannot drift
// (R6df672).
func elementIsHintOnly(el chainElement) bool {
	// A named element came from a `fallbacks:` block, i.e. the author
	// declared a distinct route on purpose. Even one that only varies
	// the model is meaningful on claw, which derives its provider from
	// the model-spec prefix — so a named element is never collapsed away.
	// A skip element is likewise never a credential hint, whatever its
	// name — the AST-JSON path (studio saves) can produce a nameless one.
	return !el.Skip && el.Backend == "" && el.Label == ""
}

// chainIsHintOnly reports whether every element is a pure provider-hint
// (no named `fallbacks:` route). The discriminator for the legacy
// collapse path; collapseHintOnlyChain walks the same predicate.
func chainIsHintOnly(chain []chainElement) bool {
	for _, el := range chain {
		if !elementIsHintOnly(el) {
			return false
		}
	}
	return true
}

// collapseHintOnlyChain trims the leading run of hint-only elements to
// its head when the resolved backend ignores the hint. Callers apply it
// before dispatch, where the node's backend is known.
//
// It collapses the PREFIX rather than refusing whole-chain when a named
// route exists: a node carrying both `provider: "a,b"` and a
// `fallbacks:` block would otherwise re-issue an identical call with a
// second full retry budget on claw/codex — exactly the waste
// providerFallbackEligible and C088 exist to prevent — merely because
// an unrelated route was declared further down.
func collapseHintOnlyChain(chain []chainElement, backendName string) []chainElement {
	if len(chain) < 2 || providerFallbackEligible(backendName) {
		return chain
	}
	prefix := 0
	for _, el := range chain {
		if !elementIsHintOnly(el) {
			break
		}
		prefix++
	}
	if prefix < 2 {
		return chain
	}
	return append(chain[:1:1], chain[prefix:]...)
}

// elementAccepts reports whether a failure of the PREVIOUS element may
// route to this one.
//
// A nil/empty On list accepts everything — that is the legacy
// `provider:` chain, which has always fallen through on any error, and
// changing that would silently regress every shipped chain.
//
// An UNCLASSIFIED failure always routes, even against an explicit
// filter. Refusing it would strand a run on precisely the failures
// iterion failed to describe — sandboxed claw flattens every error to a
// string at the IPC boundary, and kimi/grok have no error channel at
// all — turning a missing classifier into a dead end rather than a
// fall-through.
//
// A SKIP element is the one exception: routing an indescribable failure
// to another backend is a safety net, but CONVERTING it into a success
// is a lie — a CLI exit 1 or a provider 400 would silently become a
// zero-value verdict. A filtered skip therefore accepts only what its
// `on:` names; the author who wants everything writes `on: [any]`
// (which resolves to an empty filter).
func elementAccepts(el chainElement, cat delegate.FallbackCategory) bool {
	if el.Skip && cat == delegate.FallbackUnclassified {
		return len(el.On) == 0
	}
	if len(el.On) == 0 || cat == delegate.FallbackUnclassified {
		return true
	}
	for _, want := range el.On {
		if want == cat {
			return true
		}
	}
	return false
}

// anyElementAccepts reports whether at least one element in the rest of
// the chain would accept a failure of this category. Used by the
// usage-window retry carve-out: skip the in-place budget only when a
// later route can actually take the failure (Re50c7d — `on:` is a
// per-route filter, not a chain terminator).
func anyElementAccepts(rest []chainElement, cat delegate.FallbackCategory) bool {
	for _, el := range rest {
		if elementAccepts(el, cat) {
			return true
		}
	}
	return false
}

// firstAcceptingFrom returns the index of the first element in chain[from:]
// that accepts cat, or -1 if none do.
func firstAcceptingFrom(chain []chainElement, from int, cat delegate.FallbackCategory) int {
	for i := from; i < len(chain); i++ {
		if elementAccepts(chain[i], cat) {
			return i
		}
	}
	return -1
}

// chainSpend accumulates what the FAILED elements of a chain consumed,
// so the winning element's Result can carry the node's true cost.
//
// Dropping it is not a rounding error under ADR-087: with a same-model
// credential swap the discarded work was bounded, but a cross-backend
// element can be a whole agentic session — invisible to max_cost_usd,
// to the org monthly cap, and to a lending donor's ledger. The
// precedent is validateAndRetry, whose comment records that dropping
// the first attempt's usage "broke budget enforcement at the margins".
type chainSpend struct {
	tokens   int
	duration time.Duration
	costUSD  float64
}

func (s *chainSpend) add(r delegate.Result) {
	s.tokens += r.Tokens
	s.duration += r.Duration
	s.costUSD += cost.USDFromOutput(r.Output)
}

// applyTo folds the accumulated spend into the winning result.
//
// Both Result.Tokens and Output["_tokens"] are updated: the runtime's
// budget enforcement (extractUsage) reads the map, not the struct, and
// every shipped backend already populates `_tokens` via Annotate — so
// leaving the map at the last route's figure alone would under-report
// the chain's true cost to max_cost_usd / org caps / donor ledgers.
func (s chainSpend) applyTo(r delegate.Result) delegate.Result {
	if s.tokens == 0 && s.duration == 0 && s.costUSD == 0 {
		return r
	}
	r.Tokens += s.tokens
	r.Duration += s.duration
	if r.Output != nil {
		if s.tokens > 0 {
			r.Output["_tokens"] = r.Tokens
		}
		if s.costUSD > 0 {
			r.Output["_cost_usd"] = cost.USDFromOutput(r.Output) + s.costUSD
		}
	}
	return r
}

// ErrChainExhausted is the terminal error of a fallback chain whose
// every element failed. It carries EVERY element's error, not just the
// last one.
//
// That is the whole point of the type. Two independent mechanisms
// errors.As on what a node surfaces — the run-level usage-window retry
// (pkg/runner/usage_retry.go) and the credential-pool donor cooldown
// (pkg/runner/loop_spend.go) — and both look for
// *delegate.ErrRateLimited{usage_window}. A chain that starts on an
// exhausted forfait and ends on an unrelated 401 would, if it surfaced
// only the last error, silently disarm both: the run is never parked
// until the window reopens, and the donor whose subscription is shut
// keeps being handed to the next run.
type ErrChainExhausted struct {
	Chain string  // the chain in its source form, for the message
	Errs  []error // one per element, in walk order
}

func (e *ErrChainExhausted) Error() string {
	last := ""
	if n := len(e.Errs); n > 0 && e.Errs[n-1] != nil {
		last = e.Errs[n-1].Error()
	}
	return fmt.Sprintf("all routes in chain %s failed; last error: %s", e.Chain, last)
}

// Unwrap exposes every element's error so errors.Is / errors.As traverse
// the whole set rather than the last element alone.
func (e *ErrChainExhausted) Unwrap() []error { return e.Errs }

// dispatchWithProviderFallback runs backend.Execute across the node's
// provider chain, transparently falling through to the next provider on
// a hard failure beyond the retry budget. It mutates task.ProviderHint
// (and, for elements that pin one, task.Model) per attempt and returns
// the first success, or the last error once the chain is exhausted.
//
// "Hard failure" is any non-nil error returned by retryDelegateLoop —
// a non-retryable error, or a retryable one that exhausted the budget.
// Context cancellation / deadline is NOT a provider failure: it aborts
// the chain immediately so a cancelled run doesn't thrash through every
// provider. Each fall-through emits exactly one log note and one
// OnProviderFallback hook, so the operator sees a route change, not a
// failure.
//
// task.Model as built by buildTask is the node baseline. An element that
// declares a `provider:model` model overrides it for that attempt; an
// element without one (Model == "") restores the baseline — so a
// model-less element after a model-bearing one does NOT inherit the
// previous element's override.
func (e *ClawExecutor) dispatchWithProviderFallback(
	ctx context.Context,
	nodeID, backendName string,
	chain []chainElement,
	backend delegate.Backend,
	task *delegate.Task,
) (delegate.Result, error) {
	chain = collapseHintOnlyChain(chain, backendName)
	baseModel := task.Model
	out, err := e.dispatchChain(ctx, nodeID, chain, baseModel,
		func(_ context.Context, _ int, el chainElement) (string, delegate.Backend, *delegate.Task, error) {
			task.ProviderHint = el.Provider
			// An element without its own model restores the node
			// baseline, so a model-less element after a model-bearing one
			// does NOT inherit the previous element's override.
			if el.Model != "" {
				task.Model = el.Model
			} else {
				task.Model = baseModel
			}
			return backendName, backend, task, nil
		})
	return out.Result, err
}

// elementBuilder resolves the backend and builds a FRESH delegate.Task
// for one chain element. index is the element's position; anything > 0
// is a fall-through, and the builder is responsible for not letting it
// inherit the previous element's backend-specific continuity.
//
// Passing a builder rather than a prebuilt (backend, task) pair is what
// makes a cross-backend chain possible at all: buildTask bakes at least
// seven backend-shaped fields (SystemPromptMode, UserContent,
// AllowedTools, ToolDefs, MCPServers, the model-spec format, Hooks), so
// re-issuing a claude_code-shaped task on claw produces a TOOL-LESS
// agent that still carries an output schema — a schema-valid verdict it
// never verified.
type elementBuilder func(ctx context.Context, index int, el chainElement) (string, delegate.Backend, *delegate.Task, error)

// newElementBuilder wires the per-element plumbing both dispatch sites
// share: resolve-and-cache the backend, build-and-cache one task per
// backend, apply the element's provider/model, and drop resume
// continuity on a fall-through.
//
// `assemble` is the only part that differs — the agent/judge path calls
// buildTask, the LLM router assembles its own literal — and it is called
// AT MOST ONCE PER BACKEND. That is what keeps a legacy `provider:`
// chain free: every element resolves to the same backend, so the task is
// built once and only ProviderHint/Model change between attempts, which
// is byte-for-byte the pre-ADR-087 behaviour. A rebuild happens exactly
// when an element names a different backend — the case where reusing the
// task would silently change what the node can DO.
func (e *ClawExecutor) newElementBuilder(
	nodeID, baseBackendName string,
	baseBackend delegate.Backend,
	assemble func(ctx context.Context, backendName string) (*delegate.Task, error),
) elementBuilder {
	backends := map[string]delegate.Backend{}
	if baseBackend != nil {
		backends[baseBackendName] = baseBackend
	}
	tasks := map[string]*delegate.Task{}
	baseModels := map[string]string{}

	return func(ctx context.Context, index int, el chainElement) (string, delegate.Backend, *delegate.Task, error) {
		bn := baseBackendName
		if el.Backend != "" {
			bn = el.Backend
		}
		backend, ok := backends[bn]
		if !ok {
			if e.backendRegistry == nil {
				return "", nil, nil, fmt.Errorf("model: node %q uses backend %q but no backend registry configured", nodeID, bn)
			}
			resolved, err := e.backendRegistry.Resolve(bn)
			if err != nil {
				return "", nil, nil, fmt.Errorf("model: node %q: %w", nodeID, err)
			}
			backend = resolved
			backends[bn] = resolved
		}
		task, ok := tasks[bn]
		if !ok {
			built, err := assemble(ctx, bn)
			if err != nil {
				return "", nil, nil, err
			}
			task = built
			tasks[bn] = task
			baseModels[bn] = built.Model
		}
		task.ProviderHint = el.Provider
		if el.Model != "" {
			task.Model = el.Model
		} else {
			task.Model = baseModels[bn]
		}
		if index > 0 {
			// A fall-through starts a fresh conversation. The resume
			// continuity applied at build time (the operator's answer,
			// the pending tool_use, the prior messages) belongs to the
			// element that paused; replaying it into another backend
			// re-sends one provider's turn to a provider that never
			// issued it.
			task.ResumeConversation = nil
			task.ResumePendingToolUseID = ""
			task.ResumeAnswer = ""
			task.SessionID = ""
			task.SessionFingerprint = ""
			task.ForkSession = false
		}
		return bn, backend, task, nil
	}
}

// chainOutcome names the element that actually SERVED a node, alongside
// its result. Everything downstream of dispatch — the schema-validation
// retry, an ask_user pause's checkpoint, the output stamp — must act on
// the serving element, not on the one the node asked for. Before
// ADR-087 the two could not differ, so the executor carried a single
// backendName; a chain that can cross backends makes that variable a
// lie the moment it falls through.
type chainOutcome struct {
	Result      delegate.Result
	BackendName string           // backend that served
	Backend     delegate.Backend // its handle, for the schema retry
	Task        *delegate.Task   // the task it ran, for the schema retry
	// ServedBy names the element that served, and FellThrough reports
	// whether it was anything but the node's first choice. They exist so
	// a bot's DETERMINISTIC gate can fail closed on a degraded input —
	// the same posture as an unreadable output. A reviewer served by a
	// weaker model still emits a well-formed verdict; the count it
	// produces is the only thing that changes, and nothing else in the
	// run would record why.
	ServedBy    string
	FellThrough bool
	// Skipped reports that the chain ended on an `action: skip` terminal
	// route: nothing served, and the caller must synthesize the node's
	// zero-value output (only the caller knows the schema). Result carries
	// the failed routes' accumulated spend, never content.
	Skipped bool
}

// dispatchChain walks a node's fallback chain, building each element
// fresh and returning the first success — or an ErrChainExhausted
// carrying every element's error once the chain runs out.
//
// "Failure" is any non-nil error from the retry loop: a non-retryable
// error, or a retryable one that exhausted its budget. Context
// cancellation / deadline is NOT an element failure — it is terminal for
// the whole node, so it aborts the walk rather than thrashing through
// every remaining element.
//
// Each fall-through evicts the node's in-process conversation, emits one
// log note and one OnProviderFallback hook, and accumulates what the
// failed element spent. The operator sees a route change, not a failure.
// baseModel is the node's own `model:` — what an element that pins none
// of its own runs. It is reported on the fall-through event so the
// operator reads the model that WILL run rather than a blank field, and
// it makes the inheritance rule explicit: an element without a model
// restores the baseline instead of inheriting its predecessor's
// override.
func (e *ClawExecutor) dispatchChain(
	ctx context.Context,
	nodeID string,
	chain []chainElement,
	baseModel string,
	build elementBuilder,
) (chainOutcome, error) {
	if len(chain) == 0 {
		chain = []chainElement{{}}
	}

	var (
		result      delegate.Result
		err         error
		spent       chainSpend
		causes      []error
		nextAllowed int // first index the walk may execute (skip filter)
		// lastCat is the most recent EXECUTE failure's classification. A
		// later build error must route on it, not on Unclassified: a
		// usage_window outage followed by an unbuildable rescue route
		// would otherwise disarm a usage_window-filtered skip and turn
		// the operator's `skip` policy into `wait`.
		lastCat = delegate.FallbackUnclassified
		// lastBackend is the backend of the most recently EXECUTED route —
		// the spend's origin. The skip outcome must carry it: an empty
		// BackendName falls back to the node's REQUESTED backend at the
		// event layer, and the runner's cost accumulator keys its claw
		// double-count exclusion on that name — a metered route's real
		// spend mislabelled "claw" is erased from the org cap and the
		// credpool donor ledger. (Known limit: a chain that burned on TWO
		// backends keeps one label — the last one.)
		lastBackend string
	)
	effModel := func(el chainElement) string {
		if el.Model != "" {
			return el.Model
		}
		return baseModel
	}
	for i, el := range chain {
		if i < nextAllowed {
			// Skipped by an earlier `on:` filter (Re50c7d). These
			// elements were never built or executed.
			continue
		}
		rest := chain[i+1:]
		fallbackRemains := len(rest) > 0

		// An `action: skip` terminal route: the walk arrived here through a
		// failure its `on:` filter accepted (a skip element is never first —
		// chain[0] is always the node's own route, C173 pins skip last).
		// Complete the node with a skip outcome carrying only the failed
		// routes' spend; the caller synthesizes the zero-value output.
		if el.Skip {
			res := spent.applyTo(delegate.Result{Output: map[string]any{}})
			return chainOutcome{
				Result: res,
				// The route that EXECUTED and spent — see lastBackend.
				BackendName: lastBackend,
				ServedBy:    stepLabel(el),
				FellThrough: true,
				Skipped:     true,
			}, nil
		}

		backendName, backend, task, buildErr := build(ctx, i, el)
		if buildErr != nil {
			// A build failure is this element's failure, not the node's:
			// an unresolvable backend or an uncredentialed element must
			// not veto the elements after it.
			err = buildErr
			causes = append(causes, buildErr)
			// A build spends nothing of its own. Zero the terminal
			// result so a prior failed-execute that already sits in
			// `spent` is not applied twice when the walk ends on a
			// trailing build error (R5180a7).
			result = delegate.Result{}
			if !fallbackRemains {
				break
			}
			// A build error carries no classification of its own; route on
			// the last EXECUTE failure's category (Unclassified when none
			// yet) through the same acceptance walk as an execute failure —
			// a FILTERED skip is never reached by an unclassified build
			// error, and a usage_window-filtered skip still fires when the
			// original outage WAS a usage window.
			j := firstAcceptingFrom(chain, i+1, lastCat)
			if j < 0 {
				if e.logger != nil {
					e.logger.Warn("[%s#%d/%s] %q failed to build; no remaining route accepts an unclassified failure — stopping the chain",
						nodeID, LoopIterationFromContext(ctx), backendName, stepLabel(el))
				}
				break
			}
			next := chain[j]
			toModel := effModel(next)
			if next.Skip {
				toModel = ""
			}
			e.noteFallback(ctx, nodeID, el, next, backendName, effModel(el), toModel, buildErr)
			nextAllowed = j
			continue
		}
		key := cooldownKey(backendName, task)
		if fallbackRemains {
			if cd, cooled := e.routeCooldowns.active(key, e.cooldownNow()); cooled {
				// A remembered failure obeys the same per-route `on:` filters
				// as a fresh one. If nothing later accepts it, fail open by
				// attempting this element normally — especially important for
				// a node whose authored fallback does not cover this category.
				j := firstAcceptingFrom(chain, i+1, cd.Category)
				if j >= 0 {
					next := chain[j]
					toModel := effModel(next)
					if next.Skip {
						toModel = ""
					}
					e.noteCooldownFallback(ctx, nodeID, el, next, backendName,
						effModel(el), toModel, cd)
					// The call was skipped, but the condition that caused the
					// skip is still active. Preserve its typed cause so a later
					// fallback failure cannot hide a usage-window wall from the
					// run-level durable retry or credential-pool donor rest.
					if cd.Cause != nil {
						causes = append(causes, cd.Cause)
					}
					// A route change drops the node's session even when no call
					// was made on this dispatch. The store has no provider
					// fingerprint, so a prior failed attempt's messages must not
					// be replayed into the fallback element.
					e.evictNodeSessionForFallback(ctx, nodeID)
					// A remembered failure must route a later build error exactly
					// like a fresh one. Otherwise a usage_window-filtered terminal
					// skip becomes unreachable after an unbuildable rescue route.
					lastCat = cd.Category
					nextAllowed = j
					continue
				}
			}
		}
		// Whether ANY remaining route would take a given failure — the
		// carve-out must not strip a retry budget the chain will not
		// compensate for. Scanning the whole rest (not just the next
		// element) matches the skip semantics of `on:` (Re50c7d).
		var accepts func(error) bool
		if fallbackRemains {
			accepts = func(failure error) bool {
				return anyElementAccepts(rest, delegate.ClassifyFallback(failure, isDelegateRetryable(failure)))
			}
		}
		lastBackend = backendName
		result, err = e.retryDelegateLoopChain(ctx, nodeID, backendName, accepts, func() (delegate.Result, error) {
			return backend.Execute(ctx, *task)
		})
		// Best-effort session degrade (inherit_if_available / persist): the
		// upstream session id resolved, but its backing state can be gone —
		// a cloud resume replaces the sandbox container and a CLI backend's
		// session files die with it, after which every resume of this node
		// fails identically (lived on branch-improve-loop's plan_revise:
		// error_during_execution in ~2.6s, forever). "If available" covers
		// that too: retry ONCE with the session dropped, loudly. Only for
		// execution-shaped failures — an auth/usage-window/unavailable
		// failure is credential- or model-level and a fresh session would
		// hit the same wall.
		if err != nil && ctx.Err() == nil && task.SessionOptional && task.SessionID != "" {
			switch delegate.ClassifyFallback(err, isDelegateRetryable(err)) {
			case delegate.FallbackUnclassified, delegate.FallbackTransientExhausted:
				if e.logger != nil {
					e.logger.Warn("[%s#%d/%s] optional session %s failed to serve (%v) — retrying once with a FRESH session; the node will not see the upstream conversation",
						nodeID, LoopIterationFromContext(ctx), backendName, task.SessionID, err)
				}
				spent.add(result)
				fresh := *task
				fresh.SessionID = ""
				fresh.ForkSession = false
				fresh.SessionFingerprint = ""
				freshResult, freshErr := e.retryDelegateLoopChain(ctx, nodeID, backendName, accepts, func() (delegate.Result, error) {
					return backend.Execute(ctx, fresh)
				})
				if freshErr == nil {
					task = &fresh
				}
				result, err = freshResult, freshErr
			}
		}
		if err == nil {
			return chainOutcome{
				Result:      spent.applyTo(result),
				BackendName: backendName,
				Backend:     backend,
				Task:        task,
				ServedBy:    stepLabel(el),
				FellThrough: i > 0,
			}, nil
		}
		causes = append(causes, err)

		if ctx.Err() != nil {
			return chainOutcome{Result: result, BackendName: backendName}, err
		}
		cat := delegate.ClassifyFallback(err, isDelegateRetryable(err))
		lastCat = cat
		// Remember only typed failures whose provider supplied a future
		// reset. Missing or stale reset data deliberately leaves the route
		// hot, so bookkeeping uncertainty cannot suppress a healthy call.
		//
		// Recorded BEFORE the chain-shape checks below, because what the
		// provider refused is a property of the ROUTE, not of this node's
		// fallback chain: a node with no fallback at all — or none whose
		// `on:` accepts the category — still teaches the ledger, so the
		// next node whose chain DOES have somewhere to go skips the spawn
		// this one had to pay for.
		e.routeCooldowns.record(key, cooldownForFailure(err, cat), e.cooldownNow())
		if !fallbackRemains {
			break
		}
		// `on:` is a per-route filter, not a chain terminator (Re50c7d).
		// A middle route that refuses the category is SKIPPED so a later
		// route that accepts it (e.g. the shipped example's gpt route
		// taking transient_exhausted after api's default set refuses it)
		// still runs. The walk ends only when NO remaining route accepts.
		j := firstAcceptingFrom(chain, i+1, cat)
		if j < 0 {
			if e.logger != nil {
				e.logger.Warn("[%s#%d/%s] %q failed (%s); no remaining route accepts that condition — stopping the chain",
					nodeID, LoopIterationFromContext(ctx), backendName, stepLabel(el), cat)
			}
			// Do NOT spent.add: the terminal `result` is this element,
			// and the tail folds it with `spent.applyTo` (R5180a7).
			break
		}
		for k := i + 1; k < j; k++ {
			if e.logger != nil {
				e.logger.Warn("[%s#%d/%s] %q failed (%s); skipping %q (does not accept) — trying later routes",
					nodeID, LoopIterationFromContext(ctx), backendName, stepLabel(el), cat, stepLabel(chain[k]))
			}
		}
		// Accumulate only once this route is definitively abandoned AND
		// another route will try. The terminal `result` is folded with
		// `spent` on the way out, so adding a route that is also the
		// final result would count it twice — and on a single-route
		// node that is a plain doubling of what the delegate_error
		// event reports.
		spent.add(result)
		// Drop the failed element's conversation before another
		// backend/provider sees it. The session store is keyed
		// (runID, nodeID) with no provider fingerprint and captures a
		// FAILED attempt's messages, so replaying them into the next
		// element re-sends one provider's signed thinking blocks to
		// another — a 400 at best, a mangled conversation at worst.
		e.evictNodeSessionForFallback(ctx, nodeID)
		fromModel := result.EffectiveModel
		if fromModel == "" {
			fromModel = effModel(el)
		}
		next := chain[j]
		toModel := effModel(next)
		if next.Skip {
			// A skip route runs no model; inheriting the baseline here
			// would report a model that will never execute.
			toModel = ""
		}
		e.noteFallback(ctx, nodeID, el, next, backendName, fromModel, toModel, err)
		nextAllowed = j
	}

	// An exhausted chain still spent what its routes burned: fold the
	// accumulation in here too, or a 3-route failure under-reports two
	// whole agentic sessions to max_cost_usd, the org monthly cap and a
	// lending donor's ledger.
	result = spent.applyTo(result)
	if len(chain) > 1 {
		return chainOutcome{Result: result}, &ErrChainExhausted{Chain: chainLabel(chain), Errs: causes}
	}
	return chainOutcome{Result: result}, err
}

// fallbackRouteEnds resolves the two ends of a route change for the event
// layer: an element that pins no backend of its own inherits the node's, and
// a terminal `action: skip` names NO backend — inheriting one would report a
// bascule to a backend that will never run.
//
// Shared by both note* paths on purpose: a fresh refusal and a remembered
// cooldown skip describe the same route change, so they must not be able to
// describe it differently.
func fallbackRouteEnds(from, to chainElement, backendName string) (fromBackend, toBackend string) {
	fromBackend = from.Backend
	if fromBackend == "" {
		fromBackend = backendName
	}
	if to.Skip {
		return fromBackend, ""
	}
	toBackend = to.Backend
	if toBackend == "" {
		toBackend = backendName
	}
	return fromBackend, toBackend
}

// noteCooldownFallback exposes a route change that cost no delegate attempt.
// It remains a model_fallback timeline event, but attempts=0 and the cooldown
// metadata distinguish it from the refusal that originally armed the entry.
func (e *ClawExecutor) noteCooldownFallback(
	ctx context.Context,
	nodeID string,
	from, to chainElement,
	backendName string,
	fromModel, toModel string,
	cd routeCooldown,
) {
	fromBackend, toBackend := fallbackRouteEnds(from, to, backendName)
	if e.logger != nil {
		e.logger.Info("[%s#%d/%s] skipping %q: %s cooldown active until %s; routing to %q",
			nodeID, LoopIterationFromContext(ctx), backendName, stepLabel(from),
			cd.Category, cd.Until.UTC().Format(time.RFC3339), stepLabel(to))
	}
	if e.hooks.OnProviderFallback == nil {
		return
	}
	e.hooks.OnProviderFallback(nodeID, ProviderFallbackInfo{
		BackendName:   backendName,
		From:          from.Provider,
		To:            to.Provider,
		FromModel:     fromModel,
		ToModel:       toModel,
		FromBackend:   fromBackend,
		ToBackend:     toBackend,
		Reason:        string(cd.Category),
		Attempts:      0,
		Cooldown:      true,
		CooldownUntil: cd.Until,
		ToSkip:        to.Skip,
	})
}

// noteFallback emits the one log line and the one hook that make a
// route change visible.
func (e *ClawExecutor) noteFallback(
	ctx context.Context,
	nodeID string,
	from, to chainElement,
	backendName string,
	fromModel, toModel string,
	err error,
) {
	fromBackend, toBackend := fallbackRouteEnds(from, to, backendName)
	if e.logger != nil {
		e.logger.Warn("[%s#%d/%s] %q failed beyond retry budget; falling through to %q: %v",
			nodeID, LoopIterationFromContext(ctx), backendName,
			stepLabel(from), stepLabel(to), err)
	}
	if e.hooks.OnProviderFallback == nil {
		return
	}
	e.hooks.OnProviderFallback(nodeID, ProviderFallbackInfo{
		BackendName: backendName,
		From:        from.Provider,
		To:          to.Provider,
		FromModel:   fromModel,
		ToModel:     toModel,
		FromBackend: fromBackend,
		ToBackend:   toBackend,
		Reason:      string(delegate.ClassifyFallback(err, isDelegateRetryable(err))),
		Attempts:    e.retry.maxAttempts(),
		Err:         err,
		ToSkip:      to.Skip,
	})
}

// evictNodeSessionForFallback drops the node's in-process claw
// conversation so the next chain element starts fresh.
//
// The cost is stated plainly: the failed attempt's work is discarded,
// which the steady-state path deliberately preserves for compaction.
// Keeping it would be worse — the store has no provider fingerprint, so
// the preserved conversation would be replayed into whatever runs next.
func (e *ClawExecutor) evictNodeSessionForFallback(ctx context.Context, nodeID string) {
	runID, sessions := runtimeContextFrom(ctx)
	if runID == "" || sessions == nil {
		return
	}
	sessions.evict(runID, nodeID)
}
