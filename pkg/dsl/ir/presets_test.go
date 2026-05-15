package ir

import "testing"

const presetsBaseWorkflow = `
schema empty:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent runner:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr

workflow demo:
  entry: runner
  runner -> done
`

func TestCompilePresets_Roundtrip(t *testing.T) {
	src := `vars:
  api_url: string
  debug: bool
  retries: int

presets:
  dev:
    api_url: "http://localhost:8080"
    debug: true
    retries: 1
  prod:
    api_url: "https://api.example.com"
    debug: false
    retries: 5
` + presetsBaseWorkflow

	w := mustCompile(t, src)
	if len(w.Presets) != 2 {
		t.Fatalf("expected 2 presets, got %d", len(w.Presets))
	}

	dev, ok := w.Presets["dev"]
	if !ok {
		t.Fatal("dev preset missing")
	}
	if got := dev.Values["api_url"]; got != "http://localhost:8080" {
		t.Errorf("dev.api_url = %v", got)
	}
	if got, ok := dev.Values["debug"].(bool); !ok || !got {
		t.Errorf("dev.debug = %v (%T), want true (bool)", dev.Values["debug"], dev.Values["debug"])
	}
	if got, ok := dev.Values["retries"].(int64); !ok || got != 1 {
		t.Errorf("dev.retries = %v (%T), want 1 (int64)", dev.Values["retries"], dev.Values["retries"])
	}

	prod := w.Presets["prod"]
	if got := prod.Values["api_url"]; got != "https://api.example.com" {
		t.Errorf("prod.api_url = %v", got)
	}
}

func TestCompilePresets_FloatAcceptsInt(t *testing.T) {
	src := `vars:
  ratio: float

presets:
  pick:
    ratio: 1
` + presetsBaseWorkflow

	w := mustCompile(t, src)
	got, ok := w.Presets["pick"].Values["ratio"].(float64)
	if !ok || got != 1.0 {
		t.Errorf("ratio = %v (%T), want 1.0 (float64)", w.Presets["pick"].Values["ratio"], w.Presets["pick"].Values["ratio"])
	}
}

func TestValidatePresets_UnknownVar(t *testing.T) {
	src := `vars:
  api_url: string

presets:
  dev:
    api_url: "x"
    nope: "y"
` + presetsBaseWorkflow

	r := compileFile(t, src)
	expectDiag(t, r, DiagPresetUnknownVar)
}

func TestValidatePresets_TypeMismatch(t *testing.T) {
	src := `vars:
  debug: bool

presets:
  dev:
    debug: "yes"
` + presetsBaseWorkflow

	r := compileFile(t, src)
	expectDiag(t, r, DiagPresetTypeMismatch)
}

func TestValidatePresets_DuplicatePreset(t *testing.T) {
	src := `vars:
  api_url: string

presets:
  dev:
    api_url: "x"
  dev:
    api_url: "y"
` + presetsBaseWorkflow

	r := compileFile(t, src)
	expectDiag(t, r, DiagDuplicatePreset)
}

