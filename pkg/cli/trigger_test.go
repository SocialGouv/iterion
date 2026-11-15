package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

func writeTriggerBot(t *testing.T, botsDir, name, manifest string) {
	t.Helper()
	dir := filepath.Join(botsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	main := "workflow w:\n  agent a:\n    model: \"test\"\n  a -> done\n\nagent a:\n  model: \"test\"\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The local store is the journal under the store directory: a restart finds
// the manifest's rows under the same ids, the operator's choices on them,
// and the operator's own rows; a project whose bots declare nothing still
// gets a store, so the operator's subscriptions have a spine.
func TestTheLocalTriggerStoreSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	storeDir, botsDir := t.TempDir(), t.TempDir()
	writeTriggerBot(t, botsDir, "reporter", "name: reporter\nschema_version: 1\ninvocations:\n  - kind: schedule\n    schedule:\n      suggested_cron: \"0 7 * * 1\"\n")
	id := trigger.DerivedSubscriptionID("", "reporter", bundle.InvocationKindSchedule, "0", "")

	first := openLocalTriggerStore(storeDir, []string{botsDir}, nil)
	row, err := first.Get(ctx, id)
	if err != nil {
		t.Fatalf("the manifest's schedule has no row under its derived id: %v", err)
	}
	row.Enabled = false
	if err := first.Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := first.Create(ctx, trigger.Subscription{ID: "op-1", BotID: "reporter", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	second := openLocalTriggerStore(storeDir, []string{botsDir}, nil)
	got, err := second.Get(ctx, id)
	if err != nil || got.Enabled {
		t.Fatalf("after a restart the manifest row = %+v, %v; want it still disabled", got, err)
	}
	if _, err := second.Get(ctx, "op-1"); err != nil {
		t.Fatalf("after a restart the operator's row is gone: %v", err)
	}

	empty := openLocalTriggerStore(t.TempDir(), []string{t.TempDir()}, nil)
	if empty == nil {
		t.Fatal("a project whose bots declare nothing got no store")
	}
}
