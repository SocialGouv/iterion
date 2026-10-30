package server

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimalAgentSource = `agent draft:
  model: "anthropic/claude-sonnet-4-6"
  reasoning_effort: medium

workflow main:
  entry: draft
  draft -> done
`

const tinyJudgeSource = `judge verify:
  model: "openai/gpt-5.4-mini"
  reasoning_effort: low
  max_tokens: 256

workflow main:
  entry: verify
  verify -> done
`

const toolOnlySource = `tool echo:
  command: "echo hi"

workflow main:
  entry: echo
  echo -> done
`

func postPreviewCost(t *testing.T, hs string, body string) previewCostResponse {
	t.Helper()
	resp, err := http.Post(hs+"/api/runs/preview-cost", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got previewCostResponse
	decodeJSONResp(t, resp, &got)
	return got
}

func TestPreviewCost_AgentNode(t *testing.T) {
	_, hs := newTestServer(t)
	got := postPreviewCost(t, hs.URL, `{"source":`+jsonString(minimalAgentSource)+`}`)
	if len(got.Nodes) != 1 {
		t.Fatalf("expected 1 LLM node, got %d", len(got.Nodes))
	}
	n := got.Nodes[0]
	if n.Kind != "agent" || n.NodeID != "draft" {
		t.Fatalf("unexpected node: %+v", n)
	}
	if n.TokensIn == 0 || n.TokensOut == 0 {
		t.Fatalf("expected non-zero token estimate: %+v", n)
	}
	if got.CostMinUSD <= 0 || got.CostMaxUSD <= got.CostMinUSD {
		t.Fatalf("expected positive monotone cost bracket, got min=%v max=%v", got.CostMinUSD, got.CostMaxUSD)
	}
}

func TestPreviewCost_JudgeRespectsMaxTokens(t *testing.T) {
	_, hs := newTestServer(t)
	got := postPreviewCost(t, hs.URL, `{"source":`+jsonString(tinyJudgeSource)+`}`)
	if len(got.Nodes) != 1 {
		t.Fatalf("expected 1 LLM node, got %d", len(got.Nodes))
	}
	n := got.Nodes[0]
	if n.Kind != "judge" {
		t.Fatalf("expected judge kind, got %q", n.Kind)
	}
	if n.TokensOut > 256 {
		t.Fatalf("max_tokens 256 should cap output estimate, got %d", n.TokensOut)
	}
}

func TestPreviewCost_NoLLMNodes(t *testing.T) {
	_, hs := newTestServer(t)
	got := postPreviewCost(t, hs.URL, `{"source":`+jsonString(toolOnlySource)+`}`)
	if len(got.Nodes) != 0 {
		t.Fatalf("expected zero LLM nodes for tool-only workflow")
	}
	if !containsNote(got.Notes, "no_llm_nodes") {
		t.Fatalf("expected no_llm_nodes note, got %+v", got.Notes)
	}
}

func TestPreviewCost_UnparseableSource(t *testing.T) {
	_, hs := newTestServer(t)
	got := postPreviewCost(t, hs.URL, `{"source":"not a valid iter file ::"}`)
	// Endpoint must never 500 on garbage — chip stays hidden, Launch
	// still fails at /api/validate. See cost_preview.go handler header.
	if got.CostMinUSD != 0 || got.CostMaxUSD != 0 {
		t.Fatalf("expected zero cost on unparseable input, got %+v", got)
	}
}

func TestPreviewCost_MissingBoth(t *testing.T) {
	_, hs := newTestServer(t)
	resp, err := http.Post(hs.URL+"/api/runs/preview-cost", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// jsonString escapes a Go string for embedding inside a JSON literal.
// Avoids pulling encoding/json just for two test bodies.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func containsNote(notes []string, want string) bool {
	for _, n := range notes {
		if n == want {
			return true
		}
	}
	return false
}

// The main of a bot in several files: it imports the fragment that declares
// the second LLM node, and the fragment's own reasoning_effort differs from
// the main's so that the estimate cannot be right by accident.
const unitMainSource = `import "lib/review.bot"

agent draft:
  model: "anthropic/claude-sonnet-4-6"
  reasoning_effort: medium

workflow main:
  entry: draft
  draft -> polish -> done
`

// The fragment's node names a prompt the BUNDLE ships (prompts/*.md) rather
// than one the text declares — the shape a real bot in several files has.
// The reference resolves only because the preview merges the bundle's
// prompts the way the launch does: without the merge it is a C003, the
// launch refuses the program, and the two whole-unit previews below answer
// workflow_errors instead of a price — which is what makes the merge a
// line a test holds.
const unitFragmentSource = `agent polish:
  model: "anthropic/claude-sonnet-4-6"
  reasoning_effort: high
  system: polish_system
`

const unitBundlePrompt = "Polish the draft without changing its meaning.\n"

// writeUnitBot lays a bot in several files under the server's workdir and
// answers the file_path a client names it by. The skills/ directory is what
// makes the directory a BUNDLE (bundle.DirForMainBot): a bot in several
// files is a bot with a directory, and that is the directory the server
// places from the path when the client sends the main's text inline.
func writeUnitBot(t *testing.T, workDir string) string {
	t.Helper()
	dir := filepath.Join(workDir, "pricedbot")
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir prompts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "polish_system.md"), []byte(unitBundlePrompt), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(unitMainSource), 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "review.bot"), []byte(unitFragmentSource), 0o644); err != nil {
		t.Fatalf("write fragment: %v", err)
	}
	return "pricedbot/main.bot"
}

