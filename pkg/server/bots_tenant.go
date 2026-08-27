package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/store"
)

// botEntryView is a discovered bot plus its editability. Editable/Origin let the
// studio render a baked catalog bot read-only ("Duplicate & edit" only) and a
// team-authored bot as directly editable. Both fields are additive to the
// EntryWithSchema shape the SPA already consumes.
type botEntryView struct {
	botregistry.EntryWithSchema
	// Editable is true for team-authored bots (persisted in the bot-source
	// store), false for the read-only baked catalog.
	Editable bool `json:"editable"`
	// Origin is "tenant" for a team-authored bot, "catalog" for a baked one.
	Origin string `json:"origin"`
}

// mergedBotEntries returns the catalog bots plus the caller's team-authored bots.
// A team bot overrides a catalog bot of the same name (the override the tenant
// store is designed to express). Catalog entries are marked read-only; team
// entries editable. Outside cloud / without an active team it returns the
// catalog alone, all read-only. The second return carries the per-entry
// discovery errors (one per skipped malformed bundle) so the list endpoint
// can surface them instead of letting a bad bundle vanish silently.
func (s *Server) mergedBotEntries(ctx context.Context) ([]botEntryView, []botregistry.DiscoveryError, error) {
	var catalog []botregistry.EntryWithSchema
	var diags []botregistry.DiscoveryError
	if len(s.effectivePaths()) > 0 {
		var err error
		catalog, diags, err = botregistry.ListWithSchemaDiagnostics(s.botListOptions())
		if err != nil {
			return nil, nil, err
		}
	}
	byName := make(map[string]botEntryView, len(catalog))
	order := make([]string, 0, len(catalog))
	for _, e := range catalog {
		if _, seen := byName[e.Name]; !seen {
			order = append(order, e.Name)
		}
		byName[e.Name] = botEntryView{EntryWithSchema: e, Editable: false, Origin: "catalog"}
	}
	for _, e := range s.tenantBotEntries(ctx) {
		if _, seen := byName[e.Name]; !seen {
			order = append(order, e.Name)
		}
		byName[e.Name] = botEntryView{EntryWithSchema: e, Editable: true, Origin: "tenant"}
	}
	out := make([]botEntryView, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out, diags, nil
}

// tenantBotEntries materializes the caller team's authored bot bundles to a temp
// tree and runs the same botregistry.ListWithSchema discovery over them, so a
// tenant bot's metadata + vars schema are extracted identically to a catalog
// bot's — no parallel schema code. Returns nil (never an error) when there is no
// store, no active team, or nothing to materialize: a broken tenant bundle must
// not blank the whole gallery.
func (s *Server) tenantBotEntries(ctx context.Context) []botregistry.EntryWithSchema {
	if s.botSources == nil {
		return nil
	}
	id, _ := auth.FromContext(ctx)
	if id.TeamID == "" {
		return nil
	}
	list, err := s.botSources.ListByTenant(store.WithTenant(ctx, id.TeamID), id.TeamID)
	if err != nil || len(list) == 0 {
		return nil
	}
	root, err := os.MkdirTemp("", "iterion-tenant-bots-*")
	if err != nil {
		return nil
	}
	defer func() { _ = os.RemoveAll(root) }()

	for _, bs := range list {
		botDir := filepath.Join(root, bs.Slug)
		wrote := false
		for rel, content := range bs.Files {
			// The store already validated every key as a safe relative path;
			// re-clean defensively so a materialization can never escape root.
			clean := filepath.Clean(filepath.FromSlash(rel))
			if clean == ".." || filepath.IsAbs(clean) || hasParentTraversal(clean) {
				continue
			}
			dst := filepath.Join(botDir, clean)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				continue
			}
			if err := os.WriteFile(dst, []byte(content), 0o644); err == nil {
				wrote = true
			}
		}
		if !wrote {
			continue
		}
	}

	entries, err := botregistry.ListWithSchema(botregistry.ListOptions{Paths: []string{root}})
	if err != nil {
		return nil
	}
	// A tenant bot's identity is its SLUG (the store key), not the workflow name
	// a loose main.bot would otherwise be discovered under. The slug is the dir
	// directly under root — force it so list/get/launch key on the same name.
	for i := range entries {
		if slug := slugFromMaterializedPath(root, entries[i].Path); slug != "" {
			entries[i].Name = slug
		}
	}
	return entries
}

// slugFromMaterializedPath extracts "<slug>" — the first path segment under
// root. botregistry sets an entry's Path to the bundle DIRECTORY ("<root>/<slug>"),
// so the relative path is a single segment; a loose bot would be "<slug>/main.bot".
// Either way the slug is the first segment.
func slugFromMaterializedPath(root, botPath string) string {
	rel, err := filepath.Rel(root, botPath)
	if err != nil {
		return ""
	}
	seg, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	if seg == "" || seg == "." || seg == ".." {
		return ""
	}
	return seg
}

func hasParentTraversal(clean string) bool {
	return clean == ".." ||
		len(clean) >= 3 && clean[:3] == ".."+string(filepath.Separator)
}
