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

// The twins agree on what Update may move: the mutable payload (kind, the
// secret reference, the declared name, the context source, the hosts, the
// update stamp), never the record's identity or provenance — the Mongo twin
// updates field-wise ($set), so bot_id, created_by and created_at are
// frozen there; the memory twin must refuse to move them too, or a fixture
// exercises a mutation production ignores.
func TestBotBindingTwins_UpdateCannotMoveTheRecordsIdentity(t *testing.T) {
	for _, tw := range bindingTwins(t) {
		t.Run(tw.name, func(t *testing.T) {
			ctx := store.WithTenant(context.Background(), "team-1")
			b := contextBindingFixture("bind-id", "team-1", "bot-doki", "handbook")
			if err := tw.store.Create(ctx, b); err != nil {
				t.Fatalf("create: %v", err)
			}
			moved := contextBindingFixture("bind-id", "team-1", "bot-other", "handbook")
			moved.CreatedBy = "mallory"
			moved.CreatedAt = moved.CreatedAt.Add(-72 * time.Hour)
			moved.Context.Ref = "v2.0.0"
			if err := tw.store.Update(ctx, moved); err != nil {
				t.Fatalf("update: %v", err)
			}
			got, err := tw.store.Get(ctx, "bind-id")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Context.Ref != "v2.0.0" {
				t.Errorf("the mutable payload did not move: %+v", got.Context)
			}
			if got.BotID != "bot-doki" {
				t.Errorf("bot_id moved to %q — the Mongo twin's field-wise update freezes it", got.BotID)
			}
			if got.CreatedBy != "alice" {
				t.Errorf("created_by moved to %q", got.CreatedBy)
			}
			if !got.CreatedAt.Equal(b.CreatedAt) {
				t.Errorf("created_at moved to %v", got.CreatedAt)
			}
		})
	}
}

// Every mutable field the store's Update may move, moved ONE AT A TIME and
// read back: the memory twin applies the field-wise merge by hand, and a
// merge line that drops its field must redden its assertion here — the
// route answers the REQUEST's echo, so a store that keeps the old value is
// a 200 that lies, divergent from the Mongo twin.
func TestBotBindingTwins_UpdateMovesTheMutablePayloadFieldByField(t *testing.T) {
	for _, tw := range bindingTwins(t) {
		t.Run(tw.name, func(t *testing.T) {
			ctx := store.WithTenant(context.Background(), "team-1")
			seed := func(t *testing.T, id, name string) BotSecretBinding {
				t.Helper()
				b := contextBindingFixture(id, "team-1", "bot-doki", name)
				if err := tw.store.Create(ctx, b); err != nil {
					t.Fatalf("create %s: %v", id, err)
				}
				return b
			}
			reread := func(t *testing.T, id string) BotSecretBinding {
				t.Helper()
				got, err := tw.store.Get(ctx, id)
				if err != nil {
					t.Fatalf("get %s: %v", id, err)
				}
				return got
			}

			t.Run("kind", func(t *testing.T) {
				b := seed(t, "f-kind", "n-kind")
				b.Kind = BindingKindSecret
				b.Context, b.SecretID = nil, "sec-9" // a coherent secret-kind payload
				if err := tw.store.Update(ctx, b); err != nil {
					t.Fatalf("update: %v", err)
				}
				if got := reread(t, "f-kind"); got.Kind != BindingKindSecret || got.SecretID != "sec-9" || got.Context != nil {
					t.Errorf("the kind switch did not land: %+v", got)
				}
			})
			t.Run("secret_id", func(t *testing.T) {
				b := seed(t, "f-sec", "n-sec")
				b.Context = &ContextSource{GitURL: b.Context.GitURL, Ref: b.Context.Ref, Path: b.Context.Path, SecretID: "sec-read-2"}
				if err := tw.store.Update(ctx, b); err != nil {
					t.Fatalf("update: %v", err)
				}
				if got := reread(t, "f-sec"); got.Context.SecretID != "sec-read-2" {
					t.Errorf("context.secret_id did not move: %+v", got.Context)
				}
			})
			t.Run("secret_name_for_workflow", func(t *testing.T) {
				b := seed(t, "f-name", "n-name")
				b.SecretNameForWorkflow = "n-name-renamed"
				if err := tw.store.Update(ctx, b); err != nil {
					t.Fatalf("update: %v", err)
				}
				if got := reread(t, "f-name"); got.SecretNameForWorkflow != "n-name-renamed" {
					t.Errorf("the declared name did not move: %q", got.SecretNameForWorkflow)
				}
			})
			t.Run("context", func(t *testing.T) {
				b := seed(t, "f-ctx", "n-ctx")
				b.Context.Ref = "v9.9.9"
				if err := tw.store.Update(ctx, b); err != nil {
					t.Fatalf("update: %v", err)
				}
				if got := reread(t, "f-ctx"); got.Context.Ref != "v9.9.9" {
					t.Errorf("the context payload did not move: %+v", got.Context)
				}
			})
			t.Run("allowed_hosts", func(t *testing.T) {
				b := seed(t, "f-hosts", "n-hosts")
				b.AllowedHosts = []string{"only.example"}
				if err := tw.store.Update(ctx, b); err != nil {
					t.Fatalf("update: %v", err)
				}
				if got := reread(t, "f-hosts"); len(got.AllowedHosts) != 1 || got.AllowedHosts[0] != "only.example" {
					t.Errorf("allowed_hosts did not move: %v", got.AllowedHosts)
				}
			})
			t.Run("updated_at", func(t *testing.T) {
				b := seed(t, "f-stamp", "n-stamp")
				later := b.UpdatedAt.Add(2 * time.Hour)
				b.UpdatedAt = later
				if err := tw.store.Update(ctx, b); err != nil {
					t.Fatalf("update: %v", err)
				}
				if got := reread(t, "f-stamp"); !got.UpdatedAt.Equal(later) {
					t.Errorf("updated_at did not move: %v, want %v", got.UpdatedAt, later)
				}
			})
		})
	}
}

// The declaration gate runs BEFORE any fetch: a binding under a name the
// workflow never declares is refused without touching the network — the
// undeclared case is the operator's misconfiguration to read on the record,
// and a fetch it triggers would cost a clone per launch per dead binding.
// A fetch-before-gate mutant turns the counter's assertion red.
func TestAdv2_UndeclaredNameNeverFetches(t *testing.T) {
	bindings := NewMemoryBotSecretBindingStore()
	ctx := store.WithTenant(context.Background(), "team-1")
	if err := bindings.Create(ctx, contextBindingFor("b1", "ghost", "x.md")); err != nil {
		t.Fatal(err)
	}
	fetches := 0
	fetch := func(context.Context, BotSecretBinding) (string, error) {
		fetches++
		return checkoutFunc(t, map[string]string{"x.md": "x"}, nil)(context.Background(), BotSecretBinding{})
	}
	resolved, skipped, err := resolveOne(t, bindings, fetch, map[string]bool{"handbook": true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 0 || len(skipped) != 1 {
		t.Fatalf("resolved=%d skipped=%d, want 0/1", len(resolved), len(skipped))
	}
	if fetches != 0 {
		t.Errorf("an undeclared name triggered %d fetch(es) — the declaration gate must run before the network", fetches)
	}
}
