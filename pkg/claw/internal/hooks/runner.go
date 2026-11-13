// Package hooks provides an in-process programmatic hook system for the
// claw-code-go runtime. It complements the public top-level `hooks` package
// (which executes external shell commands as hooks) by letting consumers
// register Go callbacks that observe and optionally intercept lifecycle
// events: pre/post tool execution, user prompt submission, compaction, and
// session stop.
//
// Design goals:
//
//   - Zero-cost when no Runner is configured. Call sites use
//     runner.Fire(...) only after a nil-check, and the integration in
//     runtime/conversation.go and runtime/compact.go is a single nil guard
//     plus a Fire call.
//
//   - Sequential, deterministic dispatch. Handlers for an event run in the
//     order they were registered. The first handler returning a non-Continue
//     decision wins and short-circuits subsequent handlers. This mirrors the
//     Rust plugin hooks runner's "first decision wins" semantics.
//
//   - Robust to misbehaving handlers. A handler returning an error is logged
//     to stderr and treated as Continue: hook authors should not be able to
//     break a conversation by panicking or erroring inside a hook. Panics in
//     a handler are recovered and treated as a logged error.
//
//   - Thread-safe registration and dispatch. Handlers can be registered from
//     any goroutine. Fire takes a read lock so concurrent dispatch is fine.
//
// This package is deliberately minimal: it does NOT include a shell-command
// hook runner (that lives in the public `hooks` package), a plugin lifecycle
// manager, or on-disk hook configuration. Those layers, if needed, can be
// built on top of this Runner by registering Handler functions that delegate
// to whatever transport the consumer prefers.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// Event identifies a lifecycle stage at which hooks can fire.
type Event int

const (
	// PreToolUse fires immediately before a tool is invoked. A Block decision
	// causes the runtime to skip the tool and inject a synthetic refusal
	// tool_result back to the model.
	PreToolUse Event = iota

	// PostToolUse fires after a tool returns successfully (no error).
	PostToolUse

	// PostToolUseFailure fires after a tool returns with an error. The error
	// is exposed via Context.ToolError.
	PostToolUseFailure

	// UserPromptSubmit fires immediately before the next user prompt is
	// submitted to the model. A Modify decision replaces Context.UserPrompt
	// with Decision.Replacement.UserPrompt.
	UserPromptSubmit

	// PreCompact fires before history compaction is triggered. A Block
	// decision instructs the caller to skip compaction this turn.
	PreCompact

	// PostCompact fires after compaction completes (whether or not it
	// actually trimmed messages).
	PostCompact

	// Stop fires once at session end (e.g. when the conversation loop tears
	// down). It is purely observational; decisions are ignored.
	Stop

	// PrePluginInstall fires before a plugin is installed. A Block decision
	// aborts the install: no files are copied, the registry is not mutated,
	// and Install returns an error so callers can surface the refusal.
	PrePluginInstall

	// PostPluginInstall fires after Install completes — successfully or not.
	// On success, Context.Plugin.Error is nil. On failure, it is set to the
	// error returned by the install pipeline so observers can audit the
	// outcome.
	PostPluginInstall

	// PrePluginUninstall fires before a plugin is uninstalled. A Block
	// decision aborts the uninstall: the plugin remains in the registry and
	// on disk.
	PrePluginUninstall

	// PostPluginUninstall fires after Uninstall completes — successfully or
	// not. Context.Plugin.Error reflects the outcome.
	PostPluginUninstall
)

// String returns the canonical name of an Event for diagnostics and logging.
func (e Event) String() string {
	switch e {
	case PreToolUse:
		return "PreToolUse"
	case PostToolUse:
		return "PostToolUse"
	case PostToolUseFailure:
		return "PostToolUseFailure"
	case UserPromptSubmit:
		return "UserPromptSubmit"
	case PreCompact:
		return "PreCompact"
	case PostCompact:
		return "PostCompact"
	case Stop:
		return "Stop"
	case PrePluginInstall:
		return "PrePluginInstall"
	case PostPluginInstall:
		return "PostPluginInstall"
	case PrePluginUninstall:
		return "PrePluginUninstall"
	case PostPluginUninstall:
		return "PostPluginUninstall"
	default:
		return fmt.Sprintf("Event(%d)", int(e))
	}
}

// PluginInfo carries plugin lifecycle metadata into the hook Context.
// Defined in this package (rather than in `plugin/`) to keep the hooks
// runner free of plugin-package imports and avoid an import cycle.
type PluginInfo struct {
	ID          string
	Name        string
	Version     string
	Description string
	InstallPath string
	Source      string
	Error       error
}

// Context is the payload passed to a Handler. Fields are populated only when
// relevant to the event:
//
//   - PreToolUse / PostToolUse / PostToolUseFailure: ToolName, ToolInput.
//     PostToolUse additionally sets ToolResult; PostToolUseFailure sets
//     ToolError.
//   - UserPromptSubmit: UserPrompt.
//   - PreCompact / PostCompact: MessageCount.
//   - Stop: no event-specific fields.
//   - PrePluginInstall / PostPluginInstall / PrePluginUninstall /
//     PostPluginUninstall: Plugin (PluginInfo). Pre-events leave
//     Plugin.Error nil; Post-events set it on failure.
//
// SessionID and WorkDir are populated on every event when the runtime knows
// them. The struct is intentionally flat so it is cheap to copy by value;
// callers should not retain pointers to it past the Handler invocation.
type Context struct {
	Event        Event
	ToolName     string
	ToolInput    map[string]any
	ToolResult   string
	ToolError    error
	UserPrompt   string
	MessageCount int
	SessionID    string
	WorkDir      string
	Plugin       *PluginInfo
}

