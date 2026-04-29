package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

const (
	askUserMaxOptionLen = 200
	askUserMaxOptions   = 32
)

// Option is a single selectable answer surfaced to the user.
type Option struct {
	ID    string
	Label string
}

// Question describes a structured question put to the user.
type Question struct {
	Prompt        string
	Options       []Option
	AllowFreeText bool
}

// Answer carries the user's response. Exactly one of OptionID / FreeText is
// populated for a successful answer; both empty indicates a skip.
type Answer struct {
	OptionID string
	FreeText string
	Skipped  bool
}

// Asker pulls an answer from the user (or a batch source).
type Asker interface {
	Ask(ctx context.Context, q Question) (Answer, error)
}

// ErrNoAsker is returned when ask_user runs with no Asker wired in a
// non-interactive context.
var ErrNoAsker = fmt.Errorf("ask_user: no interactive Asker configured (non-interactive mode)")

// StdinAsker reads the answer from os.Stdin and writes the prompt to os.Stdout.
type StdinAsker struct {
	In  io.Reader
	Out io.Writer
}

// NewStdinAsker returns an Asker bound to stdio.
func NewStdinAsker() *StdinAsker {
	return &StdinAsker{In: os.Stdin, Out: os.Stdout}
}

// Ask renders the question on Out, reads a line on In, and resolves it to an
// Option (by index or label) or to free text.
func (s *StdinAsker) Ask(ctx context.Context, q Question) (Answer, error) {
	in := s.In
	if in == nil {
		in = os.Stdin
	}
	out := s.Out
	if out == nil {
		out = os.Stdout
	}

	if _, err := fmt.Fprintf(out, "%s\n", q.Prompt); err != nil {
		return Answer{}, err
	}
	for i, opt := range q.Options {
		if _, err := fmt.Fprintf(out, "  %d) %s\n", i+1, opt.Label); err != nil {
			return Answer{}, err
		}
	}
	hint := "Enter choice"
	if q.AllowFreeText && len(q.Options) > 0 {
		hint = "Enter choice or free text"
	} else if q.AllowFreeText {
		hint = "Enter response"
	}
	if _, err := fmt.Fprintf(out, "%s: ", hint); err != nil {
		return Answer{}, err
	}

	type lineResult struct {
		text string
		err  error
	}
	ch := make(chan lineResult, 1)
	go func() {
		r := bufio.NewReader(in)
		line, err := r.ReadString('\n')
		ch <- lineResult{text: line, err: err}
	}()

	select {
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	case res := <-ch:
		if res.err != nil && res.err != io.EOF {
			return Answer{}, res.err
		}
		return resolveAnswer(strings.TrimRight(res.text, "\r\n"), q), nil
	}
}

// ProgrammaticAsker resolves answers via a caller-supplied function. Used for
// SDK / batch / test contexts where there is no human at a terminal.
type ProgrammaticAsker struct {
	Handler func(ctx context.Context, q Question) (Answer, error)
}

// Ask delegates to Handler; nil handler returns ErrNoAsker.
func (p *ProgrammaticAsker) Ask(ctx context.Context, q Question) (Answer, error) {
	if p == nil || p.Handler == nil {
		return Answer{}, ErrNoAsker
	}
	return p.Handler(ctx, q)
}

// TUIAsker bridges to a TUI through a delivery callback. The TUI layer of
// claw-code-go itself does not use this — it consumes the runtime's
// TurnEventAskUser channel directly — but external TUIs that embed the
// SDK can wire this implementation.
type TUIAsker struct {
	Deliver func(ctx context.Context, q Question) (Answer, error)
}

// Ask delegates to Deliver; nil deliver returns ErrNoAsker.
func (t *TUIAsker) Ask(ctx context.Context, q Question) (Answer, error) {
	if t == nil || t.Deliver == nil {
		return Answer{}, ErrNoAsker
	}
	return t.Deliver(ctx, q)
}