func nodeByID(t *testing.T, got previewCostResponse, id string) previewCostNode {
	t.Helper()
	for _, n := range got.Nodes {
		if n.NodeID == id {
			return n
		}
	}
	t.Fatalf("no node %q in the preview: %+v (notes %v)", id, got.Nodes, got.Notes)
	return previewCostNode{}
}

// The preview of a bot in several files prices the WHOLE unit — the main's
// nodes and the fragments' — because it reads the program a launch of the
// same candidate compiles. Parsing the main alone leaves its `import` lines
// unresolved, which the compiler refuses as such (C030): the chip then has
// no workflow at all and shows nothing for the one shape whose price is
// worth previewing (#1648).
func TestPreviewCost_PricesTheWholeUnitBehindAnInlineMain(t *testing.T) {
	s, hs := newTestServer(t)
	fp := writeUnitBot(t, s.cfg.WorkDir)

	got := postPreviewCost(t, hs.URL, `{"file_path":`+jsonString(fp)+`,"source":`+jsonString(unitMainSource)+`}`)
	if len(got.Nodes) != 2 {
		t.Fatalf("expected the main's node AND the fragment's, got %d: %+v (notes %v)", len(got.Nodes), got.Nodes, got.Notes)
	}
	// The fragment's declaration is READ, not inferred: its own effort tier
	// decides its token envelope, and it is not the main's.
	polish := nodeByID(t, got, "polish")
	draft := nodeByID(t, got, "draft")
	if polish.TokensIn != effortTokens["high"].in {
		t.Errorf("the fragment's node is not priced at its own effort: tokens_in=%d, want %d", polish.TokensIn, effortTokens["high"].in)
	}
	if draft.TokensIn != effortTokens["medium"].in {
		t.Errorf("the main's node lost its own effort: tokens_in=%d, want %d", draft.TokensIn, effortTokens["medium"].in)
	}
	if got.TokensMin != draft.TokensIn+draft.TokensOut+polish.TokensIn+polish.TokensOut {
		t.Errorf("the total does not carry both nodes: tokens_min=%d", got.TokensMin)
	}
	if len(got.Notes) != 0 {
		t.Errorf("a priced unit carries no note, got %v", got.Notes)
	}
}

// The same unit named by PATH alone, with no inline buffer: the fragments
// are read beside the main on disk, as a path-driven launch reads them.
func TestPreviewCost_PricesTheWholeUnitFromItsPath(t *testing.T) {
	s, hs := newTestServer(t)
	fp := writeUnitBot(t, s.cfg.WorkDir)

	got := postPreviewCost(t, hs.URL, `{"file_path":`+jsonString(fp)+`}`)
	if len(got.Nodes) != 2 {
		t.Fatalf("expected the whole unit from its path, got %d: %+v (notes %v)", len(got.Nodes), got.Nodes, got.Notes)
	}
	if n := nodeByID(t, got, "polish"); n.TokensIn != effortTokens["high"].in {
		t.Errorf("the fragment's node is not priced at its own effort: tokens_in=%d", n.TokensIn)
	}
}

