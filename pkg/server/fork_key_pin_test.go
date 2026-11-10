package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A cloud server's fork drops a key pin whose key no longer exists: the
// credential sweep cleared the webhook's pin at the key's deletion, and the
// run document's frozen copy must not strand the fork child the operator
// makes to recover the run (its gateway pin is exclusive — every resume of
// it would be refused). A live pin travels. Drives the server's own wiring
// (runview.WithForkKeyPinCheck on the cloud service) through Fork.
func TestACloudForkDropsAKeyPinWhoseKeyNoLongerExists(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatal(err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	alive := secrets.ApiKey{ID: secrets.NewApiKeyID(), TenantID: "team1", ScopeTeamID: "team1", Provider: secrets.ProviderOpenAI, Name: "alive", CreatedAt: time.Now().UTC()}
	if err := keys.Create(context.Background(), alive); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{
		WorkDir:                 t.TempDir(),
		SkipProjectRegistration: true,
		DisableAuth:             true,
		Mode:                    "cloud",
		Store:                   st,
		ApiKeys:                 keys,
	}, iterlog.New(iterlog.LevelError, nil))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	if srv.runs == nil {
		t.Fatal("fixture: the cloud server built no run service")
	}
	ctx := store.WithTenant(context.Background(), "team1")
	const parentID = "run-parent"
	if _, err := st.CreateRun(ctx, parentID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	parent, err := st.LoadRun(ctx, parentID)
	if err != nil {
		t.Fatal(err)
	}
	const deleted = "key-deleted-since"
	parent.TenantID = "team1"
	parent.KeyOverrides = map[string]string{string(secrets.ProviderOpenAICompatible): deleted, string(secrets.ProviderOpenAI): alive.ID}
	parent.Status = store.RunStatusCancelled
	parent.Checkpoint = &store.Checkpoint{NodeID: "implement"}
	if err := st.SaveRun(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteTurn(ctx, &store.TurnCheckpoint{
		RunID: parentID, NodeID: "implement", Backend: "claw", FinishReason: "tool_use",
		MessagesRef: "implement/0/0.messages.json",
		Messages:    json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]`),
		WrittenAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := srv.runs.Fork(ctx, runview.ForkSpec{RunID: parentID, NodeID: "implement", TurnIndex: -1})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	child, err := st.LoadRun(ctx, res.NewRunID)
	if err != nil {
		t.Fatal(err)
	}
	if pin, kept := child.KeyOverrides[string(secrets.ProviderOpenAICompatible)]; kept {
		t.Fatalf("the child kept the gateway pin %q whose key no longer exists", pin)
	}
	if child.KeyOverrides[string(secrets.ProviderOpenAI)] != alive.ID {
		t.Fatalf("the child dropped the live pin: %v", child.KeyOverrides)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], deleted) {
		t.Fatalf("fork notes = %v, want the dropped pin named", res.Notes)
	}
}
