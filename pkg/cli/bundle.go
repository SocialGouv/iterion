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
//   - srcDir must be an existing directory containing `main.bot` at the root.
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
