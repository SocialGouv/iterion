package knowledge

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy/detector"
)

// memStore is a minimal in-memory MemoryStore for exercising the
// export/import archive logic without a filesystem or Mongo.
type memStore struct {
	mu   sync.Mutex
	docs map[string][]byte
}

var _ MemoryStore = (*memStore)(nil)

func newMemStore() *memStore { return &memStore{docs: map[string][]byte{}} }

func (m *memStore) Root(ref SpaceRef) (string, error) { return "mem://" + ref.ID(), nil }

func (m *memStore) BuildIndex(context.Context, SpaceRef) ([]IndexEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	paths := make([]string, 0, len(m.docs))
	for p := range m.docs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]IndexEntry, 0, len(paths))
	for _, p := range paths {
		title, desc, tags := ParseMarkdownMeta(m.docs[p])
		out = append(out, IndexEntry{Path: p, Title: title, Description: desc, Tags: tags})
	}
	return out, nil
}

func (m *memStore) Autoload(context.Context, SpaceRef, []string) ([]AutoloadEntry, error) {
	return nil, nil
}

func (m *memStore) ListDocuments(context.Context, SpaceRef, string) ([]DocumentMeta, error) {
	return nil, nil
}

func (m *memStore) ReadDocument(_ context.Context, _ SpaceRef, path string) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	body, ok := m.docs[path]
	if !ok {
		return Document{}, fmt.Errorf("%w: %s", ErrDocNotFound, path)
	}
	return Document{
		Meta:    DocumentMeta{Path: path, Size: int64(len(body)), Checksum: ChecksumHex(body)},
		Content: append([]byte(nil), body...),
	}, nil
}

func (m *memStore) WriteDocument(_ context.Context, _ SpaceRef, in DocumentInput) (DocumentMeta, error) {
	// Mirror the real adapters: the write path is path-clamped.
	if err := ValidateDocPath(in.Path); err != nil {
		return DocumentMeta{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs[in.Path] = append([]byte(nil), in.Content...)
	return DocumentMeta{Path: in.Path, Size: int64(len(in.Content)), Checksum: ChecksumHex(in.Content), UpdatedBy: in.UpdatedBy}, nil
}

func (m *memStore) DeleteDocument(_ context.Context, _ SpaceRef, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.docs, path)
	return nil
}

func (m *memStore) UsageBytes(context.Context, SpaceRef) (int64, int64, error) { return 0, 0, nil }

func (m *memStore) get(path string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.docs[path]
	return string(b), ok
}

func testRef() SpaceRef {
	return SpaceRef{Visibility: VisibilityProject, ProjectID: "p1", Name: "findings"}
}

type tarEntry struct {
	name string
	body []byte
}

// buildArchive assembles a gzip+tar stream from ordered entries, for
// crafting malicious/edge-case import inputs.
func buildArchive(t *testing.T, entries []tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	ref := testRef()
	src := newMemStore()
	src.docs["b.md"] = []byte("# B\n\nsecond doc")
	src.docs["sub/a.md"] = []byte("---\ntitle: A\n---\nfirst doc")

	var buf bytes.Buffer
	manifest, err := ExportSpace(ctx, src, ref, &buf)
	if err != nil {
		t.Fatalf("ExportSpace: %v", err)
	}
	if manifest.Format != ExportFormat || manifest.DocCount != 2 {
		t.Errorf("manifest = %+v, want format %q, 2 docs", manifest, ExportFormat)
	}
	if len(manifest.Documents) != 2 || manifest.Documents[0].Path != "b.md" || manifest.Documents[1].Path != "sub/a.md" {
		t.Errorf("manifest docs not sorted by path: %+v", manifest.Documents)
	}

	// The archive layout is manifest.json, then docs/<path>, then checksums.
	gz, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	bodies := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		body := new(bytes.Buffer)
		if _, err := body.ReadFrom(tr); err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		bodies[hdr.Name] = body.Bytes()
	}
	wantNames := []string{"manifest.json", "docs/b.md", "docs/sub/a.md", "checksums.sha256"}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Errorf("archive entries = %v, want %v", names, wantNames)
	}
	var inArchive ExportManifest
	if err := json.Unmarshal(bodies["manifest.json"], &inArchive); err != nil {
		t.Fatalf("manifest.json in archive not valid JSON: %v", err)
	}
	if inArchive.Format != ExportFormat || inArchive.Space.Name != ref.Name {
		t.Errorf("embedded manifest = %+v", inArchive)
	}
	checkLines := strings.Split(strings.TrimSpace(string(bodies["checksums.sha256"])), "\n")
	if len(checkLines) != 2 || !strings.HasPrefix(checkLines[0], ChecksumHex([]byte("# B\n\nsecond doc"))) {
		t.Errorf("checksums.sha256 = %q", string(bodies["checksums.sha256"]))
	}

	dst := newMemStore()
	sum, err := ImportSpace(ctx, dst, ref, bytes.NewReader(buf.Bytes()), ImportSkip)
	if err != nil {
		t.Fatalf("ImportSpace: %v", err)
	}
	if sum.Imported != 2 || sum.Skipped != 0 || sum.Renamed != 0 {
		t.Errorf("summary = %+v, want 2 imported", sum)
	}
	if got, _ := dst.get("sub/a.md"); got != "---\ntitle: A\n---\nfirst doc" {
		t.Errorf("sub/a.md round-trip mismatch: %q", got)
	}
	if got, _ := dst.get("b.md"); got != "# B\n\nsecond doc" {
		t.Errorf("b.md round-trip mismatch: %q", got)
	}
}

