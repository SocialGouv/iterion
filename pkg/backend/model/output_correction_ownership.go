package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

const OutputCorrectionProtocol = 1

// OutputCorrectionOwnership is copied into each Execute context by its caller.
// Only a caller holding the complete fenced store protocol may install it.
// No constructor or setter on a shared executor activates correction.
type OutputCorrectionOwnership struct {
	Protocol  int
	Authority store.RunExecutionAuthority
}
type correctionOwnershipKey struct{}

func WithoutOutputCorrectionOwnership(ctx context.Context) context.Context {
	return context.WithValue(ctx, correctionOwnershipKey{}, (*OutputCorrectionOwnership)(nil))
}

type correctionHandleKey struct{}
type correctionConversationKey struct{}

func WithOutputCorrectionOwnership(ctx context.Context, ownership OutputCorrectionOwnership) context.Context {
	return context.WithValue(ctx, correctionOwnershipKey{}, ownership)
}
func OutputCorrectionOwnershipFromContext(ctx context.Context) (OutputCorrectionOwnership, bool) {
	ownership, ok := ctx.Value(correctionOwnershipKey{}).(OutputCorrectionOwnership)
	return ownership, ok
}

var ErrOutputCorrectionIneligible = errors.New("model: no eligible output correction handle for this invocation")
var ErrOutputCorrectionReleased = errors.New("model: output correction handle already released")

// OutputCorrectionFailure transports invocation-local state through Execute's
// redaction boundary. It is not a runtime SCHEMA_VALIDATION error. Its handle
// contains live resources and must never be serialized or logged.
type OutputCorrectionFailure struct {
	handle     *OutputCorrectionHandle
	original   map[string]any
	validation error
	message    string
}

func (f *OutputCorrectionFailure) Error() string                   { return f.message }
func (f *OutputCorrectionFailure) Unwrap() error                   { return f.validation }
func (f *OutputCorrectionFailure) Handle() *OutputCorrectionHandle { return f.handle }
func (f *OutputCorrectionFailure) ParseFallback() bool             { return f.handle.parseFallback }

// Output returns a copy so callers cannot change the next round's evidence.
func (f *OutputCorrectionFailure) Output() map[string]any { return copyCorrectionOutput(f.original) }

// correctionLifetime owns BOTH the grant and the auto-memory mirror. Before
// transfer it is released by executeBackend's defer, including panic unwinds.
type correctionLifetime struct {
	once       sync.Once
	revoke     func()
	syncMemory func()
}

func (l *correctionLifetime) release() {
	l.once.Do(func() {
		if l.revoke != nil {
			defer l.revoke()
		}
		if l.syncMemory != nil {
			l.syncMemory()
		}
	})
}

// OutputCorrectionHandle is opaque and belongs to one Execute, never a nodeID.
// Release is required on every terminal path, including budget zero and a
// persistence refusal before the first round. It is safe to call repeatedly.
type correctionExtractionStep struct {
	task               delegate.Task
	primary, secondary delegate.Result
}

type OutputCorrectionHandle struct {
	pendingExtraction *correctionExtractionStep
	continuation      json.RawMessage
	mu                sync.Mutex
	executor          *ClawExecutor
	node              ir.Node
	finalSessionKey   string
	finalPersist      bool
	fields            backendFields
	ownership         OutputCorrectionOwnership
	task              delegate.Task
	backend           delegate.Backend
	backendName       string
	raw               delegate.Result
	aggregate         delegate.Result
	schema            *ir.Schema
	registry          map[string]*ir.Schema
	route             chainOutcome
	deadline          time.Time
	timeout           time.Duration
	parseFallback     bool
	lifetime          *correctionLifetime
	released          bool
	finalized         bool
	sessionCosts      map[correctionSession]float64
	usageCollector    *ExecutionUsageCollector
}
type correctionSession struct{ backend, id, fingerprint string }

// HasPendingExtraction reports a distinct next operation, never permission to
// call it. The owner must reserve another correction attempt first.
func (h *OutputCorrectionHandle) HasPendingExtraction() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pendingExtraction != nil && !h.released && !h.finalized
}

