package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// publishTokenSentinel stands in for a minted publish grant: a value no
// real token can take, so finding it in an output proves a leak.
const publishTokenSentinel = "sentinel-publish-token-1997"

// Bots declare the grant under this exact name in their vars: block; the
// constant is the wire contract between the server that mints it and every
// .bot that reads it, so it cannot drift silently.
func TestForgePublishTokenVar_IsTheNameBotsDeclare(t *testing.T) {
	if ForgePublishTokenVar != "forge_publish_token" {
		t.Fatalf("ForgePublishTokenVar = %q, want forge_publish_token — every bot declaring the grant would stop receiving it", ForgePublishTokenVar)
	}
	if !IsServerMintedSecretVar(ForgePublishTokenVar) {
		t.Fatal("the publish grant is not a server-minted secret var — no read surface masks it")
	}
	// The endpoints minted beside the grant are not credentials: the fork
	// dialog and the run view show them as they are.
	for _, name := range []string{"forge_publish_url", "forge_pr_state_url", "forge_delivery_preflight_url", "pr_url"} {
		if IsServerMintedSecretVar(name) {
			t.Errorf("%q is masked as a secret, but it is an endpoint the operator needs to read", name)
		}
	}
}

// A bot declares the grant with an empty default: a run launched without one
// carries the empty var, and a mask there would claim a grant was minted.
func TestRedactLaunchVars_LeavesAnEmptyGrantAsIs(t *testing.T) {
	in := map[string]any{"mode": "fast", ForgePublishTokenVar: ""}
	if out := RedactLaunchVars(in); out[ForgePublishTokenVar] != "" {
		t.Fatalf("an empty grant var reads %v, want it empty", out[ForgePublishTokenVar])
	}
	if HasServerMintedSecret(in) {
		t.Fatal("HasServerMintedSecret = true for an empty grant var")
	}
}

// A read surface hands out a masked copy; the record it was read from keeps
// the live grant, because the runner, a resume and a fork read it there.
func TestRedactLaunchVars_MasksTheGrantAndNeverTouchesTheRecord(t *testing.T) {
	record := map[string]any{
		"pr_url":             "https://github.com/o/r/pull/7",
		ForgePublishTokenVar: publishTokenSentinel,
		"attempts":           3,
	}
	out := RedactLaunchVars(record)

	if got := out[ForgePublishTokenVar]; got != RedactedLaunchVar {
		t.Fatalf("masked %s = %v, want %q", ForgePublishTokenVar, got, RedactedLaunchVar)
	}
	if out["pr_url"] != "https://github.com/o/r/pull/7" || out["attempts"] != 3 {
		t.Fatalf("masking changed the other vars: %v", out)
	}
	if record[ForgePublishTokenVar] != publishTokenSentinel {
		t.Fatalf("the record's grant became %v — the runner reading this map would lose it", record[ForgePublishTokenVar])
	}
	// A copy, not an alias: a caller that decorates the masked map must not
	// write into the record.
	out["decorated"] = true
	if _, ok := record["decorated"]; ok {
		t.Fatal("the masked map aliases the record")
	}
}

// Nothing to mask costs nothing: the map comes back as it is, never copied.
func TestRedactLaunchVars_ReturnsTheSameMapWhenNothingIsSecret(t *testing.T) {
	vars := map[string]any{"pr_url": "https://github.com/o/r/pull/7"}
	if out := RedactLaunchVars(vars); reflect.ValueOf(out).Pointer() != reflect.ValueOf(vars).Pointer() {
		t.Fatal("a map without a server-minted var was copied")
	}
	if out := RedactLaunchVars(nil); out != nil {
		t.Fatalf("nil vars = %v, want nil", out)
	}
}

// A write path a client drives drops the key whatever its value — the mask
// the client was shown or a stale token — for the two map shapes that carry
// launch vars: run inputs (any) and board-card bot args (string).
func TestDropServerMintedVars_RemovesTheKeyAndNeverTouchesTheInput(t *testing.T) {
	t.Run("bot args", func(t *testing.T) {
		for _, value := range []string{RedactedLaunchVar, publishTokenSentinel} {
			in := map[string]string{"pr_url": "https://github.com/o/r/pull/7", ForgePublishTokenVar: value}
			out := DropServerMintedVars(in)
			if _, ok := out[ForgePublishTokenVar]; ok {
				t.Fatalf("value %q: the grant key survived: %v", value, out)
			}
			if out["pr_url"] != "https://github.com/o/r/pull/7" || len(out) != 1 {
				t.Fatalf("value %q: the other args changed: %v", value, out)
			}
			if in[ForgePublishTokenVar] != value {
				t.Fatalf("value %q: the caller's map lost the key", value)
			}
		}
	})
	t.Run("run inputs", func(t *testing.T) {
		in := map[string]any{"mode": "fast", ForgePublishTokenVar: RedactedLaunchVar}
		out := DropServerMintedVars(in)
		if _, ok := out[ForgePublishTokenVar]; ok {
			t.Fatalf("the grant key survived: %v", out)
		}
		if out["mode"] != "fast" || len(out) != 1 {
			t.Fatalf("the other inputs changed: %v", out)
		}
		if in[ForgePublishTokenVar] != RedactedLaunchVar {
			t.Fatal("the caller's map lost the key")
		}
	})
	t.Run("the endpoints go with the grant", func(t *testing.T) {
		in := map[string]string{"pr_url": "https://github.com/o/r/pull/7"}
		for _, name := range ServerMintedLaunchVars {
			in[name] = "https://collector.example/" + name
		}
		out := DropServerMintedVars(in)
		if len(out) != 1 || out["pr_url"] == "" {
			t.Fatalf("DropServerMintedVars = %v, want only pr_url: an endpoint a client sets would receive the run's grant", out)
		}
	})
	t.Run("nothing to drop returns the same map", func(t *testing.T) {
		in := map[string]string{"pr_url": "https://github.com/o/r/pull/7"}
		if out := DropServerMintedVars(in); reflect.ValueOf(out).Pointer() != reflect.ValueOf(in).Pointer() {
			t.Fatal("a map without a server-minted var was copied")
		}
		if out := DropServerMintedVars[string](nil); out != nil {
			t.Fatalf("nil args = %v, want nil", out)
		}
	})
}