func TestExportRefusesLiteralSecrets(t *testing.T) {
	ctx := context.Background()
	src := newMemStore()
	src.docs["notes.md"] = []byte("token: ghp_0123456789abcdefghijklmnopqrstuvwxyz") // the real PAT shape: ghp_ + 36

	_, err := ExportSpace(ctx, src, testRef(), &bytes.Buffer{})
	var secretErr *ErrSecretInExport
	if !errors.As(err, &secretErr) {
		t.Fatalf("err = %v, want *ErrSecretInExport", err)
	}
	if secretErr.Path != "notes.md" || !strings.Contains(secretErr.Reason, "github") {
		t.Errorf("secret error = %+v", secretErr)
	}

	// Symbolic iterion placeholders round-trip fine.
	src.docs["notes.md"] = []byte("use __ITERION_SECRET_GH_TOKEN__ here")
	if _, err := ExportSpace(ctx, src, testRef(), &bytes.Buffer{}); err != nil {
		t.Errorf("placeholder should export cleanly: %v", err)
	}
}

func TestScanForSecret(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"clean", "just some markdown", false},
		{"openai key", "OPENAI_API_KEY=sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2M3n4", true},
		// A real AWS access key id is exactly AKIA + 16 upper-case
		// alphanumerics. The fixture used to be an 11-character stand-in,
		// which the shape rule (added so "EMEA_ASIA_PACIFIC_REGION" stops
		// being refused) correctly does not treat as a key.
		{"aws akia mid-text", "arn AKIAIOSFODNN7EXAMPLE more", true},
		{"aws-shaped word in an identifier", "the EMEA_ASIA_PACIFIC_REGION constant", false},
		{"gitlab pat", "glpat-A1b2C3d4E5f6G7h8I9j0", true},
		{"placeholder ok", "__ITERION_SECRET_X__", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanForSecret([]byte(tc.content)) != ""
			if got != tc.want {
				t.Errorf("ScanForSecret(%q) hit = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

func TestImportStrategies(t *testing.T) {
	ctx := context.Background()
	ref := testRef()

	// Source archive holds a.md with "new" content.
	src := newMemStore()
	src.docs["a.md"] = []byte("new")
	var archive bytes.Buffer
	if _, err := ExportSpace(ctx, src, ref, &archive); err != nil {
		t.Fatal(err)
	}
	raw := archive.Bytes()

	seeded := func() *memStore {
		dst := newMemStore()
		dst.docs["a.md"] = []byte("old")
		return dst
	}

	t.Run("skip keeps existing", func(t *testing.T) {
		dst := seeded()
		sum, err := ImportSpace(ctx, dst, ref, bytes.NewReader(raw), ImportSkip)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Skipped != 1 || sum.Imported != 0 {
			t.Errorf("summary = %+v, want 1 skipped", sum)
		}
		if got, _ := dst.get("a.md"); got != "old" {
			t.Errorf("a.md = %q, want old", got)
		}
	})

	t.Run("empty strategy defaults to skip", func(t *testing.T) {
		dst := seeded()
		sum, err := ImportSpace(ctx, dst, ref, bytes.NewReader(raw), "")
		if err != nil {
			t.Fatal(err)
		}
		if sum.Skipped != 1 || sum.Imported != 0 {
			t.Errorf("summary = %+v, want 1 skipped", sum)
		}
	})

	t.Run("overwrite replaces", func(t *testing.T) {
		dst := seeded()
		sum, err := ImportSpace(ctx, dst, ref, bytes.NewReader(raw), ImportOverwrite)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Imported != 1 || sum.Skipped != 0 {
			t.Errorf("summary = %+v, want 1 imported", sum)
		}
		if got, _ := dst.get("a.md"); got != "new" {
			t.Errorf("a.md = %q, want new", got)
		}
	})

	t.Run("rename writes alongside", func(t *testing.T) {
		dst := seeded()
		sum, err := ImportSpace(ctx, dst, ref, bytes.NewReader(raw), ImportRename)
		if err != nil {
			t.Fatal(err)
		}
		// Characterization: a renamed doc counts in BOTH Renamed and Imported.
		if sum.Renamed != 1 || sum.Imported != 1 {
			t.Errorf("summary = %+v, want renamed=1 imported=1", sum)
		}
		if got, _ := dst.get("a.md"); got != "old" {
			t.Errorf("original clobbered: %q", got)
		}
		if got, ok := dst.get("a.import.md"); !ok || got != "new" {
			t.Errorf("a.import.md = %q ok=%v, want new", got, ok)
		}
	})
}

func TestImportRejectsSecretBody(t *testing.T) {
	archive := buildArchive(t, []tarEntry{
		{name: "docs/x.md", body: []byte("key AKIAIOSFODNN7EXAMPLE")}, // a real AWS key id is AKIA + 16
	})
	_, err := ImportSpace(context.Background(), newMemStore(), testRef(), archive, ImportSkip)
	var secretErr *ErrSecretInExport
	if !errors.As(err, &secretErr) {
		t.Fatalf("err = %v, want *ErrSecretInExport", err)
	}
	if secretErr.Path != "x.md" {
		t.Errorf("secret path = %q, want x.md", secretErr.Path)
	}
}

func TestImportPathHandling(t *testing.T) {
	ctx := context.Background()

	t.Run("non-doc entries ignored", func(t *testing.T) {
		dst := newMemStore()
		archive := buildArchive(t, []tarEntry{
			{name: "manifest.json", body: []byte("{}")},
			{name: "checksums.sha256", body: []byte("")},
			{name: "random.txt", body: []byte("noise")},
			{name: "docs/keep.md", body: []byte("ok")},
		})
		sum, err := ImportSpace(ctx, dst, testRef(), archive, ImportSkip)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Imported != 1 {
			t.Errorf("summary = %+v, want only docs/keep.md imported", sum)
		}
		if _, ok := dst.get("keep.md"); !ok {
			t.Error("keep.md missing")
		}
	})

	t.Run("traversal entry is silently skipped", func(t *testing.T) {
		// Characterization: "docs/../../x.md" path.Cleans to "../x.md",
		// loses the docs/ prefix and is skipped WITHOUT an error — safe,
		// but the operator gets no signal about the dropped entry.
		dst := newMemStore()
		archive := buildArchive(t, []tarEntry{
			{name: "docs/../../evil.md", body: []byte("x")},
		})
		sum, err := ImportSpace(ctx, dst, testRef(), archive, ImportSkip)
		if err != nil {
			t.Fatalf("err = %v, want silent skip", err)
		}
		if sum.Imported != 0 || len(dst.docs) != 0 {
			t.Errorf("traversal entry was imported: %+v docs=%v", sum, dst.docs)
		}
	})

	t.Run("dotdot-prefixed filename rejected", func(t *testing.T) {
		// Characterization: HasPrefix(rel, "..") also rejects a legitimate
		// filename like "..notes.md" (not actually traversal).
		archive := buildArchive(t, []tarEntry{
			{name: "docs/..notes.md", body: []byte("x")},
		})
		_, err := ImportSpace(ctx, newMemStore(), testRef(), archive, ImportSkip)
		if err == nil || !strings.Contains(err.Error(), "unsafe path") {
			t.Errorf("err = %v, want unsafe path error", err)
		}
	})

	t.Run("oversized entry rejected", func(t *testing.T) {
		big := bytes.Repeat([]byte("a"), int(DefaultMaxDocumentSize)+1)
		archive := buildArchive(t, []tarEntry{{name: "docs/big.md", body: big}})
		_, err := ImportSpace(ctx, newMemStore(), testRef(), archive, ImportSkip)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("err = %v, want size error", err)
		}
	})

	t.Run("not gzip", func(t *testing.T) {
		_, err := ImportSpace(ctx, newMemStore(), testRef(), strings.NewReader("plain text"), ImportSkip)
		if err == nil || !strings.Contains(err.Error(), "gzip") {
			t.Errorf("err = %v, want gzip error", err)
		}
	})
}

// A bare substring test is unusable on prose. These are notes an agent
// legitimately writes about its own work, and every one of them was refused
// before the prefix gained a word boundary and a minimum body length —
// silently dropping the note, with a model as the only witness.
func TestScanForSecret_ProseIsNotACredential(t *testing.T) {
	for _, content := range []string{
		"run the task-runner before shipping",
		"see the task-list in docs/",
		"GitHub PATs start with ghp_ — revoke them in settings",
		"the sk-modal component uses a portal",
		"prefixes to watch for: sk-, ghp_, glpat-",
		"ASIA is the region grouping used by the billing export",
		"the EMEA_ASIA_PACIFIC_REGION constant",
		"risk-register and desk-check are both overdue",
		"nothing sensitive at all here",
	} {
		if reason := ScanForSecret([]byte(content)); reason != "" {
			t.Errorf("refused a legitimate note %q: %s", content, reason)
		}
	}
}

// …and the real shapes must still be refused — including a secret pasted
// straight behind a separator or a word, which is how one actually arrives in
// a note. Scoping the mid-word skip to `sk-` alone (the only prefix that
// occurs inside English words) is what closed these; a blanket letter rule let
// every one of them through.
func TestScanForSecret_RealTokenShapesStillRefused(t *testing.T) {
	for _, content := range []string{
		// Every fixture is the provider's PUBLISHED shape. A truncated
		// stand-in certifies nothing — three of these used to "pass" against
		// a guard that was matching a bare prefix.
		"ANTHROPIC_API_KEY=sk-ant-api03-A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6",
		"token ghp_0123456789abcdefghijklmnopqrstuvwxyz",
		"aws id AKIAIOSFODNN7EXAMPLE in the runbook",
		"slack xoxb-1234567890-0987654321-A1b2C3d4E5f6G7h8I9j0K1l2",
		"glpat-A1b2C3d4E5f6G7h8I9j0",
		"github_pat_11ABCDEFGHIJKLMNOPQRST_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456",
		// An ANSI colour sequence must not hide a key: agents capture
		// coloured shell output routinely, and an SGR code ends in a letter.
		"\x1b[32msk-ant-api03-A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6\x1b[0m",
	} {
		if ScanForSecret([]byte(content)) == "" {
			t.Errorf("a credential-shaped string was NOT refused: %q", content)
		}
	}
}

// mayContainSecret decides whether a document is worth the full catalogue
// scan, so a rule whose literal is missing from it is a rule that never fires
// on this path — a silent, permanent miss. The shared positive corpus is the
// authority: every string the catalogue is meant to catch must survive the
// pre-filter.
func TestMayContainSecret_CoversEveryCatalogueRule(t *testing.T) {
	corpus, err := os.ReadFile("../backend/tool/privacy/detector/testdata/secrets_positive.txt")
	if err != nil {
		t.Fatalf("read the shared positive corpus: %v", err)
	}
	lines := make([]string, 0, 64)
	for _, line := range strings.Split(string(corpus), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}

	// Part 1 — the pre-filter must not hide anything from the scan this
	// package actually performs.
	//
	// Ask the CATALOGUE, not ScanForSecret: ScanForSecret consults the
	// pre-filter itself, so using it here would make the test agree with
	// whatever the pre-filter happens to do and certify nothing.
	for _, line := range lines {
		spans := secretDetector.Scan(line, detector.Options{
			Categories: []string{"secret"},
			MinScore:   minSecretScore,
		})
		if len(spans) == 0 {
			continue // not a secret-category entry, or below the score floor
		}
		if !mayContainSecret(line) {
			t.Errorf("the pre-filter would skip a document the catalogue catches (rule %s): %q", spans[0].Rule, line)
		}
	}

	// Part 2 — every rule must have a corpus line, or part 1 silently did not
	// check it. That is not hypothetical: aws_secret_key had no line, so
	// nothing noticed the pre-filter enumerated three of the eight casings of
	// "key" while the rule behind it is (?i) — a real 40-character key in
	// `aws_secret_kEy = "…"` skipped the scan entirely and was persisted into
	// a space every later run of the bot reads.
	//
	// Each rule is asked IN ISOLATION, because Scan merges overlapping spans
	// and keeps the first rule declared: ssh_private_key is entirely subsumed
	// by pem_private_key at equal score, so through the full catalogue it can
	// never appear and would read as "no corpus line" when the line is right
	// there.
	for _, r := range detector.BuiltinRules() {
		if r.Category() != "secret" {
			continue
		}
		alone := detector.NewWithRules([]detector.Rule{r})
		exercised := false
		for _, line := range lines {
			if len(alone.Scan(line, detector.Options{Categories: []string{"secret"}})) > 0 {
				exercised = true
				break
			}
		}
		if !exercised {
			t.Errorf("rule %q is never exercised by the positive corpus, so part 1 never checked it — add a line in the vendor's PUBLISHED shape to testdata/secrets_positive.txt", r.Name())
		}
	}
}
