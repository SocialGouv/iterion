package server

import (
	"net/http"
	"testing"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

func strp(s string) *string { return &s }

// sampleGitMetaWithDiffs extends sampleGitMeta with persisted per-file diff
// content (inline before/after), so the diff fallbacks have something to serve.
func sampleGitMetaWithDiffs() *store.RunGitMeta {
	m := sampleGitMeta()
	c1 := "2222222222222222222222222222222222222222"
	head := "1111111111111111111111111111111111111111"
	m.FileDiffs = map[string]*store.RunFileDiff{
		"pkg/a.go": {Path: "pkg/a.go", Before: strp("package a\n"), After: strp("package a // edited\n")},
		"pkg/b.go": {Path: "pkg/b.go", After: strp("package b\n")}, // added
	}
	m.CommitFileDiffs = map[string]map[string]*store.RunFileDiff{
		c1:   {"pkg/a.go": {Path: "pkg/a.go", Before: strp("package a\n"), After: strp("package a // edited\n")}},
		head: {"pkg/b.go": {Path: "pkg/b.go", After: strp("package b\n")}},
	}
	return m
}

func TestRunFileDiff_PersistedFallback(t *testing.T) {
	srv, hs := newTestServer(t)
	seedCloudRunWithGitMeta(t, srv, "cloud-fdiff", sampleGitMetaWithDiffs())

	resp, err := http.Get(hs.URL + "/api/runs/cloud-fdiff/files/diff?path=pkg/a.go")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (not a 409)", resp.StatusCode)
	}
	var out gitlib.DiffPayload
	decodeJSONResp(t, resp, &out)
	if out.Before == nil || *out.Before != "package a\n" {
		t.Errorf("before = %v, want 'package a\\n'", out.Before)
	}
	if out.After == nil || *out.After != "package a // edited\n" {
		t.Errorf("after = %v", out.After)
	}
}

func TestRunFileDiff_PersistedFallback_AddedFile(t *testing.T) {
	srv, hs := newTestServer(t)
	seedCloudRunWithGitMeta(t, srv, "cloud-fdiff-add", sampleGitMetaWithDiffs())

	resp, err := http.Get(hs.URL + "/api/runs/cloud-fdiff-add/files/diff?path=pkg/b.go")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out gitlib.DiffPayload
	decodeJSONResp(t, resp, &out)
	if out.Before != nil {
		t.Errorf("before = %v, want nil for an added file", out.Before)
	}
	if out.After == nil || *out.After != "package b\n" {
		t.Errorf("after = %v", out.After)
	}
}

// A path with no persisted diff falls through to the 409 (unchanged behaviour),
// not a 500 or a bogus empty diff.
func TestRunFileDiff_PersistedFallback_UnknownPath(t *testing.T) {
	srv, hs := newTestServer(t)
	seedCloudRunWithGitMeta(t, srv, "cloud-fdiff-unknown", sampleGitMetaWithDiffs())

	resp, err := http.Get(hs.URL + "/api/runs/cloud-fdiff-unknown/files/diff?path=pkg/missing.go")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a path absent from the snapshot", resp.StatusCode)
	}
}

func TestRunCommitFileDiff_PersistedFallback(t *testing.T) {
	srv, hs := newTestServer(t)
	seedCloudRunWithGitMeta(t, srv, "cloud-cdiff", sampleGitMetaWithDiffs())

	c1 := "2222222222222222222222222222222222222222"
	resp, err := http.Get(hs.URL + "/api/runs/cloud-cdiff/commits/" + c1 + "/diff?path=pkg/a.go")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (not a 404)", resp.StatusCode)
	}
	var out gitlib.DiffPayload
	decodeJSONResp(t, resp, &out)
	if out.After == nil || *out.After != "package a // edited\n" {
		t.Errorf("after = %v", out.After)
	}
}

// A commit SHA outside the recorded range still 404s — the persisted fallback
// enforces the same in-range guard as the live path.
func TestRunCommitFileDiff_PersistedFallback_OutOfRange(t *testing.T) {
	srv, hs := newTestServer(t)
	seedCloudRunWithGitMeta(t, srv, "cloud-cdiff-oob", sampleGitMetaWithDiffs())

	bogus := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	resp, err := http.Get(hs.URL + "/api/runs/cloud-cdiff-oob/commits/" + bogus + "/diff?path=pkg/a.go")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an out-of-range sha", resp.StatusCode)
	}
}

// A truncated diff (content dropped for budget) resolves to an oversized
// placeholder, never a 409/404 — the panel still renders.
func TestRunFileDiff_PersistedFallback_Truncated(t *testing.T) {
	srv, hs := newTestServer(t)
	m := sampleGitMeta()
	m.FileDiffs = map[string]*store.RunFileDiff{
		"pkg/a.go": {Path: "pkg/a.go", Truncated: true},
	}
	m.DiffsTruncated = true
	seedCloudRunWithGitMeta(t, srv, "cloud-fdiff-trunc", m)

	resp, err := http.Get(hs.URL + "/api/runs/cloud-fdiff-trunc/files/diff?path=pkg/a.go")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out gitlib.DiffPayload
	decodeJSONResp(t, resp, &out)
	if !out.Oversized {
		t.Errorf("truncated diff = %+v, want Oversized placeholder", out)
	}
}
