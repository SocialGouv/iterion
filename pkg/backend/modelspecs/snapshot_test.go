package modelspecs

import (
	"strings"
	"testing"
	"time"
)

// The embedded snapshot answers exact lookups — after the same
// lower-casing every lookup here applies — for the anchors the gateway
// deployments actually serve, and offers NO fallback to a bare id or a
// suffix: exact means exact.
func TestSnapshotLookupExact_AnswersTheAnchorsExactly(t *testing.T) {
	for _, key := range []struct{ provider, model string }{
		{"scaleway", "gpt-oss-120b"},
		{"SCALEWAY", "GLM-5.2"},
		{"scaleway", "qwen3-coder-30b-a3b-instruct"},
	} {
		spec, ok := SnapshotLookupExact(key.provider, key.model)
		if !ok {
			t.Fatalf("SnapshotLookupExact(%q, %q) missed — the anchor is gone from the snapshot", key.provider, key.model)
		}
		if spec.ContextWindow <= 0 {
			t.Errorf("%s/%s carries no window: %+v", key.provider, key.model, spec)
		}
	}
	if _, ok := SnapshotLookupExact("scaleway", "gpt-oss"); ok {
		t.Error("a suffix matched — exact means exact")
	}
	if _, ok := SnapshotLookupExact("gpt-oss-120b", ""); ok {
		t.Error("a bare id matched")
	}
}

// The snapshot is fresh by construction (regenerated with the slice) and
// its header parses.
func TestSnapshotInfo_ReadsTheHeader(t *testing.T) {
	hdr, count := SnapshotInfo()
	if hdr.Schema != SnapshotSchema || hdr.Source != Source || hdr.SourceDigest == "" || hdr.AsOf == "" {
		t.Fatalf("header = %+v", hdr)
	}
	if count < 1000 {
		t.Errorf("%d entries — the snapshot looks truncated", count)
	}
	// The AGE itself is never asserted: the snapshot is a committed
	// artifact with a fixed as_of, and a wall-clock assertion here is a
	// calendar time bomb (ADR-122 rejected that class). Freshness is the
	// runtime's 180-day warn + `task models:snapshot` at slice time, both
	// visible in review — not a test that reds tomorrow.
	if _, ok := SnapshotAge(); !ok {
		t.Error("SnapshotAge could not parse the header's as_of")
	}
}

// WriteSnapshot renders what SnapshotLookupExact reads, canonically:
// header first, one sorted line per entry.
func TestWriteSnapshot_RoundTrips(t *testing.T) {
	body := []byte(`{"prov":{"models":{"m1":{"limit":{"context":4096,"output":512},"cost":{"input":2,"output":8}}}}}`)
	out, err := WriteSnapshot(body, "sha256:test", time.Now().UTC())
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want header + 1 entry", len(lines))
	}
	if !strings.Contains(lines[1], `"key":"prov/m1"`) || !strings.Contains(lines[1], `"context_window":4096`) {
		t.Errorf("entry = %s", lines[1])
	}
}
