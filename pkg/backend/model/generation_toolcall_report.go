package model

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// ---------------------------------------------------------------------------
// What a tool call's events say about where it came from and how it ran.
// ---------------------------------------------------------------------------

// toolCallInfo is the ONE constructor of a ToolCallInfo for a tool_use block.
//
// Every path that reports a call — the start, the completion and each
// refusal — builds its info here, so the identity a reader groups by (the
// call's own id, the id of the call that made it, its batch slot) is stamped
// by one function rather than remembered at eight sites. A site that wrote
// its own literal would be the site where a nested call's parent goes
// missing; TestEveryToolCallInfoComesFromTheOneConstructor refuses one.
func toolCallInfo(tu toolUseBlock) ToolCallInfo {
	return ToolCallInfo{
		ToolName:        tu.Name,
		InputSize:       len(tu.PartialJSON),
		ToolUseID:       tu.ID,
		ParentToolUseID: tu.Parent,
		Batch:           tu.Batch,
	}
}

// executingToolCallKey carries, on the context a tool's Execute receives,
// the identity of that very call: its tool_use id and the id of the call
// that made it. Two readers:
//
//   - a tool that makes calls of its own reads the id as their parent: the
//     codemode tool hands it to the server its program's calls go through,
//     so each nested call's events name the program they ran under;
//   - a tool whose Execute forwards the call elsewhere (the sandboxed
//     runner's proxy to the launcher) sends both, so the far side's events
//     about the call carry the same identity as this side's.
//
// The name is generic on purpose — whatever next runs calls on a call's
// behalf reads the same key.
type executingToolCallKey struct{}

type executingToolCall struct{ id, parent string }

func withExecutingToolCall(ctx context.Context, id, parent string) context.Context {
	return context.WithValue(ctx, executingToolCallKey{}, executingToolCall{id: id, parent: parent})
}

// executingToolUseIDFrom reads the id runToolExecution stamped. Empty when the
// Execute was not reached through the door — a test driving a tool directly —
// and then the calls it makes carry no parent and read as top-level calls.
func executingToolUseIDFrom(ctx context.Context) string {
	call, _ := ctx.Value(executingToolCallKey{}).(executingToolCall)
	return call.id
}

// ExecutingToolCallFrom reports the identity of the tool call whose Execute
// holds ctx: its tool_use id and its parent's, both empty outside the door.
// The sandboxed runner's launcher proxy reads it to put the call's identity
// on the forwarded IPC call.
func ExecutingToolCallFrom(ctx context.Context) (toolUseID, parentToolUseID string) {
	call, _ := ctx.Value(executingToolCallKey{}).(executingToolCall)
	return call.id, call.parent
}

// toolCallReport receives what a tool's Execute reports about its own run,
// beyond its output, for the call's completion event. runToolExecution arms
// a fresh one per call — the precedent is tool.RichResultSink — so a nested
// call's report can never land on its parent's event, and reads it back after
// Execute returns.
type toolCallReport struct {
	mu       sync.Mutex
	codemode *CodemodeReport
}

type toolCallReportKey struct{}

func withToolCallReport(ctx context.Context, r *toolCallReport) context.Context {
	return context.WithValue(ctx, toolCallReportKey{}, r)
}

// reportCodemode records a program's report on the call's sink. A context
// with no sink is an Execute reached outside the door, where no completion
// event will be emitted for the report to ride.
func reportCodemode(ctx context.Context, rep CodemodeReport) {
	r, _ := ctx.Value(toolCallReportKey{}).(*toolCallReport)
	if r == nil {
		return
	}
	r.mu.Lock()
	r.codemode = &rep
	r.mu.Unlock()
}

func (r *toolCallReport) codemodeReport() *CodemodeReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.codemode
}

// CodemodeReport is what one codemode program did, carried on its call's
// completion event as `codemode`. The run's events stay the only source: the
// report is a field of the event the call already emits, not a journal of its
// own.
type CodemodeReport struct {
	// Limits are the bounds the host ASKED the child to hold — the author's
	// declaration after scripthost.Limits.Clamp, which is what Host.Run
	// sends. What the kernel actually enforced can be stricter, and is
	// Confinement.
	Limits scripthost.Limits `json:"limits"`
	// Confinement is what the child reports it applied to itself
	// (scripthost.Applied): the CPU seconds and data-segment bound the
	// kernel accepted — a deployment's stricter rlimit wins, CPU rounds up
	// to a whole second — the Landlock ABI, the socket wall, whether every
	// thread is bound. Absent when no child reported one: refused before the
	// spawn, or dead before its document.
	Confinement *scripthost.Applied `json:"confinement,omitempty"`
	// EffectiveWall is the wall time the program actually had when the
	// node's own deadline left less than Limits.Wall: Host.Run derives its
	// deadline from the caller's context, which keeps the earlier of the
	// two. Measured when the program was handed to the host; absent when the
	// node's deadline was further out than the wall bound, or unset.
	EffectiveWall time.Duration `json:"effective_wall_ns,omitempty"`
	// Adjusted names each bound the clamp moved away from what the author
	// declared: raised to a floor or held under a ceiling. A bound nobody
	// declared takes its default and is not an adjustment.
	Adjusted []CodemodeLimitAdjustment `json:"adjusted,omitempty"`
	// Refusal is the scripthost refusal code a program that did not finish
	// ended on; empty on success and on a host error (a cancelled run, a
	// broken deployment), which the completion event's error names.
	Refusal string `json:"refusal,omitempty"`
	// Calls is how many requests the parent served the program against its
	// call budget, run-state requests included (scripthost.CallStats).
	Calls int64 `json:"calls"`
	// BudgetRefused is how many requests the parent refused on a spent call
	// budget. Calls the child refused itself after latching never reach the
	// parent and are not counted.
	BudgetRefused int64 `json:"budget_refused,omitempty"`
	// StateCalls is how many served requests were call.store or call.load.
	StateCalls int64 `json:"state_calls,omitempty"`
	// SourceBytes is the size of the program's source.
	SourceBytes int `json:"source_bytes"`
}

