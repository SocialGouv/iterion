package scripthost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/scripthost/toolindex"
)

func discoveryCatalog() []toolindex.Tool {
	return []toolindex.Tool{
		{Name: "read_file", Description: "Read a file from the workspace.", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		{Name: "list_dir", Description: "List the entries of a directory.", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		{Name: "weather", Description: "Tell the weather."},
	}
}

// THE witness of CM-3 through the real child: a program discovers the node's
// tools by what they do, reads a declaration, and calls the tool it found —
// and discovery itself costs the parent nothing, because the child answers
// it from the catalogue the request carried.
func TestAProgramDiscoversAToolAndCallsIt(t *testing.T) {
	t.Parallel()

	srv := &recordingServer{}
	h := toolHost(t, srv)
	h.Catalog = discoveryCatalog()

	resp, err := h.Run(context.Background(), `
		const hits = call.search("read a file");
		const decl = call.describe(hits[0].name);
		call(hits[0].name, { path: "/w/a.txt" });
		({ first: hits[0].name, summary: hits[0].summary, decl, n: hits.length })
	`, smallLimits())
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != "" {
		t.Fatalf("the program was refused: %s — %s", resp.Refusal, resp.Message)
	}
	var got struct {
		First, Summary, Decl string
		N                    int
	}
	if err := json.Unmarshal([]byte(resp.Value), &got); err != nil {
		t.Fatalf("value %q: %v", resp.Value, err)
	}
	if got.First != "read_file" || got.Summary != "Read a file from the workspace." {
		t.Fatalf("search answered %+v, want read_file first", got)
	}
	if !strings.Contains(got.Decl, `declare function call(name: "read_file", input: {`) {
		t.Fatalf("describe answered %q", got.Decl)
	}
	if got.N == 0 || got.N > toolindex.DefaultSearchLimit {
		t.Fatalf("search answered %d hits", got.N)
	}
	seen := srv.seen()
	if len(seen) != 1 || seen[0].Name != "read_file" {
		t.Fatalf("the parent saw %+v: discovery must cost no call, and the found tool must be called once", seen)
	}
}

// An unknown name is a catchable TypeError that points at search, not an
// undefined the program would pass to call().
func TestDescribingAnUnknownToolThrowsAndPointsAtSearch(t *testing.T) {
	t.Parallel()

	h := toolHost(t, &recordingServer{})
	h.Catalog = discoveryCatalog()
	resp, err := h.Run(context.Background(), `
		let msg = "";
		try { call.describe("nope"); } catch (e) { msg = (e instanceof TypeError) + ":" + e.message; }
		msg
	`, smallLimits())
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if !strings.Contains(resp.Value, `true:`) || !strings.Contains(resp.Value, "call.search finds one") {
		t.Fatalf("value = %s (%s %s)", resp.Value, resp.Refusal, resp.Message)
	}
}

// No catalogue is an empty index, never an absent method: a program that
// searches a node with nothing to find gets an empty array.
func TestWithoutACatalogueSearchAnswersNothing(t *testing.T) {
	t.Parallel()

	resp, err := toolHost(t, &recordingServer{}).Run(context.Background(),
		`[typeof call.search, typeof call.describe, call.search("anything").length]`, smallLimits())
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Value != `["function","function",0]` {
		t.Fatalf("value = %s (%s %s)", resp.Value, resp.Refusal, resp.Message)
	}
}

// Host.Run bounds the catalogue itself: a caller handing it one past the
// tool bound cannot make the child read past what its request bound reserves.
// The tools past the bound are the last by name, and the child does not know
// them.
func TestRunBoundsTheCatalogueItSends(t *testing.T) {
	t.Parallel()

	var cat []toolindex.Tool
	for i := 0; i < toolindex.MaxTools+5; i++ {
		cat = append(cat, toolindex.Tool{Name: fmt.Sprintf("tool%04d", i)})
	}
	h := toolHost(t, &recordingServer{})
	h.Catalog = cat
	resp, err := h.Run(context.Background(), fmt.Sprintf(`
		let past = "described";
		try { call.describe("tool%04d"); } catch (e) { past = "unknown"; }
		[typeof call.describe("tool0000"), past]
	`, toolindex.MaxTools+4), smallLimits())
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Value != `["string","unknown"]` {
		t.Fatalf("value = %s (%s %s): the child must know the tools under the bound and not the ones past it", resp.Value, resp.Refusal, resp.Message)
	}
}

// The request is written in the catalogue bound's encoding (no HTML
// escaping): a catalogue of `<` at its bounds — 1 MiB of schemas and 40
// descriptions of 2000 bytes — must reach the child. HTML-escaped, the same
// catalogue is six-fold and overflows what the child reads.
func TestACatalogueOfHTMLCharactersReachesTheChild(t *testing.T) {
	t.Parallel()

	var cat []toolindex.Tool
	for i := 0; i < 40; i++ {
		cat = append(cat, toolindex.Tool{
			Name:        fmt.Sprintf("t%02d", i),
			Description: strings.Repeat("<", 2000),
			InputSchema: json.RawMessage(`{"type":"object","description":"` + strings.Repeat("<", 30<<10) + `"}`),
		})
	}
	h := toolHost(t, &recordingServer{})
	h.Catalog = cat
	resp, err := h.Run(context.Background(), `call.describe("t00").length > 0`, smallLimits())
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Value != "true" {
		t.Fatalf("value = %q (%s %s)", resp.Value, resp.Refusal, resp.Message)
	}
}

// ScriptBytes bounds the raw source, but JSON escapes a control character
// six-fold: a script under its bound can encode past what the child reads
// before it is confined. That is the script's size and is refused as such,
// before any process exists — not a truncated request the model is told is a
// broken deployment.
func TestAScriptThatEncodesPastTheChildsReadIsRefusedBeforeTheSpawn(t *testing.T) {
	t.Parallel()

	src := "1//" + strings.Repeat("\x01", 1100<<10)
	limits := smallLimits()
	limits.ScriptBytes = 2 << 20
	resp, err := Host{Binary: "/nonexistent/never-spawned"}.Run(context.Background(), src, limits)
	if err != nil {
		t.Fatalf("host: %v — the refusal must come before any spawn", err)
	}
	if resp.Refusal != RefusalScriptBytes || !strings.Contains(resp.Message, "encodes to a") {
		t.Fatalf("refusal = %q %q, want %q naming the encoded size", resp.Refusal, resp.Message, RefusalScriptBytes)
	}
}