func (h *OutputCorrectionHandle) String() string   { return "<output correction handle>" }
func (h *OutputCorrectionHandle) GoString() string { return h.String() }
func (h *OutputCorrectionHandle) MarshalJSON() ([]byte, error) {
	return nil, errors.New("model: live correction handle cannot be serialized")
}
func (h *OutputCorrectionHandle) MarshalBSON() ([]byte, error) {
	return nil, errors.New("model: live correction handle cannot be serialized")
}

func (h *OutputCorrectionHandle) Ownership() OutputCorrectionOwnership { return h.ownership }
func (h *OutputCorrectionHandle) Deadline() (time.Time, time.Duration) { return h.deadline, h.timeout }
func (h *OutputCorrectionHandle) Output() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return copyCorrectionOutput(h.aggregate.Output)
}
func (h *OutputCorrectionHandle) Release() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.released {
		h.released = true
		h.lifetime.release()
	}
}

// WithOutputCorrectionHandle selects this invocation, not another concurrent
// branch of the same node. It does not install or refresh its execution epoch.
func WithOutputCorrectionHandle(ctx context.Context, h *OutputCorrectionHandle) context.Context {
	return context.WithValue(ctx, correctionHandleKey{}, h)
}

func (e *ClawExecutor) correctionHandle(ctx context.Context, node ir.Node) (*OutputCorrectionHandle, error) {
	h, _ := ctx.Value(correctionHandleKey{}).(*OutputCorrectionHandle)
	ownership, ok := OutputCorrectionOwnershipFromContext(ctx)
	if h == nil || h.executor != e || h.node != node || h.usageCollector != executionUsageCollector(ctx) || !ok || ownership != h.ownership || (RunIDFromContext(ctx) != "" && RunIDFromContext(ctx) != ownership.Authority.RunID) {
		return nil, ErrOutputCorrectionIneligible
	}
	return h, nil
}

func (e *ClawExecutor) newCorrectionHandle(ctx context.Context, node ir.Node, f backendFields, ownership OutputCorrectionOwnership, backendName string, backend delegate.Backend, task *delegate.Task, result delegate.Result, schema *ir.Schema, route chainOutcome, deadline time.Time, timeout time.Duration, lifetime *correctionLifetime) (*OutputCorrectionHandle, error) {
	frozenTask, err := copyCorrectionTask(*task)
	if err != nil {
		return nil, err
	}
	h := &OutputCorrectionHandle{usageCollector: executionUsageCollector(ctx), executor: e, node: node, fields: f, ownership: ownership, task: frozenTask, backend: backend, backendName: backendName, raw: copyCorrectionResult(result), aggregate: copyCorrectionResult(result), schema: copyCorrectionSchema(schema), registry: copyCorrectionSchemas(e.schemas), route: route, deadline: deadline, timeout: timeout, parseFallback: result.ParseFallback, lifetime: lifetime, sessionCosts: map[correctionSession]float64{}}
	if node != nil {
		h.finalSessionKey = nodeSessionKey(node)
		llm, ok := node.(ir.LLMNode)
		h.finalPersist = ok && llm.GetSession() == ir.SessionPersist
	}
	if backendName == delegate.BackendClaw {
		h.continuation = append(json.RawMessage(nil), e.completedClawConversation(ctx, &h.task, result)...)
	}
	h.route.Task = nil
	h.route.Result = delegate.Result{}
	if result.CostIsSessionTotal {
		h.sessionCosts[h.sessionKey(result)] = result.CostUSD
	}
	return h, nil
}

