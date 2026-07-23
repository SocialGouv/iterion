package botsource

import (
	"context"
	"errors"
	"testing"
)

const miniBot = "workflow main:\n  start -> done\n\ndone finish:\n"

func validSource(tenant, slug string) BotSource {
	return BotSource{
		TenantID: tenant,
		Slug:     slug,
		Files:    map[string]string{MainBotFile: miniBot},
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*BotSource)
		wantErr bool
	}{
		{"ok", func(*BotSource) {}, false},
		{"no tenant", func(s *BotSource) { s.TenantID = "" }, true},
		{"bad slug", func(s *BotSource) { s.Slug = "Bad Slug" }, true},
		{"no files", func(s *BotSource) { s.Files = nil }, true},
		{"missing main.bot", func(s *BotSource) { s.Files = map[string]string{"skills/x.md": "hi"} }, true},
		{"empty main.bot", func(s *BotSource) { s.Files[MainBotFile] = "  " }, true},
		{"traversal key", func(s *BotSource) { s.Files["../evil"] = "x" }, true},
		{"absolute key", func(s *BotSource) { s.Files["/etc/passwd"] = "x" }, true},
		{"git key", func(s *BotSource) { s.Files[".git/config"] = "x" }, true},
		{"unnormalized key", func(s *BotSource) { s.Files["skills/./x.md"] = "x" }, true},
		{"nested skill ok", func(s *BotSource) { s.Files["skills/x.md"] = "hi" }, false},
		{"bad manifest", func(s *BotSource) { s.Files["manifest.yaml"] = "name: [unterminated" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSource("team-1", "reviewer")
			tc.mutate(&s)
			err := s.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestMemoryStore_CRUD(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()

	created, err := st.Create(ctx, validSource("team-1", "reviewer"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" || created.Version != 1 || created.Origin != "tenant" {
		t.Fatalf("Create did not stamp defaults: %+v", created)
	}

	// Slug conflict within the same tenant.
	if _, err := st.Create(ctx, validSource("team-1", "reviewer")); !errors.Is(err, ErrSlugConflict) {
		t.Fatalf("want ErrSlugConflict, got %v", err)
	}
	// Same slug in another tenant is fine (isolation).
	if _, err := st.Create(ctx, validSource("team-2", "reviewer")); err != nil {
		t.Fatalf("cross-tenant same slug should be allowed: %v", err)
	}

	// GetBySlug is tenant-scoped.
	got, err := st.GetBySlug(ctx, "team-1", "reviewer")
	if err != nil || got.ID != created.ID {
		t.Fatalf("GetBySlug: %v id=%s", err, got.ID)
	}
	if _, err := st.GetBySlug(ctx, "team-3", "reviewer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for foreign tenant, got %v", err)
	}

	// Update bumps version.
	created.Files["skills/help.md"] = "# help"
	updated, err := st.Update(ctx, created)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Version != 2 {
		t.Fatalf("want version 2, got %d", updated.Version)
	}

	// Stale if-match version is rejected.
	created.Version = 1
	if _, err := st.Update(ctx, created); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("want ErrVersionConflict, got %v", err)
	}

	// List is tenant-scoped.
	list, err := st.ListByTenant(ctx, "team-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByTenant team-1: %v len=%d", err, len(list))
	}

	if err := st.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
}
