package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botsource"
)

// remote admin bots — platform bot overrides (super-admin): the DB-backed
// form of the baked bot catalog, so pushing a new version of any bot to the
// deployment is one CLI call instead of an image build + rollout. The
// iteration loop this file exists for:
//
//	edit bots/review-pr/ locally
//	iterion remote admin bots push bots/review-pr
//	→ the next launch of review-pr (webhook, schedule, board, studio) runs it
//	iterion remote admin bots rm review-pr   # revert to the baked catalog

// RemoteAdminBotsPush reads a bundle directory into a file map and PUTs it
// as the platform override for slug (default: the directory's basename).
// Non-UTF-8 files are refused EXPLICITLY: the store carries JSON text, so a
// binary file cannot ride it — the bundle keeps its baked form instead of
// being silently corrupted. Server-side compile errors and push warnings
// surface verbatim.
func RemoteAdminBotsPush(ctx context.Context, c *RemoteClient, p *Printer, dir, slug string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("bundle dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("bundle path %q is not a directory (push takes the bundle dir, e.g. bots/review-pr)", dir)
	}
	if slug == "" {
		slug = filepath.Base(filepath.Clean(dir))
	}
	// Validate client-side so `push .` fails on "slug '.' is invalid" rather
	// than on whatever the path-cleaned URL happens to hit.
	if err := botsource.ValidSlug(slug); err != nil {
		return fmt.Errorf("%w (pass --slug to name the override explicitly)", err)
	}
	files, err := botsource.ReadBundleDir(dir)
	if err != nil {
		return err
	}
	if strings.TrimSpace(files[botsource.MainBotFile]) == "" {
		return fmt.Errorf("bundle dir %q has no %s", dir, botsource.MainBotFile)
	}
	body, err := json.Marshal(map[string]any{"files": files})
	if err != nil {
		return err
	}
	raw, err := c.Call(ctx, "PUT", "/api/admin/bots/"+slug, json.RawMessage(body), nil)
	if err != nil {
		return err
	}
	// Surface warnings prominently on stderr; the JSON result stays on stdout.
	var resp struct {
		Version  int      `json:"version"`
		Warnings []string `json:"warnings"`
	}
	if jerr := json.Unmarshal(raw, &resp); jerr == nil {
		for _, w := range resp.Warnings {
			fmt.Fprintln(os.Stderr, "WARNING: "+w)
		}
		fmt.Fprintf(os.Stderr, "pushed %q (%d files) as platform override v%d — next launch runs it; `admin bots rm %s` reverts to the baked catalog\n",
			slug, len(files), resp.Version, slug)
	}
	PrintRemoteJSON(p, raw)
	return nil
}

// RemoteAdminBotsPull writes the platform override's files under outDir
// (default "./<slug>"), refusing to overwrite an existing directory.
func RemoteAdminBotsPull(ctx context.Context, c *RemoteClient, p *Printer, slug, outDir string) error {
	var resp struct {
		Files map[string]string `json:"files"`
	}
	if _, err := c.Call(ctx, "GET", "/api/admin/bots/"+slug, nil, &resp); err != nil {
		return err
	}
	if outDir == "" {
		outDir = slug
	}
	if _, err := os.Stat(outDir); err == nil {
		return fmt.Errorf("output dir %q already exists — pass --out to choose another", outDir)
	}
	// Same validated, all-or-nothing write path the engine uses.
	if err := botsource.Materialize(outDir, resp.Files); err != nil {
		return err
	}
	p.Line("pulled %q: %d files → %s", slug, len(resp.Files), outDir)
	return nil
}