// ActionKind enumerates what a Decision instructs the runtime to do.
type ActionKind int

const (
	// ActionContinue lets execution proceed unchanged. This is the
	// zero-value, so a default-constructed Decision is a Continue.
	ActionContinue ActionKind = iota

	// ActionModify rewrites the context. Currently honored only for
	// UserPromptSubmit, where Decision.Replacement.UserPrompt replaces the
	// pending user prompt.
	ActionModify

	// ActionBlock stops the operation. The runtime treats Block as a tool
	// refusal (PreToolUse) or a skipped compaction (PreCompact). For events
	// where blocking is meaningless (PostToolUse, Stop, etc.) a Block
	// decision is logged and treated as Continue.
	ActionBlock
)

// Decision is the result of a Handler invocation.
type Decision struct {
	// Action determines how the runtime interprets this decision.
	Action ActionKind

	// Replacement carries the new context fields for ActionModify. Only the
	// event-relevant fields need to be set (e.g. UserPrompt for
	// UserPromptSubmit). Nil for non-Modify actions.
	Replacement *Context

	// Reason is a human-readable explanation surfaced in logs and (for
	// Block) injected into the synthetic refusal tool_result. Optional.
	Reason string
}

// Handler is a hook callback. It receives the Go context and the hook payload
// and returns a Decision plus an optional error. Errors are logged and
// treated as Continue.
type Handler func(ctx context.Context, hctx Context) (Decision, error)

// Option configures a Runner at construction time.
type Option func(*Runner)

// WithLogger sets the writer used to log handler errors and warnings.
// Default: os.Stderr. Pass io.Discard to silence the runner.
func WithLogger(w io.Writer) Option {
	return func(r *Runner) {
		if w != nil {
			r.log = w
		}
	}
}

// Runner orchestrates registration and dispatch of programmatic hooks.
// The zero value is NOT ready to use; construct one via NewRunner.
type Runner struct {
	mu       sync.RWMutex
	handlers map[Event][]Handler
	log      io.Writer
}

// NewRunner returns an empty Runner. Use Register to attach handlers.
func NewRunner(opts ...Option) *Runner {
	r := &Runner{
		handlers: make(map[Event][]Handler),
		log:      os.Stderr,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Register attaches a handler for the given event. Handlers run in
// registration order. Register is safe to call concurrently; it serializes
// with concurrent Fire calls.
//
// A nil handler is silently ignored.
func (r *Runner) Register(event Event, h Handler) {
	if r == nil || h == nil {
		return
	}
	r.mu.Lock()
	r.handlers[event] = append(r.handlers[event], h)
	r.mu.Unlock()
}

// Fire dispatches the given context to all handlers registered for
// hctx.Event, in registration order. The first handler returning a
// non-Continue decision wins and is returned immediately.
//
// Errors from a handler are logged and treated as Continue (the chain is not
// broken). Panics in a handler are recovered, logged, and treated as
// Continue.
//
// If no handler returns a non-Continue decision (or no handlers are
// registered), Fire returns ActionContinue, nil.
//
// A nil Runner returns ActionContinue, nil — this is the documented no-op
// path for callers that haven't been configured with hooks.
func (r *Runner) Fire(ctx context.Context, hctx Context) (Decision, error) {
	if r == nil {
		return Decision{Action: ActionContinue}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Snapshot handler list under read lock so we don't hold the lock while
	// invoking user code (which could block, take other locks, etc.).
	r.mu.RLock()
	handlers := r.handlers[hctx.Event]
	snapshot := make([]Handler, len(handlers))
	copy(snapshot, handlers)
	logger := r.log
	r.mu.RUnlock()

	for i, h := range snapshot {
		decision, err := safeInvoke(ctx, h, hctx)
		if err != nil {
			fmt.Fprintf(logger, "[hooks] handler %d for %s returned error: %v (treating as Continue)\n",
				i, hctx.Event, err)
			// Continue past handler errors, but stop the chain if the
			// context itself is cancelled — the remaining handlers cannot
			// usefully run and we surface the cancellation to the caller.
			if cerr := ctx.Err(); cerr != nil {
				return Decision{Action: ActionContinue}, cerr
			}
			continue
		}
		if decision.Action == ActionContinue {
			// Stop the chain if the context was cancelled mid-flight so a
			// long handler list does not keep running after the conversation
			// has been torn down.
			if cerr := ctx.Err(); cerr != nil {
				return Decision{Action: ActionContinue}, cerr
			}
			continue
		}
		// First non-Continue wins.
		return decision, nil
	}

	return Decision{Action: ActionContinue}, ctx.Err()
}

// errHandlerPanic is returned from safeInvoke when a handler panics.
var errHandlerPanic = errors.New("handler panicked")

// safeInvoke runs h with panic recovery. A panicking handler is converted
// into an error so Fire can log and continue.
func safeInvoke(ctx context.Context, h Handler, hctx Context) (decision Decision, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			decision = Decision{Action: ActionContinue}
			err = fmt.Errorf("%w: %v", errHandlerPanic, rec)
		}
	}()
	return h(ctx, hctx)
}
