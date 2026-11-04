package model

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// executeDelegate is the ONE seam a task crosses into a backend, and the
// model boundary of the secret guard (ADR-146, axis 1a): the text a model is
// about to read — the rendered system and user prompts, the user content
// blocks, the preset focus — goes through Guard.RedactForModel, so a known
// value that re-entered the prompt through an upstream node's output
// (`{{outputs.A.x}}`, an input an edge mapped) reaches the model as its
// placeholder, never in clear. The node crossing itself keeps the real
// values: the live output map still feeds the nodes downstream and the
// resume checkpoint (pkg/runtime/node_output.go), so a tool node of B can
// spend what A obtained — the model only reads the placeholder.
//
// Every backend.Execute of this package goes through here, whichever path
// built the task (the chain walk, its fresh-session retry, the schema
// re-ask): TestEveryBackendExecuteCrossesTheModelBoundary refuses a call
// that does not. The task is a value: the executor's own copy — what the
// session ledger, the events and the logs read — is untouched.
func (e *ClawExecutor) executeDelegate(ctx context.Context, backend delegate.Backend, task delegate.Task) (delegate.Result, error) {
	task.ReasoningEffort = e.routeReasoningEffort(task.NodeID, task.Model, task.ReasoningEffort)
	return backend.Execute(ctx, e.taskForModel(task))
}

// generationForModel is the boundary of the executor's DIRECT model calls —
// the human stand-in, the interaction stand-in, the review companion, the
// self-repair rung, the structured-output recovery — which hand
// GenerateObjectDirect a GenerationOptions instead of crossing a backend:
// its system text and every text block of its messages take the same pass
// as a task's prompts. TestEveryDirectGenerationOfTheExecutorCrossesTheModelBoundary
// refuses a GenerateObjectDirect call of the executor whose options did not
// come through here.
//
// The pass rides along as well, so the request builder these options reach
// screens whatever it composes from them — no direct generation builds
// system BLOCKS today, and the one that does will be covered rather than
// needing a second guard. The pass is idempotent on a placeholder, so the
// system text taking it twice costs a scan.
func (e *ClawExecutor) generationForModel(opts GenerationOptions) GenerationOptions {
	if e == nil || e.secretGuard == nil {
		return opts
	}
	redact := e.secretGuard.RedactForModel
	opts.ScreenModelText = redact
	opts.System = redact(opts.System)
	if len(opts.Messages) > 0 {
		msgs := make([]api.Message, len(opts.Messages))
		copy(msgs, opts.Messages)
		for i := range msgs {
			if len(msgs[i].Content) == 0 {
				continue
			}
			blocks := make([]api.ContentBlock, len(msgs[i].Content))
			copy(blocks, msgs[i].Content)
			for j := range blocks {
				blocks[j].Text = redact(blocks[j].Text)
			}
			msgs[i].Content = blocks
		}
		opts.Messages = msgs
	}
	return opts
}

