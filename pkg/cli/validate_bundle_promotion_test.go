package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestRunValidate_BareMainBotOfABundleSeesItsPrompts: `iterion validate
// <bundle>/main.bot` gives the verdict of `iterion validate <bundle>`. The
// bare file is promoted to its bundle by openBundleOrFile — the one
// helper run, resume, validate and doctor open a workflow through — so
// the prompts/*.md the bundle ships are in scope. The multi-file gallery
// shape, which declares no prompt in main.bot, used to validate INVALID
// (C003 twice) as a file and OK as a directory.
func TestRunValidate_BareMainBotOfABundleSeesItsPrompts(t *testing.T) {
	inTempWorkspace(t)
	p, _ := testPrinter()
	if err := BotsCreate(BotsCreateOptions{Slug: "mf", Template: "multi-file"}, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}
	for _, target := range []string{filepath.Join("bots", "mf", "main.bot"), filepath.Join("bots", "mf")} {
		jp, out := jsonPrinter()
		if err := RunValidate(target, jp); err != nil {
			t.Fatalf("validate %s: %v\n%s", target, err, out.String())
		}
		var result struct {
			Valid       bool     `json:"valid"`
			Diagnostics []any    `json:"diagnostics"`
			Bundle      string   `json:"bundle"`
			Compile     []string `json:"compile_diagnostics"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatalf("validate %s: output is not JSON: %v\n%s", target, err, out.String())
		}
		if !result.Valid {
			t.Errorf("validate %s: invalid: %v", target, result.Compile)
		}
	}
}
