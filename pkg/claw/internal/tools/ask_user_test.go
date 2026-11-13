package tools

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAskUserQuestionToolSchema(t *testing.T) {
	tool := AskUserQuestionTool()
	if tool.Name != "ask_user" {
		t.Fatalf("name = %q, want ask_user", tool.Name)
	}
	if _, ok := tool.InputSchema.Properties["question"]; !ok {
		t.Fatal("missing question property")
	}
	opts, ok := tool.InputSchema.Properties["options"]
	if !ok {
		t.Fatal("missing options property")
	}
	if opts.Items == nil {
		t.Fatal("options.Items must not be nil — array schemas without items break OpenAI validators")
	}
	if _, ok := opts.Items.Properties["id"]; !ok {
		t.Fatal("options.Items.Properties.id missing")
	}
	if _, ok := opts.Items.Properties["label"]; !ok {
		t.Fatal("options.Items.Properties.label missing")
	}
}

func TestParseAskUserInput(t *testing.T) {
	t.Run("question only defaults to free text", func(t *testing.T) {
		q, err := ParseAskUserInput(map[string]any{"question": "what now?"})
		if err != nil {
			t.Fatal(err)
		}
		if !q.AllowFreeText {
			t.Fatal("free text should default to true when no options provided")
		}
	})

	t.Run("options parsed", func(t *testing.T) {
		q, err := ParseAskUserInput(map[string]any{
			"question": "pick one",
			"options": []any{
				map[string]any{"id": "yes", "label": "Yes"},
				map[string]any{"id": "no", "label": "No"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(q.Options) != 2 || q.Options[0].ID != "yes" || q.Options[1].Label != "No" {
			t.Fatalf("options not parsed: %+v", q.Options)
		}
		if q.AllowFreeText {
			t.Fatal("with options and no allow_free_text override, default should be false")
		}
	})

	t.Run("allow_free_text override", func(t *testing.T) {
		q, err := ParseAskUserInput(map[string]any{
			"question":        "pick or speak",
			"options":         []any{map[string]any{"id": "a", "label": "A"}},
			"allow_free_text": true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !q.AllowFreeText {
			t.Fatal("allow_free_text override ignored")
		}
	})

	t.Run("missing question rejected", func(t *testing.T) {
		if _, err := ParseAskUserInput(map[string]any{}); err == nil {
			t.Fatal("expected error for missing question")
		}
	})

	t.Run("malformed options rejected", func(t *testing.T) {
		if _, err := ParseAskUserInput(map[string]any{
			"question": "q",
			"options":  []any{map[string]any{"id": "", "label": "x"}},
		}); err == nil {
			t.Fatal("expected error for empty option id")
		}
		if _, err := ParseAskUserInput(map[string]any{
			"question": "q",
			"options":  "not an array",
		}); err == nil {
			t.Fatal("expected error for non-array options")
		}
	})
}

func TestStdinAskerNumericChoice(t *testing.T) {
	in := strings.NewReader("2\n")
	var out bytes.Buffer
	asker := &StdinAsker{In: in, Out: &out}

	q := Question{
		Prompt: "Pick:",
		Options: []Option{
			{ID: "a", Label: "Apple"},
			{ID: "b", Label: "Banana"},
		},
	}
	ans, err := asker.Ask(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if ans.OptionID != "b" {
		t.Fatalf("OptionID = %q, want b", ans.OptionID)
	}
	if !strings.Contains(out.String(), "1) Apple") || !strings.Contains(out.String(), "2) Banana") {
		t.Fatalf("options not rendered: %q", out.String())
	}
}

func TestStdinAskerLabelMatch(t *testing.T) {
	in := strings.NewReader("Banana\n")
	asker := &StdinAsker{In: in, Out: &bytes.Buffer{}}
	q := Question{
		Prompt:  "Pick:",
		Options: []Option{{ID: "a", Label: "Apple"}, {ID: "b", Label: "Banana"}},
	}
	ans, err := asker.Ask(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if ans.OptionID != "b" {
		t.Fatalf("OptionID = %q, want b", ans.OptionID)
	}
}

func TestStdinAskerFreeText(t *testing.T) {
	in := strings.NewReader("a custom answer\n")
	asker := &StdinAsker{In: in, Out: &bytes.Buffer{}}
	q := Question{Prompt: "Speak:", AllowFreeText: true}
	ans, err := asker.Ask(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if ans.FreeText != "a custom answer" {
		t.Fatalf("FreeText = %q", ans.FreeText)
	}
}

func TestStdinAskerSkip(t *testing.T) {
	in := strings.NewReader("\n")
	asker := &StdinAsker{In: in, Out: &bytes.Buffer{}}
	ans, err := asker.Ask(context.Background(), Question{Prompt: "?", AllowFreeText: true})
	if err != nil {
		t.Fatal(err)
	}
	if !ans.Skipped {
		t.Fatal("empty input should produce Skipped=true")
	}
}

func TestStdinAskerContextCancel(t *testing.T) {
	pr, _ := blockingReader()
	asker := &StdinAsker{In: pr, Out: &bytes.Buffer{}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := asker.Ask(ctx, Question{Prompt: "?", AllowFreeText: true})
	if err == nil {
		t.Fatal("expected context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context error", err)
	}
}

func TestProgrammaticAsker(t *testing.T) {
	asker := &ProgrammaticAsker{
		Handler: func(_ context.Context, q Question) (Answer, error) {
			if q.Prompt != "ping" {
				t.Fatalf("handler saw prompt %q", q.Prompt)
			}
			return Answer{OptionID: "ok"}, nil
		},
	}
	ans, err := asker.Ask(context.Background(), Question{Prompt: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	if ans.OptionID != "ok" {
		t.Fatalf("OptionID = %q", ans.OptionID)
	}
}

func TestProgrammaticAskerNilHandler(t *testing.T) {
	var p *ProgrammaticAsker
	if _, err := p.Ask(context.Background(), Question{Prompt: "x"}); !errors.Is(err, ErrNoAsker) {
		t.Fatalf("got %v, want ErrNoAsker", err)
	}
	p = &ProgrammaticAsker{}
	if _, err := p.Ask(context.Background(), Question{Prompt: "x"}); !errors.Is(err, ErrNoAsker) {
		t.Fatalf("got %v, want ErrNoAsker", err)
	}
}

func TestExecuteAskUserNilAsker(t *testing.T) {
	_, err := ExecuteAskUser(context.Background(), nil, map[string]any{"question": "?"})
	if !errors.Is(err, ErrNoAsker) {
		t.Fatalf("got %v, want ErrNoAsker", err)
	}
}

func TestExecuteAskUserHappy(t *testing.T) {
	asker := &ProgrammaticAsker{
		Handler: func(_ context.Context, q Question) (Answer, error) {
			return Answer{OptionID: q.Options[0].ID}, nil
		},
	}
	out, err := ExecuteAskUser(context.Background(), asker, map[string]any{
		"question": "pick",
		"options": []any{
			map[string]any{"id": "first", "label": "First"},
			map[string]any{"id": "second", "label": "Second"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "first") || !strings.Contains(out, "First") {
		t.Fatalf("answer text %q does not contain id+label", out)
	}
}

func TestFormatAnswer(t *testing.T) {
	q := Question{Options: []Option{{ID: "yes", Label: "Yes"}}}

	if got := formatAnswer(q, Answer{Skipped: true}); got != "[skipped]" {
		t.Fatalf("skipped = %q", got)
	}
	if got := formatAnswer(q, Answer{OptionID: "yes"}); !strings.Contains(got, "Yes") {
		t.Fatalf("matched option = %q", got)
	}
	if got := formatAnswer(q, Answer{OptionID: "unknown"}); got != "unknown" {
		t.Fatalf("unmatched id = %q", got)
	}
	if got := formatAnswer(q, Answer{FreeText: "hi"}); got != "hi" {
		t.Fatalf("free text = %q", got)
	}
}

// blockingReader returns a reader that blocks on Read until the writer
// is closed, allowing us to exercise the context-cancel path.
func blockingReader() (*pipeReader, *pipeWriter) {
	r := &pipeReader{ch: make(chan struct{})}
	return r, &pipeWriter{r: r}
}

type pipeReader struct{ ch chan struct{} }

func (p *pipeReader) Read(_ []byte) (int, error) {
	<-p.ch
	return 0, errEOF
}

type pipeWriter struct{ r *pipeReader }

func (p *pipeWriter) Close() error { close(p.r.ch); return nil }

var errEOF = errors.New("eof")
