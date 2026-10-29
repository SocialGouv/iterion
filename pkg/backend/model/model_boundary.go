package model

import (
	"context"

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
func (e *ClawExecutor) generationForModel(opts GenerationOptions) GenerationOptions {
	if e == nil || e.secretGuard == nil {
		return opts
	}
	redact := e.secretGuard.RedactForModel
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

// taskForModel returns task with every model-read text passed through the
// guard's model-boundary pass. A nil guard (redaction off) is the identity.
// The operator's answer at a resume takes it too: the record keeps the
// answer whole (the run needs it), the model reads it as every other text —
// on claude_code, which carries it in the prompt, and on claw alike.
func (e *ClawExecutor) taskForModel(task delegate.Task) delegate.Task {
	if e == nil || e.secretGuard == nil {
		return task
	}
	redact := e.secretGuard.RedactForModel
	task.SystemPrompt = redact(task.SystemPrompt)
	task.UserPrompt = redact(task.UserPrompt)
	task.PresetFragment = redact(task.PresetFragment)
	task.ResumeAnswer = redact(task.ResumeAnswer)
	if len(task.UserContent) > 0 {
		blocks := make([]delegate.ContentBlock, len(task.UserContent))
		copy(blocks, task.UserContent)
		for i := range blocks {
			blocks[i].Text = redact(blocks[i].Text)
		}
		task.UserContent = blocks
	}
	return task
}
