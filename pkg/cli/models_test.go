package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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
	if m.ToolCall == nil || !*m.ToolCall || m.Temperature == nil || !*m.Temperature {
		t.Errorf("flags = %+v, want ToolCall+Temperature known true", m)
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

// TestRunModels_AGatewayIdLeavesItsFlagsUnknown: a gateway id no catalog
// table carries answers with NO capability flags — absent from the JSON,
// "?" in the table — and the source "unknown", never a false nobody stated
// (#2103).
func TestRunModels_AGatewayIdLeavesItsFlagsUnknown(t *testing.T) {
	t.Setenv("OPENAI_COMPATIBLE_MODELS", "")
	t.Setenv("OPENAI_COMPATIBLE_CATALOG_PROVIDER", "")
	t.Setenv("ITERION_OPENAI_COMPATIBLE_RESOLVED", "")
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputJSON}
	if err := RunModels(context.Background(), ModelsOptions{Spec: "openai_compatible/zz-unknown-gateway-model"}, p); err != nil {
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
	if m.Reasoning != nil || m.ToolCall != nil || m.Temperature != nil {
		t.Errorf("flags = %v/%v/%v, want all unknown (nil)", m.Reasoning, m.ToolCall, m.Temperature)
	}
	if m.Source != "unknown" {
		t.Errorf("Source = %q, want unknown", m.Source)
	}
	for _, key := range []string{`"reasoning"`, `"tool_call"`, `"temperature"`} {
		if strings.Contains(buf.String(), key) {
			t.Errorf("the JSON states %s for a flag nobody answered:\n%s", key, buf.String())
		}
	}
	buf.Reset()
	p = &Printer{W: &buf, Format: OutputHuman}
	if err := RunModels(context.Background(), ModelsOptions{Spec: "openai_compatible/zz-unknown-gateway-model"}, p); err != nil {
		t.Fatalf("RunModels (human): %v", err)
	}
	// Every one of the three flag cells, not merely SOME "?" in the table:
	// the columns are MODEL SOURCE CONTEXT "MAX OUT" REASON TOOLS TEMP, and
	// a cell printing "no" for a flag nobody answered is the bug #2103
	// closes.
	var row []string
	for line := range strings.Lines(buf.String()) {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "openai_compatible/zz-unknown-gateway-model" {
			row = f
			break
		}
	}
	if len(row) < 9 {
		t.Fatalf("no table row for the spec (got %v):\n%s", row, buf.String())
	}
	for i, want := range map[int]string{1: "unknown", 4: "?", 5: "?", 6: "?"} {
		if row[i] != want {
			t.Errorf("row cell %d (%s) = %q, want %q — the table must read ? for every unanswered flag:\n%s",
				i, []string{"MODEL", "SOURCE", "CONTEXT", "MAX OUT", "REASON", "TOOLS", "TEMP"}[i], row[i], want, buf.String())
		}
	}
}

// capturedStderr redirects os.Stderr for the caller's scope. The gateway
// catalog's unknown-id line goes to the process stderr through a fallback
// logger, so the stream is the only place to read it.
func capturedStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	os.Stderr = orig
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
	out, rerr := io.ReadAll(r)
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	if cerr := r.Close(); cerr != nil {
		t.Fatalf("close read side: %v", cerr)
	}
	return string(out)
}

// `iterion models` and GET /api/models share modelcatalog.Build, which
// resolves each entry TWICE — the capabilities, then the PRICE. Both roads
// must be quiet: listing ids an operator is typing says nothing and spends
// none of their per-id warning budget, so the launch that follows in the
// same process still says it. The whole command is driven here rather than
// a seam, because the second road is what a seam test misses.
func TestRunModels_AnUnknownGatewayIdIsListedWithoutAWord(t *testing.T) {
	t.Setenv(iterlog.EnvLevel, "info")
	t.Setenv("OPENAI_COMPATIBLE_MODELS", "")
	t.Setenv("OPENAI_COMPATIBLE_CATALOG_PROVIDER", "scaleway")
	t.Setenv("ITERION_OPENAI_COMPATIBLE_RESOLVED", "")
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)

	const id = "zz-listed-gateway-model"
	listed := capturedStderr(t, func() {
		for _, typed := range []string{"zz-l", "zz-li", id} {
			var buf bytes.Buffer
			p := &Printer{W: &buf, Format: OutputJSON}
			if err := RunModels(context.Background(), ModelsOptions{Spec: "openai_compatible/" + typed}, p); err != nil {
				t.Fatalf("RunModels(%q): %v", typed, err)
			}
		}
	})
	if strings.Contains(listed, "is in no table") {
		t.Errorf("listing unknown gateway ids logged:\n%s", listed)
	}

	// No reset: the listing must have left the per-id budget alone.
	launched := capturedStderr(t, func() {
		compatgw.ResolveCatalog(id, modelspecs.Default(), os.Getenv)
	})
	if n := strings.Count(launched, `id "`+id+`" is in no table`); n != 1 {
		t.Errorf("after listing it, the launch logged the unknown id %d times, want exactly 1:\n%s", n, launched)
	}
}
