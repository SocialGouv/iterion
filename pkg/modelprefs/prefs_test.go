package modelprefs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Both stores answer the same questions, so both run the same contract. The
// file store is what a local studio actually uses; the memory one is what the
// tests of everything else use, and a divergence between them would only show
// up as a bug on the operator's machine.
func TestStoreContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T) Store
	}{
		{"mem", func(*testing.T) Store { return NewMemStore() }},
		{"file", func(t *testing.T) Store { return NewFileStore(t.TempDir()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := tc.make(t)

			// Absent means "no preference recorded", not an error — the caller
			// falls back to the bot's own defaults.
			got, err := s.Get(ctx, "team1", "user1", "whats-next")
			if err != nil {
				t.Fatalf("Get on an empty store: %v", err)
			}
			if got != nil {
				t.Fatalf("expected nil for an unrecorded preference, got %+v", got)
			}

			want := &Pref{
				TenantID: "team1", UserID: "user1", Key: "whats-next",
				Model: "anthropic/claude-opus-5", Backend: "claude_code", Effort: "ultracode",
			}
			if err := s.Set(ctx, want); err != nil {
				t.Fatalf("Set: %v", err)
			}
			got, err = s.Get(ctx, "team1", "user1", "whats-next")
			if err != nil || got == nil {
				t.Fatalf("Get after Set: %+v, %v", got, err)
			}
			if got.Model != want.Model || got.Backend != want.Backend || got.Effort != want.Effort {
				t.Errorf("round-trip = %+v, want %+v", got, want)
			}
			if got.UpdatedAt.IsZero() {
				t.Error("Set must stamp UpdatedAt")
			}

			// Isolation across all three key dimensions.
			for _, other := range [][3]string{
				{"team2", "user1", "whats-next"},
				{"team1", "user2", "whats-next"},
				{"team1", "user1", "some-other-bot"},
			} {
				p, err := s.Get(ctx, other[0], other[1], other[2])
				if err != nil {
					t.Fatalf("Get %v: %v", other, err)
				}
				if p != nil {
					t.Errorf("preference leaked across %v: %+v", other, p)
				}
			}

			// Overwrite wins, including clearing a dimension.
			if err := s.Set(ctx, &Pref{
				TenantID: "team1", UserID: "user1", Key: "whats-next",
				Model: "openai/gpt-5.5",
			}); err != nil {
				t.Fatalf("Set overwrite: %v", err)
			}
			got, _ = s.Get(ctx, "team1", "user1", "whats-next")
			if got.Model != "openai/gpt-5.5" || got.Backend != "" || got.Effort != "" {
				t.Errorf("overwrite left stale dimensions: %+v", got)
			}

			if err := s.Delete(ctx, "team1", "user1", "whats-next"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			got, _ = s.Get(ctx, "team1", "user1", "whats-next")
			if got != nil {
				t.Errorf("Delete left %+v behind", got)
			}
			// Deleting what is not there is not an error.
			if err := s.Delete(ctx, "team1", "user1", "whats-next"); err != nil {
				t.Errorf("second Delete: %v", err)
			}
		})
	}
}

func TestBlankKeyIsRejected(t *testing.T) {
	ctx := context.Background()
	for _, s := range []Store{NewMemStore(), NewFileStore(t.TempDir())} {
		if _, err := s.Get(ctx, "t", "u", "  "); err == nil {
			t.Error("Get with a blank key must error")
		}
		if err := s.Set(ctx, &Pref{Key: ""}); err == nil {
			t.Error("Set with a blank key must error")
		}
	}
}

func TestPrefEmpty(t *testing.T) {
	if !(Pref{Key: "k"}).Empty() {
		t.Error("a preference expressing no choice is Empty")
	}
	if (Pref{Key: "k", Effort: "high"}).Empty() {
		t.Error("an effort-only preference is not Empty")
	}
}

// A hand-edited or truncated file must not brick the surface reading it: the
// worst honest outcome is that the operator re-picks their model.
func TestFileStoreSurvivesACorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model-prefs.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewFileStore(dir)
	got, err := s.Get(context.Background(), "", "", "whats-next")
	if err != nil {
		t.Fatalf("Get over a corrupt file: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no preference, got %+v", got)
	}
	// And a write repairs it rather than compounding the damage.
	if err := s.Set(context.Background(), &Pref{Key: "whats-next", Model: "openai/gpt-5.5"}); err != nil {
		t.Fatalf("Set over a corrupt file: %v", err)
	}
	got, _ = s.Get(context.Background(), "", "", "whats-next")
	if got == nil || got.Model != "openai/gpt-5.5" {
		t.Errorf("after repair = %+v", got)
	}
}

// The file has to survive the process, which is the entire point.
func TestFileStorePersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	if err := NewFileStore(dir).Set(ctx, &Pref{Key: "whats-next", Model: "anthropic/claude-opus-5"}); err != nil {
		t.Fatal(err)
	}
	got, err := NewFileStore(dir).Get(ctx, "", "", "whats-next")
	if err != nil || got == nil {
		t.Fatalf("reload = %+v, %v", got, err)
	}
	if got.Model != "anthropic/claude-opus-5" {
		t.Errorf("reloaded model = %q", got.Model)
	}
}
