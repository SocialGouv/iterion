package knowledge

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy/detector"
)

// ExportFormat identifies the memory archive format.
const ExportFormat = "iterion.memory.v1"

// ExportManifest is the archive's manifest.json.
type ExportManifest struct {
	Format    string         `json:"format"`
	Space     SpaceRef       `json:"space"`
	Documents []DocumentMeta `json:"documents"`
	DocCount  int            `json:"doc_count"`
	// Unaddressable lists documents the index reports but this build cannot
	// read, because an older one stored them under a path the shared clamp now
	// refuses (a non-canonical spelling). They are named rather than silently
	// omitted: an archive that quietly holds less than the space is the one
	// thing a backup must never be.
	Unaddressable []string `json:"unaddressable,omitempty"`
}

// ImportStrategy controls how an import treats a doc that already exists.
type ImportStrategy string

const (
	ImportSkip      ImportStrategy = "skip"      // default: never overwrite
	ImportOverwrite ImportStrategy = "overwrite" // replace (new revision)
	ImportRename    ImportStrategy = "rename"    // write under "<base>.import.<ext>"
)

// ImportSummary reports what an import did.
type ImportSummary struct {
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
	Renamed  int `json:"renamed"`
}

// ErrSecretInExport is returned by ExportSpace when a document body
// contains a literal credential shape. Exports must never leak secret
// plaintext — the operator cleans the space and re-exports.
type ErrSecretInExport struct {
	Path   string
	Reason string
}

func (e *ErrSecretInExport) Error() string {
	return fmt.Sprintf("knowledge: refusing to export %q: %s", e.Path, e.Reason)
}

// ExportSpace writes a gzip+tar archive of every markdown document in a
// space (manifest.json first, then docs/<path>, then checksums.sha256).
// It aborts with *ErrSecretInExport if any body contains a literal
// credential shape; symbolic __ITERION_SECRET_*__ placeholders pass.
func ExportSpace(ctx context.Context, store MemoryStore, ref SpaceRef, w io.Writer) (ExportManifest, error) {
	index, err := store.BuildIndex(ctx, ref)
	if err != nil {
		return ExportManifest{}, err
	}
	sort.Slice(index, func(i, j int) bool { return index[i].Path < index[j].Path })

	type entry struct {
		meta DocumentMeta
		body []byte
	}
	var docs []entry
	manifest := ExportManifest{Format: ExportFormat, Space: ref}
	for _, e := range index {
		doc, err := store.ReadDocument(ctx, ref, e.Path)
		if err != nil {
			// A path this build refuses is a legacy row, not a store fault:
			// one of them must not cost the operator the WHOLE archive, which
			// is exactly what aborting here did — a single `./note.md` written
			// by an older binary made the space unbackupable, healthy
			// documents included. Every other read error is a real failure and
			// still aborts.
			if errors.Is(err, ErrInvalidDocPath) {
				manifest.Unaddressable = append(manifest.Unaddressable, e.Path)
				continue
			}
			return ExportManifest{}, fmt.Errorf("export read %q: %w", e.Path, err)
		}
		if reason := ScanForSecret(doc.Content); reason != "" {
			return ExportManifest{}, &ErrSecretInExport{Path: e.Path, Reason: reason}
		}
		docs = append(docs, entry{meta: doc.Meta, body: doc.Content})
		manifest.Documents = append(manifest.Documents, doc.Meta)
	}
	manifest.DocCount = len(docs)

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	if err := writeTarFile(tw, "manifest.json", manifestBytes); err != nil {
		return ExportManifest{}, err
	}
	var checksums strings.Builder
	for _, d := range docs {
		if err := writeTarFile(tw, "docs/"+d.meta.Path, d.body); err != nil {
			return ExportManifest{}, err
		}
		fmt.Fprintf(&checksums, "%s  docs/%s\n", ChecksumHex(d.body), d.meta.Path)
	}
	if err := writeTarFile(tw, "checksums.sha256", []byte(checksums.String())); err != nil {
		return ExportManifest{}, err
	}
	if err := tw.Close(); err != nil {
		return ExportManifest{}, err
	}
	if err := gz.Close(); err != nil {
		return ExportManifest{}, err
	}
	return manifest, nil
}

