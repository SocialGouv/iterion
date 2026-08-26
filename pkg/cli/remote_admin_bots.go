package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
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
	files, err := readBundleDir(dir)
	if err != nil {
		return err
	}
	if strings.TrimSpace(files["main.bot"]) == "" {
		return fmt.Errorf("bundle dir %q has no main.bot", dir)
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
	for rel, content := range resp.Files {
		// The server validated the keys at write time; re-check containment
		// before writing to the operator's filesystem anyway.
		clean := filepath.Clean(filepath.FromSlash(rel))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing unsafe path %q in stored bundle", rel)
		}
		dst := filepath.Join(outDir, clean)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			return err
		}
	}
	p.Line("pulled %q: %d files → %s", slug, len(resp.Files), outDir)
	return nil
}

// readBundleDir walks a bundle directory into the path→content map the
// bot-source API stores. Skips .git/ and Go test files; errors on the first
// non-UTF-8 file rather than corrupting it through the JSON string carrier.
func readBundleDir(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		b, berr := os.ReadFile(path)
		if berr != nil {
			return berr
		}
		if !utf8.Valid(b) {
			return fmt.Errorf("%s is not UTF-8 text — binary files cannot be stored in a platform override (drop it, or keep this bot baked)", rel)
		}
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}
