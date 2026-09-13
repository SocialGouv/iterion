package botscaffold

import (
	"reflect"
	"testing"
	"testing/fstest"
)

// TestVarRefScanReadsRenderedFilesOnly: the var scan behind the form's
// missing-var refusal reads what is RENDERED against the Spec's vars —
// main.bot and the prompts — and nothing else: not a skill (mirrored
// verbatim, where a `{{vars.x}}` in prose is documentation), not a child
// workflow (which declares its own vars), not an attachment. Measured on
// an in-memory shape whose skill, child and attachment each mention a var
// main.bot never reads: the scan must not demand them.
func TestVarRefScanReadsRenderedFilesOnly(t *testing.T) {
	shape := fstest.MapFS{
		"s/main.bot.tmpl":            {Data: []byte("agent a:\n  user: u\n  system: \"{{vars.real}}\"\n")},
		"s/prompts/u.md.tmpl":        {Data: []byte("Say {{vars.also}} and vars.bare too.\n")},
		"s/prompts/deep/x.md":        {Data: []byte("{{vars.deep}}\n")},
		"s/skills/guide.md":          {Data: []byte("Use `{{vars.ghost}}` in your bot.\n")},
		"s/skills/deep/more.md.tmpl": {Data: []byte("vars.ghost2\n")},
		"s/worker.bot.tmpl":          {Data: []byte("vars:\n  child: string = \"{{vars.child}}\"\n")},
		"s/attachments/example.md":   {Data: []byte("{{vars.blob}}\n")},
		"s/manifest.yaml.tmpl":       {Data: []byte("name: {{vars.manifest}}\n")},
	}
	got := varRefsIn(shape, "s")
	want := []string{"also", "bare", "deep", "real"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("varRefsIn = %v, want %v (main.bot and prompts only)", got, want)
	}

	cases := map[string]bool{
		"main.bot.tmpl":             true,
		"main.bot":                  true,
		"prompts/mission.md.tmpl":   true,
		"prompts/deep/user.md":      true,
		"worker.bot.tmpl":           false,
		"steps/child.bot":           false,
		"skills/guide.md":           false,
		"skills/deep/guide.md.tmpl": false,
		"skills/main.bot.tmpl":      false,
		"attachments/example.md":    false,
		"manifest.yaml.tmpl":        false,
		"devbox.json":               false,
	}
	for rel, want := range cases {
		if got := scansForVarRefs(rel); got != want {
			t.Errorf("scansForVarRefs(%q) = %v, want %v", rel, got, want)
		}
	}
}