// prepareOutputCorrection stands only eligible agent/judge failures down.
// Routers call the local adapter directly and never enter this seam.
func (e *ClawExecutor) prepareOutputCorrection(ctx context.Context, node ir.Node, f backendFields, backendName string, backend delegate.Backend, task *delegate.Task, result delegate.Result, schema *ir.Schema, route chainOutcome, deadline time.Time, timeout time.Duration, lifetime *correctionLifetime) (*OutputCorrectionFailure, error) {
	ownership, owned := OutputCorrectionOwnershipFromContext(ctx)
	if !owned {
		return nil, nil
	}
	if ownership.Protocol != OutputCorrectionProtocol || ownership.Authority.RunID == "" || ownership.Authority.Epoch == "" {
		return nil, fmt.Errorf("model: invalid output correction ownership protocol or execution authority")
	}
	validation := ValidateOutputWithRegistry(result.Output, schema, e.schemas)
	if validation == nil {
		return nil, nil
	}
	if err := schemaProviderError(f, result); err != nil {
		return nil, err
	}
	violations := CollectOutputViolations(result.Output, schema, e.schemas)
	if !OutputCorrectionEligible(violations, result.ParseFallback) {
		return nil, nil
	}
	h, err := e.newCorrectionHandle(ctx, node, f, ownership, backendName, backend, task, result, schema, route, deadline, timeout, lifetime)
	if err != nil {
		return nil, err
	}
	return &OutputCorrectionFailure{handle: h, original: copyCorrectionOutput(result.Output), validation: validation, message: fmt.Sprintf("model: %s: structured output invalid: %s", f.subject(), validation)}, nil
}