// ImportSpace reads an archive produced by ExportSpace and writes its
// documents into ref. Document paths are written through the store's
// path-clamped WriteDocument, so a malicious "../" entry is rejected by
// the adapter. Bodies containing literal secret shapes are rejected.
func ImportSpace(ctx context.Context, store MemoryStore, ref SpaceRef, r io.Reader, strategy ImportStrategy) (ImportSummary, error) {
	if strategy == "" {
		strategy = ImportSkip
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return ImportSummary{}, fmt.Errorf("import: gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var sum ImportSummary
	// Match the per-document write cap so an oversized entry fails with a
	// clear size error here rather than a confusing late QuotaError from
	// WriteDocument deeper in the import.
	const maxEntry = DefaultMaxDocumentSize
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sum, fmt.Errorf("import: tar: %w", err)
		}
		name := path.Clean(hdr.Name)
		if !strings.HasPrefix(name, "docs/") {
			continue // manifest.json + checksums are advisory
		}
		rel := strings.TrimPrefix(name, "docs/")
		if rel == "" || strings.HasPrefix(rel, "..") || path.IsAbs(rel) {
			return sum, fmt.Errorf("import: unsafe path %q", hdr.Name)
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxEntry+1))
		if err != nil {
			return sum, fmt.Errorf("import read %q: %w", rel, err)
		}
		if int64(len(body)) > maxEntry {
			return sum, fmt.Errorf("import: %q exceeds %d bytes", rel, maxEntry)
		}
		if reason := ScanForSecret(body); reason != "" {
			return sum, &ErrSecretInExport{Path: rel, Reason: "import source " + reason}
		}

		dst := rel
		if _, err := store.ReadDocument(ctx, ref, rel); err == nil {
			switch strategy {
			case ImportSkip:
				sum.Skipped++
				continue
			case ImportRename:
				ext := path.Ext(rel)
				dst = strings.TrimSuffix(rel, ext) + ".import" + ext
				sum.Renamed++
			}
		}
		if _, err := store.WriteDocument(ctx, ref, DocumentInput{Path: dst, Content: body, UpdatedBy: "import"}); err != nil {
			return sum, fmt.Errorf("import write %q: %w", dst, err)
		}
		sum.Imported++
	}
	return sum, nil
}

func writeTarFile(tw *tar.Writer, name string, body []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(body)
	return err
}

// ScanForSecret returns a non-empty reason when content carries a credential,
// else "". Exported so every path that persists agent-authored bytes into a
// space — export archives and the auto-memory mirror alike — refuses the same
// shapes.
//
// It delegates to the repo's curated, gitleaks-derived rule catalogue rather
// than matching prefixes itself. Three hand-rolled generations of this
// function each failed in BOTH directions at once — a bare `strings.Contains`
// refused "task-runner" (because it contains "sk-"), and every attempt to
// carve out prose let a real key through behind a `_` or an ANSI colour
// sequence. The catalogue encodes each provider's published token SHAPE, which
// is the only thing that separates the two reliably.
//
// The bar is a STRUCTURAL match — a shape the provider publishes — not a
// generic entropy guess. A refusal here silently drops a note whose author is
// a model that will not notice, and blocks an export outright. minSecretScore
// sits just under 1.0 so that rules which are structural but carry a
// corroborating filter (a JWT's three base64 segments, an AWS secret key's
// entropy) still count; the purely-heuristic rules below it do not.
func ScanForSecret(content []byte) string {
	s := string(content)
	// The catalogue is ~30 regexes and a byte→rune table over the whole
	// document, and this runs on every changed document of every node. Almost
	// every one of them contains no credential at all, and a substring pass
	// settles that case two orders of magnitude faster (measured: 605ms vs
	// 1.9ms on a 2 MiB document). Every rule in the catalogue begins with one
	// of these literals, so a miss here is a real miss.
	if !mayContainSecret(s) {
		return ""
	}
	spans := secretDetector.Scan(s, detector.Options{
		Categories: []string{"secret"},
		MinScore:   minSecretScore,
	})
	if len(spans) == 0 {
		return ""
	}
	return "contains a literal credential token (" + spans[0].Rule + ")"
}

const minSecretScore = 0.95

// secretLiterals is the set of fixed substrings every catalogue rule's pattern
// must contain. It is a PRE-FILTER only: a hit still goes through the real
// rules, so the cost of an over-broad entry is a wasted scan, never a false
// refusal — but a MISSING one is a rule that never fires on this path, in
// silence and forever. TestMayContainSecret_CoversEveryCatalogueRule drives
// this list from the catalogue's own corpus rather than trusting the list to
// be kept in sync by hand; it caught Slack webhooks, whose URL carries none of
// the token prefixes.
// Every entry is LOWERCASE, and matching folds the document to match — see
// mayContainSecret. Enumerating casings by hand is what failed: the list
// carried "key", "Key" and "KEY" while the rule behind it is `(?i)`, so
// `aws_secret_kEy = "<40 chars>"` skipped the scan entirely and a real key
// was persisted. Three of the eight spellings of one three-letter word were
// missing, and nothing said so.
var secretLiterals = []string{
	"sk-", "sk_", "akia", "asia",
	"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_",
	"glpat-", "xox", "aiza", "npm_", "pypi-", "eyj",
	"-----begin", "service_account", "hooks.slack.com",
	// aws_secret_key is the one rule at this score floor with no literal
	// prefix of its own; it needs "key" somewhere.
	"key",
}

// mayContainSecret reports whether any catalogue rule could possibly match.
//
// Case-INSENSITIVE, uniformly. Three catalogue rules are `(?i)` and the rest
// are not, but folding everything is the safe asymmetry: a fixed-case marker
// matched case-insensitively costs a wasted scan the real rules then reject,
// whereas one casing missed on a `(?i)` rule is a credential that is never
// scanned at all — silently, and forever.
//
// The fold is one linear pass and only allocates when the document actually
// has upper-case in it; the scan it guards is ~30 regexes plus a rune table
// over the whole document, which is the cost this exists to avoid.
func mayContainSecret(s string) bool {
	folded := strings.ToLower(s)
	for _, lit := range secretLiterals {
		if strings.Contains(folded, lit) {
			return true
		}
	}
	return false
}

// secretDetector is stateless after construction and safe for concurrent use,
// so one instance serves every caller.
var secretDetector = detector.New()