// AskUserQuestionTool returns the tool definition for ask_user.
func AskUserQuestionTool() api.Tool {
	return api.Tool{
		Name:        "ask_user",
		Description: "Pause the current task and ask the user a clarifying question. Optional `options` present the user with selectable choices (numbered); when `allow_free_text` is true the user may also type a free response.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"question": {
					Type:        "string",
					Description: "The question to ask the user.",
				},
				"options": {
					Type:        "array",
					Description: "Optional list of selectable answers. Each option must have an id and a label.",
					Items: &api.Property{
						Type: "object",
						Properties: map[string]api.Property{
							"id":    {Type: "string", Description: "Stable identifier returned to the model."},
							"label": {Type: "string", Description: "Human-readable text shown to the user."},
						},
						Required: []string{"id", "label"},
					},
				},
				"allow_free_text": {
					Type:        "boolean",
					Description: "When true (default if no options are provided), the user may type a free-text response instead of selecting an option.",
				},
			},
			Required: []string{"question"},
		},
	}
}

// AskUserInput pulls the (legacy) plain-text question from a tool input map.
// Kept for callers that only need the question string; new code should use
// ParseAskUserInput for the structured form.
func AskUserInput(input map[string]any) (string, bool) {
	q, ok := input["question"].(string)
	return q, ok && q != ""
}

// ParseAskUserInput extracts a Question from a tool input map.
func ParseAskUserInput(input map[string]any) (Question, error) {
	prompt, _ := input["question"].(string)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return Question{}, fmt.Errorf("ask_user: 'question' is required")
	}
	q := Question{Prompt: prompt}

	if raw, ok := input["options"]; ok && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return Question{}, fmt.Errorf("ask_user: 'options' must be an array")
		}
		if len(list) > askUserMaxOptions {
			return Question{}, fmt.Errorf("ask_user: too many options (max %d)", askUserMaxOptions)
		}
		for i, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				return Question{}, fmt.Errorf("ask_user: option %d must be an object", i)
			}
			id, _ := m["id"].(string)
			label, _ := m["label"].(string)
			id = strings.TrimSpace(id)
			label = strings.TrimSpace(label)
			if id == "" || label == "" {
				return Question{}, fmt.Errorf("ask_user: option %d requires non-empty id and label", i)
			}
			if len(label) > askUserMaxOptionLen {
				label = label[:askUserMaxOptionLen]
			}
			q.Options = append(q.Options, Option{ID: id, Label: label})
		}
	}

	if v, ok := input["allow_free_text"].(bool); ok {
		q.AllowFreeText = v
	} else if len(q.Options) == 0 {
		q.AllowFreeText = true
	}

	return q, nil
}

// AskUserFallback returns a tool_result block for non-interactive contexts.
// Preserved for the legacy code path.
func AskUserFallback(question string) api.ContentBlock {
	return api.ContentBlock{
		Type:    "tool_result",
		Content: []api.ContentBlock{{Type: "text", Text: "[ask_user is not available in non-interactive mode. Question was: " + question + "]"}},
		IsError: false,
	}
}

// ExecuteAskUser invokes the supplied Asker and returns a string suitable for
// embedding in a tool_result content block. A nil Asker returns ErrNoAsker so
// callers can decide whether to fall back to a placeholder.
func ExecuteAskUser(ctx context.Context, asker Asker, input map[string]any) (string, error) {
	if asker == nil {
		return "", ErrNoAsker
	}
	q, err := ParseAskUserInput(input)
	if err != nil {
		return "", err
	}
	ans, err := asker.Ask(ctx, q)
	if err != nil {
		return "", err
	}
	return formatAnswer(q, ans), nil
}

func formatAnswer(q Question, ans Answer) string {
	if ans.Skipped {
		return "[skipped]"
	}
	if ans.OptionID != "" {
		for _, opt := range q.Options {
			if opt.ID == ans.OptionID {
				return fmt.Sprintf("%s: %s", ans.OptionID, opt.Label)
			}
		}
		return ans.OptionID
	}
	return ans.FreeText
}

func resolveAnswer(line string, q Question) Answer {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return Answer{Skipped: true}
	}
	if n, err := strconv.Atoi(trimmed); err == nil && n >= 1 && n <= len(q.Options) {
		return Answer{OptionID: q.Options[n-1].ID}
	}
	for _, opt := range q.Options {
		if strings.EqualFold(trimmed, opt.ID) || strings.EqualFold(trimmed, opt.Label) {
			return Answer{OptionID: opt.ID}
		}
	}
	if q.AllowFreeText {
		return Answer{FreeText: trimmed}
	}
	return Answer{FreeText: trimmed}
}
