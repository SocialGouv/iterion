// Package hooks is the public façade over the internal lifecycle
// hooks runner. Hosts build a *Runner, register Go callbacks against
// lifecycle events (PreToolUse, PostToolUse, UserPromptSubmit,
// PreCompact, PostCompact, Stop), and hand the runner to the
// conversation loop / generation engine.
//
// See the internal hooks package for design rationale: zero-cost
// when nil, sequential first-decision-wins dispatch, panic recovery.
package hooks

import (
	"io"

	intl "github.com/SocialGouv/claw-code-go/internal/hooks"
)

type Event = intl.Event

const (
	PreToolUse         = intl.PreToolUse
	PostToolUse        = intl.PostToolUse
	PostToolUseFailure = intl.PostToolUseFailure
	UserPromptSubmit   = intl.UserPromptSubmit
	PreCompact         = intl.PreCompact
	PostCompact        = intl.PostCompact
	Stop               = intl.Stop
)

type Context = intl.Context
type ActionKind = intl.ActionKind

const (
	ActionContinue = intl.ActionContinue
	ActionModify   = intl.ActionModify
	ActionBlock    = intl.ActionBlock
)

type Decision = intl.Decision
type Handler = intl.Handler
type Option = intl.Option
type Runner = intl.Runner

func NewRunner(opts ...Option) *Runner { return intl.NewRunner(opts...) }

// WithLogger sets the writer used to log handler errors. Default:
// os.Stderr. Pass io.Discard to silence the runner.
func WithLogger(w io.Writer) Option { return intl.WithLogger(w) }