// screenedInjector wraps a compaction hook so every text, tool name and
// tool input it hands the summariser that the history did not already
// carry crosses the model boundary first. A hook's turn is text the loop
// composed for itself — the workspace-memory index and autoload re-read
// at compaction — which no field pass reached and the request chokepoint
// never sees: compaction folds the turn into the summary before any
// request carries it, so a value it quoted would reach the model through
// the summary's excerpts.
//
// The hook works on its own deep copy of the history (cloneMessages), so
// nothing it writes — a text, a tool input, a nested tool_result, in place
// or through the slice's backing array — reaches the live history, nor the
// preserved turns of the compaction computed before the hook ran, which
// the loop keeps when the hook returns nil or a slice that does not
// re-compact. The wrap then hands a returned turn back as it is only when
// its Content is the very slice the copy gave that turn (same first block,
// same length) AND that Content still equals the history turn's, field for
// field. Every other turn takes the pass — a turn the hook added, marked
// or not, an input turn whose blocks the hook grew, and an input turn the
// hook rewrote in place, whose slice is the copy's own while its content
// no longer is the history's. The continuation turn an earlier compaction
// left at the head of the history carries IsInjected: it takes the pass
// too, whether the marker sits on the returned turn or on the input's, so
// a history head that was never screened does not reach the summariser in
// clear (the pass is idempotent on a placeholder, so a turn that already
// crossed costs a scan). A hook's nil stays nil (the loop's "no injection"
// branch), and a nil hook stays nil, so the loop's "no hook" branch is
// unchanged.
func screenedInjector(inject func([]api.Message) []api.Message, screen func(string) string) func([]api.Message) []api.Message {
	if inject == nil {
		return nil
	}
	type contentSlice struct {
		first *api.ContentBlock
		n     int
	}
	return func(messages []api.Message) []api.Message {
		view := cloneMessages(messages)
		own := make(map[contentSlice]int, len(view))
		for i := range view {
			if len(view[i].Content) > 0 {
				own[contentSlice{&view[i].Content[0], len(view[i].Content)}] = i
			}
		}
		modified := inject(view)
		if modified == nil {
			return nil
		}
		handed := make([]api.Message, len(modified))
		copy(handed, modified)
		for i := range handed {
			if len(handed[i].Content) == 0 {
				continue
			}
			j, fromHistory := own[contentSlice{&handed[i].Content[0], len(handed[i].Content)}]
			if fromHistory && !messages[j].IsInjected && !handed[i].IsInjected &&
				reflect.DeepEqual(handed[i].Content, messages[j].Content) {
				continue
			}
			handed[i].Content = screenedBlocks(handed[i].Content, screen)
		}
		return handed
	}
}

// cloneMessages returns a copy of messages that shares nothing a hook can
// write with them: each turn's Content, every nested tool_result block
// list, every tool input (cloneInputValue), the image source and the
// cache marker are fresh. A nil list stays nil, so an untouched copy is
// reflect.DeepEqual to its original.
func cloneMessages(messages []api.Message) []api.Message {
	if messages == nil {
		return nil
	}
	out := make([]api.Message, len(messages))
	copy(out, messages)
	for i := range out {
		out[i].Content = cloneBlocks(out[i].Content)
	}
	return out
}

// cloneBlocks is cloneMessages for one block list, nested lists included.
func cloneBlocks(blocks []api.ContentBlock) []api.ContentBlock {
	if blocks == nil {
		return nil
	}
	out := make([]api.ContentBlock, len(blocks))
	copy(out, blocks)
	for i := range out {
		if out[i].Input != nil {
			out[i].Input, _ = cloneInputValue(out[i].Input).(map[string]any)
		}
		out[i].Content = cloneBlocks(out[i].Content)
		if out[i].Source != nil {
			source := *out[i].Source
			out[i].Source = &source
		}
		if out[i].CacheControl != nil {
			marker := *out[i].CacheControl
			out[i].CacheControl = &marker
		}
	}
	return out
}

// cloneInputValue deep-copies a tool input value: the shapes a decoded
// JSON input holds (objects, arrays, scalars) and the typed lists and maps
// a hook may build. A leaf of any other type is shared — a scalar is
// immutable, and the live history's inputs are decoded JSON.
func cloneInputValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if t == nil {
			return t
		}
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = cloneInputValue(vv)
		}
		return out
	case []any:
		if t == nil {
			return t
		}
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = cloneInputValue(vv)
		}
		return out
	case []map[string]any:
		if t == nil {
			return t
		}
		out := make([]map[string]any, len(t))
		for i, vv := range t {
			out[i], _ = cloneInputValue(vv).(map[string]any)
		}
		return out
	case []string:
		return slices.Clone(t)
	case map[string]string:
		return maps.Clone(t)
	default:
		return v
	}
}

// screenedBlocks returns a copy of blocks whose every text, tool name and
// tool input, nested blocks included, took the pass; blocks itself is left
// as it was. These are what the summariser renders from a block: a text, a
// "tool_use <name>(<input>)" line, a "tool_result <name>: <inner text>"
// line.
func screenedBlocks(blocks []api.ContentBlock, screen func(string) string) []api.ContentBlock {
	out := make([]api.ContentBlock, len(blocks))
	copy(out, blocks)
	for i := range out {
		out[i].Text = screen(out[i].Text)
		if out[i].Name != "" {
			out[i].Name = screen(out[i].Name)
		}
		if out[i].Input != nil {
			out[i].Input, _ = screenedInputValue(out[i].Input, screen).(map[string]any)
		}
		if len(out[i].Content) > 0 {
			out[i].Content = screenedBlocks(out[i].Content, screen)
		}
	}
	return out
}