// A main handed over with NO directory behind it — an inline buffer whose
// fragments did not travel — is named as such, not reported as a text that
// failed to parse: it parses perfectly, and no price can be honest for a
// program whose pieces are missing.
func TestPreviewCost_InlineMainWhoseFragmentsDidNotTravelIsNamed(t *testing.T) {
	_, hs := newTestServer(t)

	got := postPreviewCost(t, hs.URL, `{"source":`+jsonString(unitMainSource)+`}`)
	if len(got.Nodes) != 0 {
		t.Fatalf("a unit main alone is priced: %+v", got.Nodes)
	}
	if len(got.Notes) != 1 || got.Notes[0] != "unit_not_loaded" {
		t.Fatalf("notes = %v, want [unit_not_loaded]", got.Notes)
	}
}

// A request that DID name a file_path is never answered "missing source or
// file_path": a real path whose file holds no workflow is incomplete, and
// the client is told which of the two it is. The old handler read the
// candidate's bytes and judged emptiness as an absent field.
func TestPreviewCost_AnEmptyFileAtARealPathIsIncompleteNotMissing(t *testing.T) {
	s, hs := newTestServer(t)
	if err := os.WriteFile(filepath.Join(s.cfg.WorkDir, "empty.bot"), nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp, err := http.Post(hs.URL+"/api/runs/preview-cost", "application/json", bytes.NewReader([]byte(`{"file_path":"empty.bot"}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a path that exists and carries no workflow", resp.StatusCode)
	}
	var got previewCostResponse
	decodeJSONResp(t, resp, &got)
	if len(got.Notes) != 1 || got.Notes[0] != "workflow_incomplete" {
		t.Fatalf("notes = %v, want [workflow_incomplete]", got.Notes)
	}
}

// writeBundleBot lays a bundle-shaped bot under the server's workdir (the
// skills/ marker is what makes the directory a bundle, as in writeUnitBot)
// and answers the file_path a client names its main by.
func writeBundleBot(t *testing.T, workDir, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(workDir, name)
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	return name + "/main.bot"
}

// A node naming a prompt neither the text nor the bundle's prompts/ declare
// compiles to a C003 the LAUNCH refuses: the preview answers no price and
// says so, where the one-bit verdict (is there a workflow) priced it — a
// workflow survives its own compile errors (#2814). Muting the diagnostics
// read prices the node again, red on the note.
func TestPreviewCost_AProgramTheLaunchRefusesIsNotPriced(t *testing.T) {
	s, hs := newTestServer(t)
	fp := writeBundleBot(t, s.cfg.WorkDir, "brokenprompt", map[string]string{
		"main.bot":         unitMainSource,
		"lib/review.bot":   strings.Replace(unitFragmentSource, "system: polish_system", "system: never_shipped", 1),
		"prompts/other.md": "An unrelated prompt the merge does carry.\n",
	})

	got := postPreviewCost(t, hs.URL, `{"file_path":`+jsonString(fp)+`}`)
	if len(got.Nodes) != 0 {
		t.Fatalf("a program the launch refuses is priced: %+v", got.Nodes)
	}
	if !containsNote(got.Notes, "workflow_errors") {
		t.Fatalf("notes = %v, want workflow_errors", got.Notes)
	}
	if got.CostMinUSD != 0 || got.CostMaxUSD != 0 {
		t.Fatalf("a refused program carries a cost: %+v", got)
	}
}

// The load's own errors are the launch's first refusal (compileLoadedUnit):
// a fragment the import cannot read leaves the merged program short of its
// declarations, and the chip prices nothing even when what remains compiles
// — the price would describe a program that cannot run.
func TestPreviewCost_AUnitTheLoadRefusesIsNotPriced(t *testing.T) {
	s, hs := newTestServer(t)
	fp := writeBundleBot(t, s.cfg.WorkDir, "missingfragment", map[string]string{
		"main.bot": `import "lib/missing.bot"

agent draft:
  model: "anthropic/claude-sonnet-4-6"
  reasoning_effort: medium

workflow main:
  entry: draft
  draft -> done
`,
	})

	got := postPreviewCost(t, hs.URL, `{"file_path":`+jsonString(fp)+`}`)
	if len(got.Nodes) != 0 {
		t.Fatalf("a unit the load refuses is priced: %+v", got.Nodes)
	}
	if !containsNote(got.Notes, "workflow_errors") {
		t.Fatalf("notes = %v, want workflow_errors", got.Notes)
	}
}
