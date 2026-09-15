package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/modelcatalog"
)

func TestRunModels_JSONSingleModel(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputJSON}

	// A fake provider/model can never appear in any aggregator cache, so the
	// source is deterministically curated and no network state matters.
	err := RunModels(context.Background(), ModelsOptions{Spec: "faketestprovider/nonexistent-xyz"}, p)
	if err != nil {
		t.Fatalf("RunModels: %v", err)
	}

	var got modelcatalog.Catalog
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(got.Models) != 1 {
		t.Fatalf("got %d models, want 1", len(got.Models))
	}
	m := got.Models[0]
	if m.Spec != "faketestprovider/nonexistent-xyz" {
		t.Errorf("Spec = %q", m.Spec)
	}
	if m.Source != "curated" {
		t.Errorf("Source = %q, want curated (unknown model)", m.Source)
	}
	// Unknown provider → conservative default: tool calling + temperature.
	if !m.ToolCall || !m.Temperature {
		t.Errorf("flags = %+v, want ToolCall+Temperature", m)
	}
}

func TestRunModels_ListsKnownSetHuman(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputHuman}

	if err := RunModels(context.Background(), ModelsOptions{}, p); err != nil {
		t.Fatalf("RunModels: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Model capabilities") {
		t.Errorf("missing header:\n%s", out)
	}
	if !strings.Contains(out, "SOURCE") || !strings.Contains(out, "CONTEXT") {
		t.Errorf("missing table columns:\n%s", out)
	}
	// Every known spec should appear as a row.
	for _, spec := range []string{"anthropic/glm-5.2", "openai/gpt-5.5"} {
		if !strings.Contains(out, spec) {
			t.Errorf("known spec %q missing from output:\n%s", spec, out)
		}
	}
}

func TestRunModels_MalformedSpecIsUserInputError(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputHuman}

	err := RunModels(context.Background(), ModelsOptions{Spec: "no-slash-here"}, p)
	if err == nil {
		t.Fatal("expected error for malformed spec")
	}
	if !errors.Is(err, ErrUserInput) {
		t.Errorf("error = %v, want ErrUserInput-wrapped", err)
	}
}

// The reachability column is the point of sharing pkg/modelcatalog with the
// HTTP endpoint: `iterion models` must answer "can I use this here", not just
// "does iterion know about it".
func TestRunModels_ReportsReachability(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputHuman}

	if err := RunModels(context.Background(), ModelsOptions{}, p); err != nil {
		t.Fatalf("RunModels: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "USABLE") {
		t.Errorf("missing USABLE column:\n%s", out)
	}
}

func TestFormatPriceNeverReadsZeroAsFree(t *testing.T) {
	unknown := modelcatalog.Entry{PriceKnown: false}
	if got := formatPrice(unknown); got != "—" {
		t.Errorf("formatPrice(unknown) = %q, want —", got)
	}
	known := modelcatalog.Entry{PriceKnown: true, InputCostPerM: 15, OutputCostPerM: 75.5}
	if got := formatPrice(known); got != "$15/$75.5" {
		t.Errorf("formatPrice = %q, want $15/$75.5", got)
	}
}

func TestFormatUsableListsBackends(t *testing.T) {
	if got := formatUsable(modelcatalog.Entry{}); got != "no" {
		t.Errorf("formatUsable(unusable) = %q, want no", got)
	}
	e := modelcatalog.Entry{Usable: true, Backends: []string{"claude_code", "claw"}}
	if got := formatUsable(e); got != "claude_code,claw" {
		t.Errorf("formatUsable = %q", got)
	}
}

func TestFormatContextWindow(t *testing.T) {
	cases := map[int]string{
		0:         "—",
		-1:        "—",
		1_000_000: "1M",
		200_000:   "200K",
		400_000:   "400K",
		4096:      "4096",
	}
	for in, want := range cases {
		if got := formatContextWindow(in); got != want {
			t.Errorf("formatContextWindow(%d) = %q, want %q", in, got, want)
		}
	}
}
