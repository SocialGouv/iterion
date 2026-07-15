// Package botreplay implements a record/replay golden-test framework for
// iterion bots.
//
// A golden fixture freezes one representative LLM node interaction —
// the input map the runtime fed to runtime.NodeExecutor.Execute and the
// output map the node returned — under
// testdata/bot-goldens/<bot>/<scenario>.json. The replay test
// (TestGoldens) re-validates every committed fixture against the bot's
// CURRENT declared schema and a set of quality invariants (required
// fields present and non-empty, no hallucinated assignees) on every CI
// run, at zero API cost.
//
// Record mode (build tag `goldens_record`, requires LLM credentials)
// hits the real provider through the production *model.ClawExecutor and
// rewrites the fixtures. It never compiles into the default test binary,
// so `go test ./...` stays lean and credential-free.
//
// The framework deliberately stubs at the NodeExecutor seam rather than
// the lower-level api.APIClient: Execute's (input → output) shape is
// exactly what a fixture stores, the existing e2e stubs already work at
// this seam, and *model.ClawExecutor exposes no client-injection option.
// Replay performs static verification over the committed fixture — the
// recorded output IS the stubbed LLM response — because all three wired
// bots contain human/tool nodes that make an unattended full-runtime
// replay impractical.
package botreplay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Fixture is a single recorded LLM node interaction. Fixtures are
// committed under testdata/bot-goldens/<bot>/<scenario>.json and
// re-validated by TestGoldens without hitting any LLM.
type Fixture struct {
	Bot        string            `json:"bot"`
	Scenario   string            `json:"scenario"`
	Node       string            `json:"node"`
	Backend    string            `json:"backend,omitempty"`
	Model      string            `json:"model,omitempty"`
	RecordedAt string            `json:"recorded_at,omitempty"`
	Vars       map[string]string `json:"vars,omitempty"`
	Input      map[string]any    `json:"input"`
	Output     map[string]any    `json:"output"`
}

// LoadFixture reads and decodes a fixture JSON file.
func LoadFixture(path string) (*Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Fixture
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("botreplay: decode %s: %w", path, err)
	}
	return &f, nil
}

// Save writes the fixture as indented JSON (diff-stable, trailing
// newline), creating parent directories as needed.
func (f *Fixture) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