// CodemodeLimitAdjustment is one bound the clamp changed, keyed by the bound's
// wire name.
type CodemodeLimitAdjustment struct {
	Field     string `json:"field"`
	Requested int64  `json:"requested"`
	Applied   int64  `json:"applied"`
}

// newCodemodeReport builds the report of one program. want is the author's
// declaration, effective its clamped form — the pair the codemode tool holds,
// so the report reads the same Clamp Host.Run applies; effectiveWall is the
// node's deadline left when it was shorter than the wall bound, else zero.
func newCodemodeReport(want, effective scripthost.Limits, source string, resp scripthost.Response, effectiveWall time.Duration) CodemodeReport {
	return CodemodeReport{
		Limits:        effective,
		Confinement:   resp.Confinement,
		EffectiveWall: effectiveWall,
		Adjusted:      limitAdjustments(want, effective),
		Refusal:       resp.Refusal,
		Calls:         resp.Calls.Served,
		BudgetRefused: resp.Calls.BudgetRefused,
		StateCalls:    resp.Calls.StateCalls,
		SourceBytes:   len(source),
	}
}

// limitAdjustments lists, in wire-name order, the bounds the author declared
// (a positive value) that the clamp applied differently.
func limitAdjustments(want, effective scripthost.Limits) []CodemodeLimitAdjustment {
	requested, applied := limitsByWireName(want), limitsByWireName(effective)
	names := make([]string, 0, len(applied))
	for name := range applied {
		names = append(names, name)
	}
	slices.Sort(names)
	var out []CodemodeLimitAdjustment
	for _, name := range names {
		if r := requested[name]; r > 0 && r != applied[name] {
			out = append(out, CodemodeLimitAdjustment{Field: name, Requested: r, Applied: applied[name]})
		}
	}
	return out
}

// limitsByWireName reads every bound under its JSON wire name — the name the
// request to the child and the `codemode:` docs use — so the event names a
// bound the way an author meets it. Read from the struct tags rather than a
// list here, so a bound added to scripthost.Limits reaches the report without
// an edit; TestCodemodeReportNamesEveryBound holds that every field is read.
func limitsByWireName(l scripthost.Limits) map[string]int64 {
	out := map[string]int64{}
	for name, v := range byWireName(reflect.ValueOf(l)) {
		if n, ok := v.(int64); ok {
			out[name] = n
		}
	}
	return out
}

// byWireName reads a struct's scalar fields under their JSON wire names, as
// the plain values an event map holds: int64, uint64 or bool.
func byWireName(v reflect.Value) map[string]any {
	t := v.Type()
	out := make(map[string]any, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		switch f := v.Field(i); f.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			out[name] = f.Int()
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			out[name] = f.Uint()
		case reflect.Bool:
			out[name] = f.Bool()
		}
	}
	return out
}

// eventData is the report as the plain map the event carries: the redacting
// emitter deep-copies maps and passes a struct through untouched, so a struct
// here would be the one value of the event no redaction pass reads.
func (r *CodemodeReport) eventData() map[string]any {
	limits := map[string]any{}
	for name, v := range limitsByWireName(r.Limits) {
		limits[name] = v
	}
	out := map[string]any{
		"limits":       limits,
		"calls":        r.Calls,
		"source_bytes": r.SourceBytes,
	}
	if len(r.Adjusted) > 0 {
		adjusted := make([]any, 0, len(r.Adjusted))
		for _, a := range r.Adjusted {
			adjusted = append(adjusted, map[string]any{"field": a.Field, "requested": a.Requested, "applied": a.Applied})
		}
		out["adjusted"] = adjusted
	}
	if r.Confinement != nil {
		out["confinement"] = byWireName(reflect.ValueOf(*r.Confinement))
	}
	if r.EffectiveWall > 0 {
		out["effective_wall_ns"] = int64(r.EffectiveWall)
	}
	if r.Refusal != "" {
		out["refusal"] = r.Refusal
	}
	if r.BudgetRefused > 0 {
		out["budget_refused"] = r.BudgetRefused
	}
	if r.StateCalls > 0 {
		out["state_calls"] = r.StateCalls
	}
	return out
}

// batchEventData is a batch slot as the plain map the event carries.
func batchEventData(slot *scripthost.BatchSlot) map[string]any {
	return map[string]any{"first": slot.First, "size": slot.Size, "index": slot.Index}
}

// stampToolLineage writes a call's parent and batch slot on its event data.
// Both keys are absent on a call the model made itself: a reader groups by
// the parent's PRESENCE, so an empty string would be a key that lies.
func stampToolLineage(data map[string]any, parent string, batch *scripthost.BatchSlot) {
	if parent != "" {
		data["parent_tool_use_id"] = parent
	}
	if batch != nil {
		data["batch"] = batchEventData(batch)
	}
}
