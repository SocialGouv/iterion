package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/botsource"
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
	// Origin is "tenant" for a team-authored bot, "platform" for a
	// deployment-wide override (super-admin-pushed, managed via
	// /api/admin/bots), "catalog" for a baked one.
	Origin string `json:"origin"`
}

// mergedBotEntries returns the catalog bots overlaid with the deployment's
// platform overrides and the caller's team-authored bots — same-name precedence
// team > platform > catalog, matching launch resolution. Catalog and platform
// entries are read-only in the studio (platform bots are managed through the
// admin API/CLI); team entries are editable. Outside cloud / without an active
// team it returns catalog + platform, all read-only.
func (s *Server) mergedBotEntries(ctx context.Context) ([]botEntryView, error) {
	var catalog []botregistry.EntryWithSchema
	if len(s.effectivePaths()) > 0 {
		var err error
		catalog, err = botregistry.ListWithSchema(s.botListOptions())
		if err != nil {
			return nil, err
		}
	}
	byName := make(map[string]botEntryView, len(catalog))
	order := make([]string, 0, len(catalog))
	add := func(entries []botregistry.EntryWithSchema, editable bool, origin string) {
		for _, e := range entries {
			if _, seen := byName[e.Name]; !seen {
				order = append(order, e.Name)
			}
			byName[e.Name] = botEntryView{EntryWithSchema: e, Editable: editable, Origin: origin}
		}
	}
	add(catalog, false, "catalog")
	add(s.platformBotEntries(), false, "platform")
	if id, _ := auth.FromContext(ctx); id.TeamID != "" {
		add(s.storedBotEntries(ctx, id.TeamID), true, "tenant")
	}
	out := make([]botEntryView, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out, nil
}

// tenantBotEntries returns the caller team's authored bot bundle entries.
func (s *Server) tenantBotEntries(ctx context.Context) []botregistry.EntryWithSchema {
	id, _ := auth.FromContext(ctx)
	if id.TeamID == "" {
		return nil
	}
	return s.storedBotEntries(ctx, id.TeamID)
}

// storedBotEntries materializes one tenant's stored bot bundles to entry
// metadata. Returns nil (never an error) when there is no store or nothing
// to materialize: a broken stored bundle must not blank the whole gallery.
func (s *Server) storedBotEntries(ctx context.Context, tenantID string) []botregistry.EntryWithSchema {
	if s.botSources == nil || tenantID == "" {
		return nil
	}
	list, err := s.botSources.ListByTenant(store.WithTenant(ctx, tenantID), tenantID)
	if err != nil || len(list) == 0 {
		return nil
	}
	return s.materializeBotEntries(list)
}

// materializeBotEntries writes stored bundles to a temp tree and runs the
// same botregistry.ListWithSchema discovery over them, so a stored bot's
// metadata + vars schema are extracted identically to a catalog bot's — no
// parallel schema code.
func (s *Server) materializeBotEntries(list []botsource.BotSource) []botregistry.EntryWithSchema {
	if len(list) == 0 {
		return nil
	}
	root, err := os.MkdirTemp("", "iterion-stored-bots-*")
	if err != nil {
		return nil
	}
	defer func() { _ = os.RemoveAll(root) }()

	for _, bs := range list {
		if err := botsource.Materialize(filepath.Join(root, bs.Slug), bs.Files); err != nil {
			// One broken bundle is skipped LOUDLY (Materialize removed its
			// partial tree), never rendered half-materialized.
			s.logger.Warn("bot source %s/%s: %v — omitted from listing", bs.TenantID, bs.Slug, err)
		}
	}

	entries, err := botregistry.ListWithSchema(botregistry.ListOptions{Paths: []string{root}})
	if err != nil {
		return nil
	}
	// A stored bot's identity is its SLUG (the store key), not the workflow name
	// a loose main.bot would otherwise be discovered under. The slug is the dir
	// directly under root — force it so list/get/launch key on the same name.
	// Then BLANK the path: it points into the temp root removed on return, and
	// a consumer that path-resolves a stored bot (manifest write, dispatcher
	// admission) must fail on an explicit empty path, not a dangling one —
	// stored bots resolve through the store, never the filesystem.
	for i := range entries {
		if slug := slugFromMaterializedPath(root, entries[i].Path); slug != "" {
			entries[i].Name = slug
		}
		entries[i].Path = ""
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
