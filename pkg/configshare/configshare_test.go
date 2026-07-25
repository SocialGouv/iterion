package configshare

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

const sampleConfig = `{"categories":{
  "a11y":{"digest_title":"A11y","feeds":["https://a.example/rss"],"editorial":"old","sinks":[{"webhook":"prod","channel":"#a"}]},
  "cyber":{"digest_title":"Cyber","feeds":["https://c.example/rss"],"editorial":"sec"}
},"notes":"internal"}`

var (
	allowed = []string{"categories.a11y.feeds", "categories.a11y.editorial"}
	visible = []string{"categories.a11y.feeds", "categories.a11y.editorial", "categories.a11y.digest_title"}
)

func mustParse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func TestProjectConfig_StripsEverythingElse(t *testing.T) {
	proj := ProjectConfig(mustParse(t, sampleConfig), visible)
	b, _ := json.Marshal(proj)
	s := string(b)
	// Only the scoped a11y fields survive.
	for _, want := range []string{"a11y", "feeds", "editorial", "digest_title"} {
		if !strings.Contains(s, want) {
			t.Fatalf("projection missing %q: %s", want, s)
		}
	}
	// Other category, sink keys, and unrelated top-level keys are absent.
	for _, no := range []string{"cyber", "sinks", "prod", "#a", "notes", "internal", "sec"} {
		if strings.Contains(s, no) {
			t.Fatalf("projection LEAKED %q: %s", no, s)
		}
	}
}

