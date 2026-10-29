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
//
// An anchor can LEAVE models.dev: a regeneration retired
// `scaleway/qwen3-coder-30b-a3b-instruct` (still published under
// `alibaba/`, which is another provider and must not answer for it — the
// no-cross-provider rule). A missed anchor is therefore not always a broken
// loader: re-anchor on an id the provider still publishes, and the
// deployment that needs the retired one declares it in
// OPENAI_COMPATIBLE_MODELS, whose entry wins over both models.dev tiers.
func TestSnapshotLookupExact_AnswersTheAnchorsExactly(t *testing.T) {
	for _, key := range []struct{ provider, model string }{
		{"scaleway", "gpt-oss-120b"},
		{"SCALEWAY", "GLM-5.2"},
		{"scaleway", "bge-multilingual-gemma2"},
	} {
		spec, ok := SnapshotLookupExact(key.provider, key.model)
		if !ok {
			t.Fatalf("SnapshotLookupExact(%q, %q) missed — the anchor left models.dev (re-anchor on an id the provider still publishes) or the loader broke", key.provider, key.model)
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

// The snapshot ANSWERS the flags models.dev publishes (schema 2): an id
// published without tool support reads a stated `tool_call:false` offline —
// the verdict a reader refuses on — where the numbers-only schema answered
// unknown, which reads as a warning. This is the end-to-end fact on the
// EMBEDDED file, not on a fixture: the loader must copy what the generator
// wrote.
func TestSnapshotLookupExact_AnswersTheFlagsModelsDevPublishes(t *testing.T) {
	embedding, ok := SnapshotLookupExact("scaleway", "bge-multilingual-gemma2")
	if !ok {
		t.Fatalf("the embedding anchor left the snapshot — re-anchor on an id models.dev publishes with tool_call:false")
	}
	if embedding.ToolCall == nil || *embedding.ToolCall {
		t.Errorf("an embedding model's tool_call = %s, want a stated false (the loader is dropping the flag)", flagWord(embedding.ToolCall))
	}
	if embedding.Reasoning == nil || *embedding.Reasoning {
		t.Errorf("an embedding model's reasoning = %s, want a stated false", flagWord(embedding.Reasoning))
	}
	if embedding.Temperature == nil || *embedding.Temperature {
		t.Errorf("an embedding model's temperature = %s, want a stated false", flagWord(embedding.Temperature))
	}
	// The numbers still answer: schema 2 ADDED fields, it replaced none.
	if embedding.ContextWindow != 8191 {
		t.Errorf("the embedding model's window = %d, want 8191", embedding.ContextWindow)
	}
	// The other verdict crosses too, so the test cannot pass by answering
	// false to everything.
	chat, ok := SnapshotLookupExact("scaleway", "gpt-oss-120b")
	if !ok {
		t.Fatalf("the chat anchor left the snapshot")
	}
	if chat.ToolCall == nil || !*chat.ToolCall {
		t.Errorf("a tool-calling model's tool_call = %s, want a stated true", flagWord(chat.ToolCall))
	}
}

// A flag the source did not state and a flag it stated FALSE are different
// answers, through the writer and the reader both: this is why the fields are
// pointers, and the round trip is where a writer that emits and a reader that
// drops would show. Driven by a literal file, so no upstream retirement can
// make it lie.
func TestSnapshotRoundTrip_AnUnstatedFlagIsNotAStatedFalse(t *testing.T) {
	body := []byte(`{"prov":{"models":{` +
		`"stated":{"limit":{"context":4096,"output":512},"cost":{"input":2,"output":8},"reasoning":false,"tool_call":false,"temperature":true},` +
		`"silent":{"limit":{"context":8192},"cost":{"input":1,"output":3}}}}}`)
	out, err := WriteSnapshot(body, "sha256:test", time.Now().UTC())
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	// The writer states a false and OMITS an unstated flag.
	if !strings.Contains(string(out), `"tool_call":false`) {
		t.Errorf("the writer dropped a stated false: %s", out)
	}
	if strings.Count(string(out), `"tool_call"`) != 1 {
		t.Errorf("the writer invented a flag for the row that states none: %s", out)
	}
	hdr, table, err := ReadSnapshot(out)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if hdr.Schema != SnapshotSchema {
		t.Errorf("the writer stamped schema %d, the reader expects %d", hdr.Schema, SnapshotSchema)
	}
	stated, silent := table["prov/stated"], table["prov/silent"]
	if stated.ToolCall == nil || *stated.ToolCall {
		t.Errorf("a stated false round-tripped as %s", flagWord(stated.ToolCall))
	}
	if stated.Temperature == nil || !*stated.Temperature {
		t.Errorf("a stated true round-tripped as %s", flagWord(stated.Temperature))
	}
	if silent.ToolCall != nil || silent.Reasoning != nil || silent.Temperature != nil {
		t.Errorf("an unstated flag round-tripped as a verdict: %s/%s/%s", flagWord(silent.Reasoning), flagWord(silent.ToolCall), flagWord(silent.Temperature))
	}
	if silent.ContextWindow != 8192 {
		t.Errorf("the silent row's window = %d, want 8192", silent.ContextWindow)
	}
}

// flagWord names an optional flag for a failure message: a verdict, or the
// absence of one.
func flagWord(f *bool) string {
	switch {
	case f == nil:
		return "unstated"
	case *f:
		return "true"
	default:
		return "false"
	}
}

// The three refusals the snapshot tier rests on, driven through the seam the
// loader itself takes (loadFrom). Each is a line the whole decision depends
// on and none had a witness: ADR-122's "schema 1 is not kept loadable" is an
// argument about THIS verdict, and a reader that accepts a schema-1 file
// serves its rows with every capability flag reading unknown, which is #2842
// re-opened with the suite green.
//
// A refused file answers NOTHING — not even the lines that parsed: half a
// table is a window and a price for some ids and silence for others, with no
// way to tell which.
func TestLoadFrom_RefusesWhatTheBinaryCannotRead(t *testing.T) {
	good, err := WriteSnapshot([]byte(`{"prov":{"models":{"m":{"limit":{"context":8191,"output":512},`+
		`"cost":{"input":1,"output":2},"tool_call":true}}}}`), "sha256:test", time.Now().UTC())
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	header, body, _ := strings.Cut(string(good), "\n")
	schemaOne := strings.Replace(header, `"schema":2`, `"schema":1`, 1)
	if schemaOne == header {
		t.Fatalf("the header does not carry the schema this case rewrites: %s", header)
	}

	for _, tc := range []struct {
		name string
		raw  string
		want string // a fragment of the refusal, naming what to do about it
	}{
		{"a schema this binary does not read", schemaOne + "\n" + body,
			"snapshot schema 1, want 2 — regenerate with `task models:snapshot`"},
		{"a header it cannot parse", "{not json\n" + body,
			"snapshot header:"},
		{"a line it cannot parse", header + "\n" + body + "{not json\n",
			"snapshot entry:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hdr, table, err := loadFrom([]byte(tc.raw))
			if err == nil {
				t.Fatalf("the file was accepted: header %+v, %d entries answer", hdr, len(table))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal reads %q, want it to name %q", err, tc.want)
			}
			if len(table) != 0 || hdr.Schema != 0 {
				t.Errorf("a refused file still answers: header %+v, %d entries", hdr, len(table))
			}
		})
	}

	// The control: the same bytes, unmodified, load and answer. A refusal
	// that refused everything would pass every case above.
	hdr, table, err := loadFrom(good)
	if err != nil {
		t.Fatalf("the file the writer just rendered was refused: %v", err)
	}
	if hdr.Schema != SnapshotSchema || len(table) != 1 {
		t.Fatalf("header %+v, %d entries, want schema %d and 1 entry", hdr, len(table), SnapshotSchema)
	}
	spec := table["prov/m"]
	if spec.ContextWindow != 8191 || spec.ToolCall == nil || !*spec.ToolCall {
		t.Fatalf("the accepted entry reads %+v tool_call=%s", spec, flagWord(spec.ToolCall))
	}
}

// loadSnapshot is the SITE: it is what SnapshotLookupExact serves from, and a
// verdict that only lived in loadFrom would leave the loader free to go back
// to ReadSnapshot alone. The embedded file is the one production reads, so
// the assertion is that the loader took the seam and the blob passes it.
func TestLoadSnapshot_ServesTheEmbeddedFileThroughTheSameVerdict(t *testing.T) {
	hdr, index, err := loadFrom([]byte(snapshotBlob))
	if err != nil {
		t.Fatalf("the embedded snapshot does not load: %v — regenerate with `task models:snapshot`", err)
	}
	served, count := SnapshotInfo()
	if served != hdr || count != len(index) {
		t.Fatalf("the loader serves header %+v / %d entries, the seam reads %+v / %d", served, count, hdr, len(index))
	}
	if count == 0 {
		t.Fatal("the embedded snapshot answers for no model: the probe lost its subject")
	}
}