// The checkpoint is what the engine resumes from: the read copy masks its
// vars, the checkpoint itself keeps the grant.
func TestRedactCheckpoint_MasksACopyAndKeepsTheResumeState(t *testing.T) {
	cp := &Checkpoint{
		NodeID:        "publish",
		InteractionID: "int-1",
		Vars:          map[string]any{ForgePublishTokenVar: publishTokenSentinel, "base_ref": "main"},
	}
	out := RedactCheckpoint(cp)
	if out == cp {
		t.Fatal("the checkpoint was masked in place")
	}
	if out.Vars[ForgePublishTokenVar] != RedactedLaunchVar || out.Vars["base_ref"] != "main" {
		t.Fatalf("masked vars = %v", out.Vars)
	}
	if out.NodeID != "publish" || out.InteractionID != "int-1" {
		t.Fatalf("the copy lost the checkpoint's other fields: %+v", out)
	}
	if cp.Vars[ForgePublishTokenVar] != publishTokenSentinel {
		t.Fatalf("the resume state's grant became %v", cp.Vars[ForgePublishTokenVar])
	}

	plain := &Checkpoint{NodeID: "n", Vars: map[string]any{"base_ref": "main"}}
	if got := RedactCheckpoint(plain); got != plain {
		t.Fatal("a checkpoint without a server-minted var was copied")
	}
	if got := RedactCheckpoint(nil); got != nil {
		t.Fatalf("nil checkpoint = %+v, want nil", got)
	}
}

// A whole-record dump masks both places the grant lives, whichever of the
// two holds it, and leaves the record it was given intact.
func TestRedactRunForOutput_MasksInputsAndCheckpointOnACopy(t *testing.T) {
	cases := []struct {
		name       string
		inputs     map[string]any
		checkpoint map[string]any
	}{
		{"both", map[string]any{ForgePublishTokenVar: publishTokenSentinel}, map[string]any{ForgePublishTokenVar: publishTokenSentinel}},
		{"inputs only", map[string]any{ForgePublishTokenVar: publishTokenSentinel}, map[string]any{"base_ref": "main"}},
		{"checkpoint only", map[string]any{"pr_url": "https://github.com/o/r/pull/7"}, map[string]any{ForgePublishTokenVar: publishTokenSentinel}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Run{ID: "run-1", WorkflowName: "review", Inputs: tc.inputs, Checkpoint: &Checkpoint{NodeID: "n", Vars: tc.checkpoint}}
			out := RedactRunForOutput(r)
			body, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), publishTokenSentinel) {
				t.Fatalf("the dump carries the grant: %s", body)
			}
			if out == r {
				t.Fatal("the record itself came back masked — the runner reading it would lose the grant")
			}
			if out.ID != "run-1" || out.WorkflowName != "review" {
				t.Fatalf("the copy lost the run's fields: %+v", out)
			}
			if _, had := tc.inputs[ForgePublishTokenVar]; had && r.Inputs[ForgePublishTokenVar] != publishTokenSentinel {
				t.Fatalf("the record's inputs lost the grant: %v", r.Inputs)
			}
			if _, had := tc.checkpoint[ForgePublishTokenVar]; had && r.Checkpoint.Vars[ForgePublishTokenVar] != publishTokenSentinel {
				t.Fatalf("the record's checkpoint lost the grant: %v", r.Checkpoint.Vars)
			}
		})
	}

	t.Run("nothing to mask returns the same run", func(t *testing.T) {
		r := &Run{ID: "run-2", Inputs: map[string]any{"pr_url": "x"}, Checkpoint: &Checkpoint{Vars: map[string]any{"base_ref": "main"}}}
		if got := RedactRunForOutput(r); got != r {
			t.Fatal("a run without a server-minted var was copied")
		}
		if got := RedactRunForOutput(nil); got != nil {
			t.Fatalf("nil run = %+v, want nil", got)
		}
	})
}

// An empty value grants nothing and names nowhere: the only thing it can do
// is withdraw, so it is the client's to send. A value is the server's to mint.
func TestDropServerMintedVarsKeepsAnExplicitWithdrawal(t *testing.T) {
	withdrawn := map[string]any{
		ForgePublishTokenVar: "",
		ForgePublishURLVar:   "   ",
		"reviewer":           "alice",
	}
	got := DropServerMintedVars(withdrawn)
	for _, name := range []string{ForgePublishTokenVar, ForgePublishURLVar} {
		if _, ok := got[name]; !ok {
			t.Errorf("DropServerMintedVars dropped the withdrawal of %s: the child would inherit its parent's live grant", name)
		}
	}
	pinned := map[string]string{ForgePublishTokenVar: "a-token-of-its-own", "reviewer": "alice"}
	if _, ok := DropServerMintedVars(pinned)[ForgePublishTokenVar]; ok {
		t.Error("DropServerMintedVars kept a token a client sent")
	}
}
