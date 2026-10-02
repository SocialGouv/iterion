package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// `iterion inspect --json` dumps the whole run record — one run, or every
// run of the store — and a run launched on a pull request holds its publish
// grant in its inputs and its checkpoint vars. The dump says a grant was
// minted without printing it; the record keeps it for the runner.
func TestInspectJSON_NeverPrintsThePublishGrant(t *testing.T) {
	const sentinel = "sentinel-publish-token-1997"
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "r-grant", "review", map[string]any{
		"pr_url":                   "https://github.com/o/r/pull/7",
		store.ForgePublishTokenVar: sentinel,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseRun(ctx, "r-grant", &store.Checkpoint{
		NodeID: "approve", InteractionID: "I1",
		Vars: map[string]any{store.ForgePublishTokenVar: sentinel},
	}); err != nil {
		t.Fatal(err)
	}

	inspect := func(t *testing.T, opts InspectOptions) string {
		t.Helper()
		var buf bytes.Buffer
		if err := RunInspect(opts, &Printer{Format: OutputJSON, W: &buf}); err != nil {
			t.Fatalf("RunInspect: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, sentinel) {
			t.Fatalf("inspect --json printed the publish grant:\n%s", out)
		}
		return out
	}
	masked := func(t *testing.T, run map[string]any) {
		t.Helper()
		inputs, _ := run["inputs"].(map[string]any)
		if inputs[store.ForgePublishTokenVar] != store.RedactedLaunchVar {
			t.Errorf("inputs = %v, want the grant shown as %q", inputs, store.RedactedLaunchVar)
		}
		if inputs["pr_url"] != "https://github.com/o/r/pull/7" {
			t.Errorf("inputs lost pr_url: %v", inputs)
		}
		cp, _ := run["checkpoint"].(map[string]any)
		vars, _ := cp["vars"].(map[string]any)
		if vars[store.ForgePublishTokenVar] != store.RedactedLaunchVar {
			t.Errorf("checkpoint = %v, want its vars masked", cp)
		}
	}

	t.Run("one run", func(t *testing.T) {
		var got struct {
			Run map[string]any `json:"run"`
		}
		if err := json.Unmarshal([]byte(inspect(t, InspectOptions{RunID: "r-grant", StoreDir: dir})), &got); err != nil {
			t.Fatal(err)
		}
		masked(t, got.Run)
	})
	t.Run("the run list", func(t *testing.T) {
		var got []map[string]any
		if err := json.Unmarshal([]byte(inspect(t, InspectOptions{StoreDir: dir})), &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("listed %d runs, want 1", len(got))
		}
		masked(t, got[0])
	})

	r, err := s.LoadRun(ctx, "r-grant")
	if err != nil {
		t.Fatal(err)
	}
	if r.Inputs[store.ForgePublishTokenVar] != sentinel || r.Checkpoint.Vars[store.ForgePublishTokenVar] != sentinel {
		t.Fatal("inspect rewrote the run record's grant")
	}
}
