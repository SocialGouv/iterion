package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// checkoutFunc builds a ContextFetchFunc that materialises a binding's source
// as a plain directory under t's temp dir, seeded with the given files. The
// git half of the seam is pluginsource.Fetcher's, tested there; the resolver
// is tested against the seam's contract: a checkout dir, or an error.
func checkoutFunc(t *testing.T, files map[string]string, err error) ContextFetchFunc {
	t.Helper()
	return func(_ context.Context, b BotSecretBinding) (string, error) {
		if err != nil {
			return "", err
		}
		dir := t.TempDir()
		for rel, content := range files {
			p := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir, nil
	}
}

func contextBindingFor(id, name, path string) BotSecretBinding {
	return BotSecretBinding{
		ID:                    id,
		TenantID:              "team-1",
		BotID:                 "bot-doki",
		Kind:                  BindingKindContext,
		SecretNameForWorkflow: name,
		Context:               &ContextSource{GitURL: "https://example.invalid/o/c.git", Ref: "v1.0.0", Path: path},
		CreatedAt:             time.Now().UTC(),
		UpdatedAt:             time.Now().UTC(),
	}
}

func resolveOne(t *testing.T, bindings BotSecretBindingStore, fetch ContextFetchFunc, declared map[string]bool, maxBytes int64) ([]ResolvedContext, []SkippedContext, error) {
	t.Helper()
	ctx := store.WithTenant(context.Background(), "team-1")
	return ResolveContextBindings(ctx, bindings, fetch, "team-1", "bot-doki", declared, maxBytes, iterlog.Nop())
}

func TestResolveContextBindings_MaterialisesThePinnedFile(t *testing.T) {
	bindings := NewMemoryBotSecretBindingStore()
	ctx := store.WithTenant(context.Background(), "team-1")
	if err := bindings.Create(ctx, contextBindingFor("b1", "handbook", "docs/handbook.md")); err != nil {
		t.Fatal(err)
	}
	fetch := checkoutFunc(t, map[string]string{"docs/handbook.md": "# the handbook\n"}, nil)

	resolved, skipped, err := resolveOne(t, bindings, fetch, map[string]bool{"handbook": true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none", skipped)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolved = %+v, want one", resolved)
	}
	rc := resolved[0]
	if rc.Name != "handbook" || rc.Filename != "handbook.md" {
		t.Errorf("resolved under name=%q filename=%q, want handbook/handbook.md", rc.Name, rc.Filename)
	}
	if string(rc.Content) != "# the handbook\n" {
		t.Errorf("content = %q", rc.Content)
	}
	if rc.MIME == "" {
		t.Error("no MIME sniffed")
	}
	// A successful materialisation leaves the record healthy.
	got, _ := bindings.Get(ctx, "b1")
	if got.Degraded() {
		t.Errorf("a resolved binding reads degraded: %q", got.DegradedReason)
	}
}

func TestResolveContextBindings_SkipsLoudlyAndKeepsTheLaunch(t *testing.T) {
	ctx := store.WithTenant(context.Background(), "team-1")

	t.Run("an undeclared name delivers nothing and says so", func(t *testing.T) {
		bindings := NewMemoryBotSecretBindingStore()
		if err := bindings.Create(ctx, contextBindingFor("b1", "ghost", "x.md")); err != nil {
			t.Fatal(err)
		}
		resolved, skipped, err := resolveOne(t, bindings, checkoutFunc(t, map[string]string{"x.md": "x"}, nil), map[string]bool{"handbook": true}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(resolved) != 0 || len(skipped) != 1 {
			t.Fatalf("resolved=%d skipped=%d, want 0/1", len(resolved), len(skipped))
		}
		if !strings.Contains(skipped[0].Err.Error(), "declares no attachments: name") {
			t.Errorf("skip reason = %v, want it to name the missing declaration", skipped[0].Err)
		}
		got, _ := bindings.Get(ctx, "b1")
		if !got.Degraded() || !strings.Contains(got.DegradedReason, "declares no") {
			t.Errorf("the skip is not visible on the record: %+v", got)
		}
	})

	t.Run("a fetch failure skips one binding, not the launch", func(t *testing.T) {
		bindings := NewMemoryBotSecretBindingStore()
		if err := bindings.Create(ctx, contextBindingFor("b1", "handbook", "h.md")); err != nil {
			t.Fatal(err)
		}
		if err := bindings.Create(ctx, contextBindingFor("b2", "pricing", "p.md")); err != nil {
			t.Fatal(err)
		}
		fetch := func(_ context.Context, b BotSecretBinding) (string, error) {
			if b.ID == "b1" {
				return "", fmt.Errorf("the remote refused the pin")
			}
			return checkoutFunc(t, map[string]string{"p.md": "price list"}, nil)(context.Background(), b)
		}
		resolved, skipped, err := resolveOne(t, bindings, fetch, map[string]bool{"handbook": true, "pricing": true}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(resolved) != 1 || resolved[0].Name != "pricing" {
			t.Errorf("resolved = %+v, want the healthy binding only", resolved)
		}
		if len(skipped) != 1 || skipped[0].Binding.ID != "b1" {
			t.Errorf("skipped = %+v, want b1", skipped)
		}
		got, _ := bindings.Get(ctx, "b1")
		if !got.Degraded() || !strings.Contains(got.DegradedReason, "remote refused") {
			t.Errorf("b1 not flagged degraded: %+v", got)
		}
	})

	t.Run("a binding that resolves again clears its flag", func(t *testing.T) {
		bindings := NewMemoryBotSecretBindingStore()
		if err := bindings.Create(ctx, contextBindingFor("b1", "handbook", "h.md")); err != nil {
			t.Fatal(err)
		}
		if err := bindings.MarkDegraded(ctx, "team-1", "b1", "an earlier launch found it broken"); err != nil {
			t.Fatal(err)
		}
		_, skipped, err := resolveOne(t, bindings, checkoutFunc(t, map[string]string{"h.md": "ok"}, nil), map[string]bool{"handbook": true}, 0)
		if err != nil || len(skipped) != 0 {
			t.Fatalf("skipped=%+v err=%v", skipped, err)
		}
		got, _ := bindings.Get(ctx, "b1")
		if got.Degraded() {
			t.Errorf("the flag survived a successful materialisation: %q", got.DegradedReason)
		}
	})

	t.Run("the list failing is the one fatal error", func(t *testing.T) {
		resolved, skipped, err := resolveOne(t, failingBindingLister{}, checkoutFunc(t, nil, nil), nil, 0)
		if err == nil || resolved != nil || skipped != nil {
			t.Errorf("resolved=%v skipped=%v err=%v, want a fatal list error", resolved, skipped, err)
		}
	})

	t.Run("the secret kind is not this resolver's business", func(t *testing.T) {
		bindings := NewMemoryBotSecretBindingStore()
		b := contextBindingFor("b1", "GITLAB_TOKEN", "x.md")
		b.Kind, b.Context, b.SecretID = "", nil, "sec1"
		if err := bindings.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
		resolved, skipped, err := resolveOne(t, bindings, checkoutFunc(t, nil, fmt.Errorf("must not be called")), map[string]bool{"GITLAB_TOKEN": true}, 0)
		if err != nil || len(resolved) != 0 || len(skipped) != 0 {
			t.Errorf("resolved=%v skipped=%v err=%v, want all empty", resolved, skipped, err)
		}
	})
}

// failingBindingLister is the one failure the resolver keeps fatal.
type failingBindingLister struct{ BotSecretBindingStore }

func (failingBindingLister) ListByTenantBot(context.Context, string, string) ([]BotSecretBinding, error) {
	return nil, fmt.Errorf("the binding store is down")
}

func TestResolveContextBindings_TheReadIsContainedAndBounded(t *testing.T) {
	ctx := store.WithTenant(context.Background(), "team-1")
	declared := map[string]bool{"doc": true}

	newBound := func(t *testing.T, path string) BotSecretBindingStore {
		t.Helper()
		bindings := NewMemoryBotSecretBindingStore()
		if err := bindings.Create(ctx, contextBindingFor("b1", "doc", path)); err != nil {
			t.Fatal(err)
		}
		return bindings
	}

	t.Run("a symlink at the bound path is not followed out of the checkout", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "host-secret.txt")
		if err := os.WriteFile(outside, []byte("the host's private bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		fetch := func(context.Context, BotSecretBinding) (string, error) {
			dir := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(dir, "linked.md")); err != nil {
				t.Fatal(err)
			}
			return dir, nil
		}
		resolved, skipped, err := resolveOne(t, newBound(t, "linked.md"), fetch, declared, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(resolved) != 0 || len(skipped) != 1 {
			t.Fatalf("resolved=%d skipped=%d — the symlink's target left the checkout", len(resolved), len(skipped))
		}
	})

	t.Run("an escaping path is the operator's misconfiguration", func(t *testing.T) {
		for _, p := range []string{"../outside.md", "/etc/hostname", "a/../../b.md"} {
			resolved, skipped, err := resolveOne(t, newBound(t, p), checkoutFunc(t, map[string]string{"b.md": "x"}, nil), declared, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(resolved) != 0 || len(skipped) != 1 {
				t.Errorf("path %q: resolved=%d skipped=%d, want refused", p, len(resolved), len(skipped))
			}
		}
	})

	t.Run("a document past the bound is refused whole, not truncated", func(t *testing.T) {
		fetch := checkoutFunc(t, map[string]string{"big.md": strings.Repeat("x", 4096)}, nil)
		resolved, skipped, err := resolveOne(t, newBound(t, "big.md"), fetch, declared, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if len(resolved) != 0 || len(skipped) != 1 {
			t.Fatalf("resolved=%d skipped=%d, want the oversize refused", len(resolved), len(skipped))
		}
		if !strings.Contains(skipped[0].Err.Error(), "ceiling") {
			t.Errorf("refusal = %v, want it to name the bound", skipped[0].Err)
		}
	})

	t.Run("a directory at the bound path is not a document", func(t *testing.T) {
		fetch := func(context.Context, BotSecretBinding) (string, error) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
				t.Fatal(err)
			}
			return dir, nil
		}
		resolved, skipped, err := resolveOne(t, newBound(t, "subdir"), fetch, declared, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(resolved) != 0 || len(skipped) != 1 {
			t.Fatalf("resolved=%d skipped=%d, want refused", len(resolved), len(skipped))
		}
	})
}

func TestBotBindingTwins_UpdateNeverTouchesTheDegradedReadout(t *testing.T) {
	for _, tw := range bindingTwins(t) {
		t.Run(tw.name, func(t *testing.T) {
			ctx := store.WithTenant(context.Background(), "team-1")
			b := contextBindingFixture("bind-deg", "team-1", "bot-doki", "handbook")
			if err := tw.store.Create(ctx, b); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := tw.store.MarkDegraded(ctx, "team-1", "bind-deg", "the pin did not resolve"); err != nil {
				t.Fatalf("mark: %v", err)
			}
			got, err := tw.store.Get(ctx, "bind-deg")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if !got.Degraded() || got.DegradedReason != "the pin did not resolve" || got.DegradedAt == nil {
				t.Fatalf("the mark did not survive the round trip: %+v", got)
			}
			// An operator's edit rides the same record and must not wipe the
			// engine's readout — a PATCH and a launch's mark race, and the
			// field-wise update is what makes the race survivable.
			b.Context.Ref = "v1.4.3"
			b.UpdatedAt = time.Now().UTC()
			if err := tw.store.Update(ctx, b); err != nil {
				t.Fatalf("update: %v", err)
			}
			got, err = tw.store.Get(ctx, "bind-deg")
			if err != nil {
				t.Fatalf("get after update: %v", err)
			}
			if got.Context.Ref != "v1.4.3" {
				t.Errorf("the edit did not land: %+v", got.Context)
			}
			if !got.Degraded() || got.DegradedReason != "the pin did not resolve" {
				t.Errorf("the update wiped the degraded readout: %+v", got)
			}
			if err := tw.store.ClearDegraded(ctx, "team-1", "bind-deg"); err != nil {
				t.Fatalf("clear: %v", err)
			}
			got, _ = tw.store.Get(ctx, "bind-deg")
			if got.Degraded() || got.DegradedAt != nil {
				t.Errorf("the clear left residue: %+v", got)
			}
			// Keyed by the explicit tenant: another team cannot reach the
			// record, and an unknown id is a typed not-found.
			if err := tw.store.MarkDegraded(ctx, "team-2", "bind-deg", "x"); !errors.Is(err, ErrBindingNotFound) {
				t.Errorf("cross-tenant mark = %v, want ErrBindingNotFound", err)
			}
			if err := tw.store.ClearDegraded(ctx, "team-1", "ghost"); !errors.Is(err, ErrBindingNotFound) {
				t.Errorf("clear of an unknown id = %v, want ErrBindingNotFound", err)
			}
		})
	}
}