// CorrectOutputWithUsage performs exactly one shared correction round. An
// arrived candidate is returned without a validation error; the owning caller
// decides whether it conforms and may reserve another round. Usage is marginal.
func (e *ClawExecutor) CorrectOutputWithUsage(ctx context.Context, node ir.Node, output map[string]any, validationErr error) (map[string]any, store.OutputCorrectionUsage, error) {
	h, err := e.correctionHandle(ctx, node)
	if err != nil {
		return nil, store.OutputCorrectionUsage{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released || h.finalized {
		return nil, store.OutputCorrectionUsage{}, ErrOutputCorrectionReleased
	}
	ctx = e.correctionRuntimeContext(ctx)
	round := e.runSchemaCorrectionRound(ctx, h, validationErr)
	if !round.answered {
		return copyCorrectionOutput(h.aggregate.Output), round.usage, e.scrubNodeError(round.err)
	}
	return copyCorrectionOutput(round.result.Output), round.usage, nil
}
func (e *ClawExecutor) CorrectOutput(ctx context.Context, node ir.Node, output map[string]any, validationErr error) (map[string]any, error) {
	candidate, _, err := e.CorrectOutputWithUsage(ctx, node, output, validationErr)
	return candidate, err
}
func (e *ClawExecutor) correctionRuntimeContext(ctx context.Context) context.Context {
	if runID := RunIDFromContext(ctx); runID != "" && e.sessions != nil {
		return withRuntimeContext(ctx, runID, e.sessions)
	}
	return ctx
}

// FinalizeCorrectedOutput shares the ordinary Execute success path. Repeated
// calls for this handle do not pack, evict or fire OnNodeFinished twice.
func (e *ClawExecutor) FinalizeCorrectedOutput(ctx context.Context, node ir.Node, output map[string]any) error {
	h, err := e.correctionHandle(ctx, node)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finalized {
		return nil
	}
	if h.released {
		return ErrOutputCorrectionReleased
	}
	h.finalized = true
	e.finalizeNodeOutputFacts(ctx, h.fields.id, h.finalSessionKey, h.finalPersist, output)
	return nil
}
func (e *ClawExecutor) finalizeNodeOutput(ctx context.Context, node ir.Node, output map[string]any) {
	llm, isLLM := node.(ir.LLMNode)
	e.finalizeNodeOutputFacts(ctx, node.NodeID(), nodeSessionKey(node), isLLM && llm.GetSession() == ir.SessionPersist, output)
}
func (e *ClawExecutor) finalizeNodeOutputFacts(ctx context.Context, nodeID, key string, persist bool, output map[string]any) {
	runID := RunIDFromContext(ctx)
	if e.sessions != nil && runID != "" {
		// The node execution is over: the next one starts a tool-step
		// budget of its own (toolTurnsAnchorCarry).
		e.sessions.dropAnchor(runID, key)
		if persist && output != nil {
			sid, _ := output[delegate.SessionIDKey].(string)
			fingerprint, _ := output[delegate.SessionFingerprintKey].(string)
			if blob := e.packClawSession(runID, key, sid, fingerprint); len(blob) > 0 {
				output[delegate.SessionStateBlobKey] = blob
			}
		} else {
			e.sessions.evict(runID, key)
		}
	}
	if output != nil && e.hooks.OnNodeFinished != nil {
		e.hooks.OnNodeFinished(nodeID, output)
	}
}

func copyCorrectionSchema(schema *ir.Schema) *ir.Schema {
	if schema == nil {
		return nil
	}
	copy := *schema
	copy.Fields = make([]*ir.SchemaField, len(schema.Fields))
	for i, f := range schema.Fields {
		if f != nil {
			field := *f
			field.EnumValues = append([]string(nil), f.EnumValues...)
			copy.Fields[i] = &field
		}
	}
	return &copy
}
func copyCorrectionSchemas(schemas map[string]*ir.Schema) map[string]*ir.Schema {
	out := make(map[string]*ir.Schema, len(schemas))
	for name, schema := range schemas {
		out[name] = copyCorrectionSchema(schema)
	}
	return out
}
func copyCorrectionResult(result delegate.Result) delegate.Result {
	result.Output = copyCorrectionOutput(result.Output)
	return result
}
func copyCorrectionOutput(output map[string]any) map[string]any {
	if output == nil {
		return nil
	}
	return copyCorrectionData(reflect.ValueOf(output), map[correctionCopyKey]reflect.Value{}).Interface().(map[string]any)
}
func copyCorrectionTask(task delegate.Task) (delegate.Task, error) {
	frozen := copyCorrectionData(reflect.ValueOf(task), map[correctionCopyKey]reflect.Value{}).Interface().(delegate.Task)
	if task.Permission != nil {
		policy, err := permission.NewPolicyFromConfig(task.Permission.Config())
		if err != nil {
			return delegate.Task{}, fmt.Errorf("model: snapshot correction permission: %w", err)
		}
		frozen.Permission = policy
	}
	return frozen, nil
}

// Copy data containers while retaining callable/service capabilities by
// identity. No serialization: task functions, exact numbers and byte slices
// survive. Memoization preserves cycles within data and avoids aliasing the
// backend's mutable maps/slices across rounds.
type correctionCopyKey struct {
	typ    reflect.Type
	ptr    uintptr
	length int
}

func copyCorrectionData(v reflect.Value, seen map[correctionCopyKey]reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		kind := v.Elem().Kind()
		if kind != reflect.Map && kind != reflect.Slice && kind != reflect.Array {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(copyCorrectionData(v.Elem(), seen))
		return out
	case reflect.Map, reflect.Slice, reflect.Pointer:
		if v.Kind() == reflect.Pointer && !v.IsNil() && v.Elem().Kind() == reflect.Struct {
			for i := 0; i < v.Elem().NumField(); i++ {
				if !v.Elem().Field(i).CanInterface() {
					return v
				}
			}
		}
		if v.IsNil() {
			return v
		}
		key := correctionCopyKey{typ: v.Type(), ptr: v.Pointer()}
		if v.Kind() == reflect.Slice {
			key.length = v.Len()
		}
		if out, ok := seen[key]; ok {
			return out
		}
		var out reflect.Value
		switch v.Kind() {
		case reflect.Map:
			out = reflect.MakeMapWithSize(v.Type(), v.Len())
			seen[key] = out
			iter := v.MapRange()
			for iter.Next() {
				out.SetMapIndex(iter.Key(), copyCorrectionData(iter.Value(), seen))
			}
		case reflect.Slice:
			out = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
			seen[key] = out
			for i := 0; i < v.Len(); i++ {
				out.Index(i).Set(copyCorrectionData(v.Index(i), seen))
			}
		case reflect.Pointer:
			out = reflect.New(v.Type().Elem())
			seen[key] = out
			out.Elem().Set(copyCorrectionData(v.Elem(), seen))
		}
		return out
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !v.Field(i).CanInterface() {
				return v
			}
		}
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			out.Field(i).Set(copyCorrectionData(v.Field(i), seen))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(copyCorrectionData(v.Index(i), seen))
		}
		return out
	}
	return v
}
