package model

import (
	"context"
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

// screenedInjector wraps a compaction hook so every text and tool name it
// hands the summariser that the history did not already carry crosses the
// model boundary first. A hook's turn is text the loop composed for itself — the
// workspace-memory index and autoload re-read at compaction — which no
// field pass reached and the request chokepoint never sees: compaction
// folds the turn into the summary before any request carries it, so a
// value it quoted would reach the model through the summary's excerpts.
//
// The wrap keys on what the history held BEFORE the hook ran, not on the
// IsInjected marker the hook is asked to set: it snapshots every input
// turn's texts (each block's, and each nested tool_result block's, which
// the summariser reads too) and its marker, then hands a returned turn
// back as it is only when its Content is the very slice an input message
// carries (same first block, same length) AND its texts still match that
// turn's snapshot. Every other turn takes the pass — a turn the hook
// added, marked or not, an input turn whose blocks the hook grew, and an
// input turn the hook rewrote in place, whose slice is the history's own
// while its text no longer is. The continuation turn an earlier compaction
// left at the head of the history carries IsInjected: it takes the pass
// too, whether the marker sits on the returned turn or on the input's, so
// a history head that was never screened does not reach the summariser in
// clear (the pass is idempotent on a placeholder, so a turn that already
// crossed costs a scan). The result is a fresh slice whose screened turns
// are copies: a hook that returns the input's backing array leaves the
// caller's history untouched. A hook's nil stays nil (the loop's "no
// injection" branch), and a nil hook stays nil, so the loop's "no hook"
// branch is unchanged.
func screenedInjector(inject func([]api.Message) []api.Message, screen func(string) string) func([]api.Message) []api.Message {
	if inject == nil {
		return nil
	}
	type contentSlice struct {
		first *api.ContentBlock
		n     int
	}
	type snapshot struct {
		texts    []string
		injected bool
	}
	return func(messages []api.Message) []api.Message {
		own := make(map[contentSlice]snapshot, len(messages))
		for i := range messages {
			if len(messages[i].Content) == 0 {
				continue
			}
			key := contentSlice{&messages[i].Content[0], len(messages[i].Content)}
			prior, seen := own[key]
			own[key] = snapshot{
				texts:    appendBlockTexts(nil, messages[i].Content),
				injected: messages[i].IsInjected || (seen && prior.injected),
			}
		}
		modified := inject(messages)
		if modified == nil {
			return nil
		}
		handed := make([]api.Message, len(modified))
		copy(handed, modified)
		var current []string
		for i := range handed {
			if len(handed[i].Content) == 0 {
				continue
			}
			prior, fromHistory := own[contentSlice{&handed[i].Content[0], len(handed[i].Content)}]
			if fromHistory && !prior.injected && !handed[i].IsInjected {
				current = appendBlockTexts(current[:0], handed[i].Content)
				if slices.Equal(current, prior.texts) {
					continue
				}
			}
			handed[i].Content = screenedBlocks(handed[i].Content, screen)
		}
		return handed
	}
}

// appendBlockTexts appends the text of every block and of every block
// nested under it (a tool_result's content) to dst, depth first.
// appendBlockTexts collects what the summariser transcript reads from a
// block list: each text, each nested text, and each tool_use Name (the
// transcript renders it as "[Tool call: <name>]"). The snapshot and the
// compare both read through it, so a Name rewritten in place is a changed
// turn exactly like a rewritten text.
func appendBlockTexts(dst []string, blocks []api.ContentBlock) []string {
	for i := range blocks {
		dst = append(dst, blocks[i].Text)
		if blocks[i].Name != "" {
			dst = append(dst, blocks[i].Name)
		}
		dst = appendBlockTexts(dst, blocks[i].Content)
	}
	return dst
}

// screenedBlocks returns a copy of blocks whose every text and tool name,
// nested ones included, took the pass; blocks itself is left as it was.
func screenedBlocks(blocks []api.ContentBlock, screen func(string) string) []api.ContentBlock {
	out := make([]api.ContentBlock, len(blocks))
	copy(out, blocks)
	for i := range out {
		out[i].Text = screen(out[i].Text)
		if out[i].Name != "" {
			out[i].Name = screen(out[i].Name)
		}
		if len(out[i].Content) > 0 {
			out[i].Content = screenedBlocks(out[i].Content, screen)
		}
	}
	return out
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
