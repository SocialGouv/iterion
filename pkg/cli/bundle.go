package cli

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

//go:embed templates/bundle_bot.bot
var bundleStubBotIter []byte

//go:embed templates/bundle_manifest.yaml
var bundleStubManifest []byte

//go:embed templates/bundle_README.md
var bundleStubReadme []byte

// BundleInitResult is the JSON shape emitted by `iterion bundle init --json`.
type BundleInitResult struct {
	Dir          string   `json:"dir"`
	FilesCreated []string `json:"files_created"`
	FilesSkipped []string `json:"files_skipped"`
}

// RunBundleInit scaffolds a `.botz` source directory at dir. Existing
// files are left untouched (silent skip) so re-running init in a
// half-edited bundle never destroys work.
func RunBundleInit(dir string, p *Printer) error {
	if dir == "" {
		return fmt.Errorf("target directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create directory %q: %w", dir, err)
	}
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}

	var created, skipped []string

	files := []struct {
		path string
		body []byte
	}{
		{"bot.bot", bundleStubBotIter},
		{"manifest.yaml", bundleStubManifest},
		{"README.md", bundleStubReadme},
	}
	for _, f := range files {
		status, err := writeIfAbsent(filepath.Join(dir, f.path), f.body)
		if err != nil {
			return err
		}
		trackFile(&created, &skipped, f.path, status)
	}

	// Layout directories — `.gitkeep` makes them survive `git add`.
	for _, sub := range []string{"skills", "prompts", "attachments"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", sub, err)
		}
		gk := filepath.Join(sub, ".gitkeep")
		status, err := writeIfAbsent(filepath.Join(dir, gk), []byte{})
		if err != nil {
			return err
		}
		trackFile(&created, &skipped, gk, status)
	}

	// .gitignore tracks the same patterns the pack walker already
	// filters out, so users don't accidentally commit local builds.
	giStatus, err := updateGitignore(dir, []string{"*.botz", ".iterion/"})
	if err != nil {
		return err
	}
	trackFile(&created, &skipped, ".gitignore", giStatus)

	if p.Format == OutputJSON {
		p.JSON(BundleInitResult{
			Dir:          absDir,
			FilesCreated: created,
			FilesSkipped: skipped,
		})
		return nil
	}

	p.Header("Bundle init")
	p.KV("Directory", absDir)
	p.Blank()
	for _, f := range created {
		p.Line("  + %s", f)
	}
	for _, f := range skipped {
		p.Line("  ~ %s (already exists, skipped)", f)
	}
	p.Blank()
	p.Line("  Next steps:")
	p.Line("    1. Edit bot.bot to match your workflow.")
	p.Line("    2. Drop skills/prompts under skills/ and prompts/ (optional).")
	p.Line("    3. Build the archive:")
	p.Line("         iterion bundle pack %s", absDir)
	p.Blank()
	return nil
}

// BundlePackResult is the JSON shape emitted by `iterion bundle pack --json`.
type BundlePackResult struct {
	Output   string `json:"output"`
	Hash     string `json:"hash"`
	Entries  int    `json:"entries"`
	BytesIn  int64  `json:"bytes_in"`
	BytesOut int64  `json:"bytes_out"`
}

// RunBundlePack writes a deterministic `.botz` archive from srcDir.
//
//   - srcDir must be an existing directory containing `bot.iter`/`bot.bot`
//     at the root.
//   - outPath, when empty, defaults to "<srcDir>.botz" in srcDir's parent.
//   - force, when true, removes any existing output before packing.
//
// Reports the result via the printer in either human or JSON format.
func RunBundlePack(srcDir, outPath string, force bool, p *Printer) error {
	if srcDir == "" {
		return fmt.Errorf("source directory is required")
	}
	absSrc, err := filepath.Abs(srcDir)
	if err != nil {
		return fmt.Errorf("resolve source: %w", err)
	}
	if outPath == "" {
		base := filepath.Base(absSrc)
		outPath = filepath.Join(filepath.Dir(absSrc), base+".botz")
	}
	if force {
		_ = removeIfExists(outPath)
	}
	res, err := bundle.PackDir(absSrc, outPath)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(BundlePackResult{
			Output:   res.OutputPath,
			Hash:     res.Hash,
			Entries:  res.Entries,
			BytesIn:  res.BytesIn,
			BytesOut: res.BytesOut,
		})
		return nil
	}
	p.Header("Bundle: " + res.OutputPath)
	p.KV("Entries", fmt.Sprintf("%d", res.Entries))
	p.KV("Compressed", formatBytes(res.BytesOut))
	p.KV("Uncompressed", formatBytes(res.BytesIn))
	p.KV("SHA-256", res.Hash)
	p.Blank()
	p.Line("  result: OK")
	return nil
}

// formatBytes renders a byte count with a single-letter unit suffix.
// Bundles are small enough that we never need anything past MiB.
func formatBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// removeIfExists is used by --force to clear a stale output without
// caring whether it was already absent.
func removeIfExists(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(abs, ".botz") {
		// Defence in depth: --force only ever removes our own format.
		return fmt.Errorf("bundle pack: refusing to remove non-.botz %s", abs)
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