// screenedInputValue returns a copy of a tool input value whose every
// string took the pass, map keys included: the summariser renders the
// input whole with fmt's %v, keys and all. A leaf of another type is
// rendered as the summariser renders it and, when that rendering carries
// a value the pass replaces, handed on as its screened rendering — the
// summary then reads the same text, screened. Keys are visited in order,
// so two keys the pass maps to one string keep a deterministic value.
func screenedInputValue(v any, screen func(string) string) any {
	switch t := v.(type) {
	case string:
		return screen(t)
	case map[string]any:
		if t == nil {
			return t
		}
		out := make(map[string]any, len(t))
		for _, k := range slices.Sorted(maps.Keys(t)) {
			out[screen(k)] = screenedInputValue(t[k], screen)
		}
		return out
	case []any:
		if t == nil {
			return t
		}
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = screenedInputValue(vv, screen)
		}
		return out
	default:
		rendered := fmt.Sprintf("%v", v)
		if screened := screen(rendered); screened != rendered {
			return screened
		}
		return v
	}
}

// taskForModel returns task with every model-read text passed through the
// guard's model-boundary pass. A nil guard (redaction off) is the identity.
// The operator's answer at a resume takes it too: the record keeps the
// answer whole (the run needs it), the model reads it as every other text —
// on claude_code, which carries it in the prompt, and on claw alike.
//
// The pass covers every field delegate.Task composes into text a model
// reads, not only the two prompts: BuildSystemPrompt appends the auto-memory
// section, the calibration fragments, the skill hints and the preset focus
// AFTER this seam, and each of those carries text the run itself never
// rendered — the agent's own MEMORY.md, the author's cursor fragments, the
// workspace's SKILL.md frontmatter. TestEveryModelReadTextOfATaskCrossesTheBoundary
// reads the composers and refuses a field this pass neither redacts nor
// allowlists with its reason.
//
// A field pass cannot reach the text a backend composes for ITSELF — the
// ambient instruction files it reads under the task's work dir, the
// workspace-memory blocks a store hands it: no Task field holds them. Those
// take the SAME pass through delegate.Task.ScreenModelText, which the task
// carries from here to wherever the backend composes them (claw at the
// request it builds, pi at its system-prompt suffix). The in-container claw
// runner is the stated residual: a closure does not cross the IOTask wire
// and the container builds no guard, so the block it composes from the
// workspace there is screened by nothing (docs/secrets.md).
func (e *ClawExecutor) taskForModel(task delegate.Task) delegate.Task {
	if e == nil || e.secretGuard == nil {
		return task
	}
	redact := e.secretGuard.RedactForModel
	task.SystemPrompt = redact(task.SystemPrompt)
	task.UserPrompt = redact(task.UserPrompt)
	task.PresetFragment = redact(task.PresetFragment)
	task.ResumeAnswer = redact(task.ResumeAnswer)
	task.AutoMemoryPrompt = redact(task.AutoMemoryPrompt)
	task.ScreenModelText = redact
	if len(task.UserContent) > 0 {
		blocks := make([]delegate.ContentBlock, len(task.UserContent))
		copy(blocks, task.UserContent)
		for i := range blocks {
			blocks[i].Text = redact(blocks[i].Text)
		}
		task.UserContent = blocks
	}
	if len(task.CursorFragments) > 0 {
		frags := make([]string, len(task.CursorFragments))
		for i, frag := range task.CursorFragments {
			frags[i] = redact(frag)
		}
		task.CursorFragments = frags
	}
	// Both leaves of a skill hint: the name and the description come from
	// the workspace's own .claude/skills/ — a checkout of the target repo —
	// so neither is iterion's text. A known value can never legitimately BE
	// a library skill's name, so the replacement only ever fires on a leak.
	if len(task.SkillHints) > 0 {
		hints := make([]delegate.SkillHint, len(task.SkillHints))
		copy(hints, task.SkillHints)
		for i := range hints {
			hints[i].Name = redact(hints[i].Name)
			hints[i].Description = redact(hints[i].Description)
		}
		task.SkillHints = hints
	}
	return task
}