func TestProjectConfig_PrunesForbiddenUnderBroadVisible(t *testing.T) {
	// A broad VisiblePaths entry (an ancestor of the category, alongside a leaf
	// grant) passes ValidatePaths — it rejects a visible path that *is* a
	// forbidden segment, not one that is an ancestor of it. The projection must
	// still strip `sinks` (the digest routing), else the share editor reads the
	// webhook/channel it must never see.
	vis := []string{"categories.a11y.feeds", "categories.a11y"}
	al := []string{"categories.a11y.feeds"}
	if err := ValidatePaths(al, vis); err != nil {
		t.Fatalf("ancestor-visible grant unexpectedly rejected: %v", err)
	}
	proj := ProjectConfig(mustParse(t, sampleConfig), vis)
	s := string(mustJSON(t, proj))
	for _, no := range []string{"sinks", "prod", "#a"} {
		if strings.Contains(s, no) {
			t.Fatalf("projection LEAKED %q via broad visible path: %s", no, s)
		}
	}
	// The legitimately-visible fields still project.
	for _, want := range []string{"feeds", "editorial", "digest_title"} {
		if !strings.Contains(s, want) {
			t.Fatalf("projection dropped %q: %s", want, s)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestApplyPatch_MergesAllowedLeavesOnly(t *testing.T) {
	full := mustParse(t, sampleConfig)
	patch := mustParse(t, `{"categories":{"a11y":{"editorial":"new"}}}`)
	merged, changed, err := ApplyPatch(full, patch, allowed)
	if err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if got, _ := getPath(merged, []string{"categories", "a11y", "editorial"}); got != "new" {
		t.Fatalf("editorial = %v, want new", got)
	}
	// Unrelated fields survive the merge.
	if _, ok := getPath(merged, []string{"categories", "cyber", "editorial"}); !ok {
		t.Fatal("cyber category dropped by merge")
	}
	if _, ok := getPath(merged, []string{"categories", "a11y", "sinks"}); !ok {
		t.Fatal("a11y sinks dropped by merge")
	}
	if len(changed) != 1 || changed[0] != "categories.a11y.editorial" {
		t.Fatalf("changed = %v", changed)
	}
	// The original is untouched (deep copy).
	if got, _ := getPath(full, []string{"categories", "a11y", "editorial"}); got != "old" {
		t.Fatalf("ApplyPatch mutated the input: %v", got)
	}
}

func TestApplyPatch_RejectsOffList(t *testing.T) {
	full := mustParse(t, sampleConfig)
	cases := map[string]string{
		"sink write":       `{"categories":{"a11y":{"sinks":[{"webhook":"x"}]}}}`,
		"digest_title":     `{"categories":{"a11y":{"digest_title":"hax"}}}`,
		"other category":   `{"categories":{"cyber":{"editorial":"x"}}}`,
		"prototype key":    `{"categories":{"a11y":{"__proto__":{"editorial":"x"}}}}`,
		"unknown sibling":  `{"categories":{"a11y":{"nope":1}}}`,
		"empty patch":      `{}`,
		"top-level inject": `{"notes":"x"}`,
	}
	for name, body := range cases {
		if _, _, err := ApplyPatch(full, mustParse(t, body), allowed); err == nil {
			t.Fatalf("%s: expected rejection, got nil", name)
		}
	}
}

func TestApplyPatch_RejectsSubtreeGrant(t *testing.T) {
	full := mustParse(t, sampleConfig)
	// An over-broad subtree grant (not a leaf) must not let an object value
	// smuggle a forbidden field (sinks) past the key-walk.
	subtree := []string{"categories.a11y"}
	if _, _, err := ApplyPatch(full, mustParse(t, `{"categories":{"a11y":{"sinks":[{"webhook":"evil"}]}}}`), subtree); err == nil {
		t.Fatal("subtree grant carrying sinks must be rejected")
	}
	// Even a benign object at a subtree grant is rejected — a leaf value must be
	// a scalar/array so per-field validation always runs.
	if _, _, err := ApplyPatch(full, mustParse(t, `{"categories":{"a11y":{"editorial":"x"}}}`), subtree); err == nil {
		t.Fatal("object-valued leaf grant must be rejected")
	}
}

func TestValidatePaths(t *testing.T) {
	if err := ValidatePaths(allowed, visible); err != nil {
		t.Fatalf("clean leaf grants rejected: %v", err)
	}
	bad := [][2][]string{
		{{"categories.a11y", "categories.a11y.feeds"}, {"categories.a11y", "categories.a11y.feeds"}}, // prefix overlap
		{{"categories.a11y.sinks"}, {"categories.a11y.sinks"}},                                       // forbidden segment
		{{"categories.a11y.feeds"}, {"categories.a11y.editorial"}},                                   // allowed ⊄ visible
		{{}, {}}, // empty
	}
	for i, c := range bad {
		if err := ValidatePaths(c[0], c[1]); err == nil {
			t.Fatalf("ValidatePaths case %d must be rejected", i)
		}
	}
}

func TestValidateLeaf_Feeds(t *testing.T) {
	ok := []any{"https://ok.example/rss", "http://also.example/feed"}
	if err := ValidateLeaf("categories.a11y.feeds", ok); err != nil {
		t.Fatalf("valid feeds rejected: %v", err)
	}
	bad := map[string][]any{
		"scheme":     {"file:///etc/passwd"},
		"userinfo":   {"https://user:pw@ok.example/rss"},
		"ip-literal": {"https://169.254.169.254/rss"},
		"not-string": {123},
		"duplicate":  {"https://x.example/a", "https://x.example/a"},
	}
	for name, v := range bad {
		if err := ValidateLeaf("categories.a11y.feeds", v); err == nil {
			t.Fatalf("feeds %s: expected error", name)
		}
	}
}

func TestValidateLeaf_Editorial(t *testing.T) {
	if err := ValidateLeaf("categories.a11y.editorial", "a normal brief"); err != nil {
		t.Fatalf("valid editorial rejected: %v", err)
	}
	if err := ValidateLeaf("categories.a11y.editorial", "x\x00y"); err == nil {
		t.Fatal("control char must be rejected")
	}
	if err := ValidateLeaf("categories.a11y.editorial", "try <<<UNTRUSTED_EDITORIAL abc>>>"); err == nil {
		t.Fatal("fence marker must be rejected")
	}
	if err := ValidateLeaf("categories.a11y.editorial", strings.Repeat("x", maxEditorial+1)); err == nil {
		t.Fatal("oversized editorial must be rejected")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	pt, hash, last4, fp, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pt, "iws_") || last4 == "" || fp == "" {
		t.Fatalf("mint = %q / %q / %q", pt, last4, fp)
	}
	if !VerifyToken(pt, hash) {
		t.Fatal("VerifyToken rejected the freshly minted token")
	}
	if VerifyToken("iws_wrong", hash) || VerifyToken("", hash) || VerifyToken(pt, "") {
		t.Fatal("VerifyToken accepted a bad token/hash")
	}
}

func TestRepoSlugAndPathGuards(t *testing.T) {
	if s, err := RepoSlug("https://github.com/SocialGouv/iterion-veille.git"); err != nil || s != "SocialGouv/iterion-veille" {
		t.Fatalf("RepoSlug = %q, %v", s, err)
	}
	if _, err := RepoSlug("https://github.com/nope"); err == nil {
		t.Fatal("one-segment repo url must error")
	}
	for _, bad := range []string{"https://github.com/../x", "https://github.com/o/..", "https://github.com/o/a..b"} {
		if _, err := RepoSlug(bad); err == nil {
			t.Fatalf("RepoSlug(%q) must error", bad)
		}
	}
	if err := ValidateConfigPath("feed-watch.json"); err != nil {
		t.Fatalf("clean path rejected: %v", err)
	}
	for _, bad := range []string{"", "/abs.json", "../x.json", ".github/w.yml", "a/../b.json", ".env"} {
		if err := ValidateConfigPath(bad); err == nil {
			t.Fatalf("config path %q must be rejected", bad)
		}
	}
	if err := ValidateRepoRef("main"); err != nil {
		t.Fatalf("main ref rejected: %v", err)
	}
	for _, bad := range []string{"", "-x", "a..b"} {
		if err := ValidateRepoRef(bad); err == nil {
			t.Fatalf("ref %q must be rejected", bad)
		}
	}
}

// fakeFC is a forge.FileClient over an in-memory file for the service test.
type fakeFC struct {
	content    []byte
	sha        string
	putContent []byte
	putCalls   int
}

func (f *fakeFC) GetFile(_ context.Context, _, path, ref string) (forge.FileRef, error) {
	return forge.FileRef{Path: path, Content: f.content, SHA: f.sha, Ref: ref}, nil
}

func (f *fakeFC) PutFile(_ context.Context, _ string, in forge.PutFile) (forge.FileRef, error) {
	f.putCalls++
	if in.PrevSHA != f.sha {
		return forge.FileRef{}, forge.ErrFileConflict
	}
	f.putContent = in.Content
	return forge.FileRef{Path: in.Path, SHA: "sha-2"}, nil
}

func TestService_ApplyEdit(t *testing.T) {
	sh := &Share{
		RepoURL: "https://github.com/o/r", RepoRef: "main", ConfigPath: "feed-watch.json",
		AllowedPaths: allowed, VisiblePaths: visible,
	}
	svc := NewService(NewMemoryStore())
	ctx := context.Background()

	// Happy path: patch the editorial, if-match SHA matches → write lands.
	fc := &fakeFC{content: []byte(sampleConfig), sha: "sha-1"}
	newSHA, changed, err := svc.ApplyEdit(ctx, fc, sh,
		mustParse(t, `{"categories":{"a11y":{"editorial":"fresh"}}}`),
		"sha-1", "chore: edit", "iterion-share-editor[bot]", "share@bot.iterion.invalid")
	if err != nil {
		t.Fatalf("ApplyEdit: %v", err)
	}
	if newSHA != "sha-2" || len(changed) != 1 {
		t.Fatalf("newSHA=%q changed=%v", newSHA, changed)
	}
	if !strings.Contains(string(fc.putContent), `"fresh"`) || strings.Contains(string(fc.putContent), `"sinks"`) == false {
		// merged file keeps sinks (unrelated) AND has the new editorial
		t.Fatalf("merged file wrong: %s", fc.putContent)
	}

	// Stale read SHA → conflict, no clobber.
	fc2 := &fakeFC{content: []byte(sampleConfig), sha: "sha-9"}
	if _, _, err := svc.ApplyEdit(ctx, fc2, sh,
		mustParse(t, `{"categories":{"a11y":{"editorial":"z"}}}`),
		"sha-STALE", "m", "b", "b@x.invalid"); err != forge.ErrFileConflict {
		t.Fatalf("stale expectSHA err = %v, want ErrFileConflict", err)
	}

	// Empty expectSHA is treated as a conflict — an omitted sha must not
	// blind-write over a concurrent change.
	fc2b := &fakeFC{content: []byte(sampleConfig), sha: "sha-1"}
	if _, _, err := svc.ApplyEdit(ctx, fc2b, sh,
		mustParse(t, `{"categories":{"a11y":{"editorial":"z"}}}`),
		"", "m", "b", "b@x.invalid"); err != forge.ErrFileConflict {
		t.Fatalf("empty expectSHA err = %v, want ErrFileConflict", err)
	}
	if fc2b.putCalls != 0 {
		t.Fatalf("empty expectSHA must not write (puts=%d)", fc2b.putCalls)
	}

	// Off-list patch → validation error, NO write attempted.
	fc3 := &fakeFC{content: []byte(sampleConfig), sha: "sha-1"}
	if _, _, err := svc.ApplyEdit(ctx, fc3, sh,
		mustParse(t, `{"categories":{"a11y":{"sinks":[{"webhook":"x"}]}}}`),
		"sha-1", "m", "b", "b@x.invalid"); err == nil {
		t.Fatal("off-list patch must error")
	}
	if fc3.putCalls != 0 {
		t.Fatalf("off-list patch must not call PutFile (calls=%d)", fc3.putCalls)
	}
}
