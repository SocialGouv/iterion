package server

import (
	"errors"
	"net/http"
	"os"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// unitNoteFor names, for the chip, why there is no program to price. The
// two notes are the client's whole contract (it hides on either), so they
// stay apart for the operator reading the response: a source that could not
// become one program is not a source that failed to parse, and a main whose
// fragments did not travel parses perfectly.
func unitNoteFor(err error) string {
	if errors.Is(err, runview.ErrInlineImport) {
		return "unit_not_loaded"
	}
	return "workflow_unparseable"
}

type previewCostRequest struct {
	FilePath     string   `json:"file_path,omitempty"`
	Source       string   `json:"source,omitempty"`
	Permission   string   `json:"permission,omitempty"`
	Backend      string   `json:"backend,omitempty"`
	BackendNames []string `json:"backend_names,omitempty"`
}

// CostMin == 0 with CostMax == 0 signals "no pricing data" so the
// studio can render "—" instead of "$0".
type previewCostNode struct {
	NodeID     string  `json:"node_id"`
	Kind       string  `json:"kind"`
	Model      string  `json:"model,omitempty"`
	Effort     string  `json:"effort,omitempty"`
	TokensIn   int     `json:"tokens_in"`
	TokensOut  int     `json:"tokens_out"`
	CostMinUSD float64 `json:"cost_min_usd"`
	CostMaxUSD float64 `json:"cost_max_usd"`
}

// Min/max bracket: min = best-case (no retries, no loops); max = 2x
// pessimistic fan-out factor. All cost numbers are best-effort hints
// — see pkg/backend/cost for the pricing-table caveats.
type previewCostResponse struct {
	TokensMin  int               `json:"tokens_min"`
	TokensMax  int               `json:"tokens_max"`
	CostMinUSD float64           `json:"cost_min_usd"`
	CostMaxUSD float64           `json:"cost_max_usd"`
	Nodes      []previewCostNode `json:"nodes"`
	Notes      []string          `json:"notes,omitempty"`
	// Effective reports the workflow's resolved knobs BELOW the
	// run-override level (node/workflow/env/default) so the Launch
	// dialog can caption each select with why the knob is what it is.
	// The client layers its own override on top (it owns the selects,
	// so "run_override" never appears here).
	Effective *previewEffectiveSettings `json:"effective,omitempty"`
	// BackendOptions is keyed by node id, then by the backend names the
	// Studio is actually offering. The server computes the exact launch-time
	// permission precedence and capability crossing so the picker cannot
	// drift from runtime admission.
	BackendOptions map[string]map[string]previewBackendOption `json:"backend_options,omitempty"`
}

type previewBackendOption struct {
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	Warning           string `json:"warning,omitempty"`
}

// previewEffectiveKnob is one knob's provenance below run-override.
type previewEffectiveKnob struct {
	// Effective value at this resolution ("auto" when nothing decides).
	Effective string `json:"effective"`
	// Source: "workflow" | "env" | "default".
	Source string `json:"source"`
	// NodePinned is true when at least one node sets its own value. Whether a
	// run setting wins that pin is knob-specific (permission/compress/memory
	// do; the default-backend setting does not).
	NodePinned bool `json:"node_pinned,omitempty"`
}

type previewEffectiveSettings struct {
	Compress       previewEffectiveKnob `json:"compress"`
	AutoMemory     previewEffectiveKnob `json:"auto_memory"`
	AmbientContext previewEffectiveKnob `json:"ambient_context"`
	Permission     previewEffectiveKnob `json:"permission"`
	Backend        previewEffectiveKnob `json:"backend"`
}

// Token envelopes are intentionally generous — the goal is to flag
// $5 workflows before launch, not to predict to the cent. Refresh
// when provider pricing or claw effort defaults shift.
var effortTokens = map[string]struct{ in, out int }{
	"none":   {2_000, 1_000},
	"low":    {3_000, 1_500},
	"medium": {12_000, 4_000},
	"high":   {32_000, 8_000},
	"xhigh":  {60_000, 12_000},
	"max":    {80_000, 16_000},
}

// A fresh workflow with no effort annotation should map to the mid-tier
// estimate, not the cheapest one.
var defaultEffortTokens = effortTokens["medium"]

func estimateNodeTokens(model, effort string, maxTokensHint int) (in, out int) {
	tier, ok := effortTokens[effort]
	if !ok {
		tier = defaultEffortTokens
	}
	in = tier.in
	// maxTokensHint == 0 means "backend default" (no cap), not zero.
	if maxTokensHint > 0 && maxTokensHint < tier.out {
		out = maxTokensHint
	} else {
		out = tier.out
	}
	_ = model
	return in, out
}

func (s *Server) handlePreviewCost(w http.ResponseWriter, r *http.Request) {
	var req previewCostRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// Inline source wins over file_path — matches POST /api/runs precedence.
	src := req.Source
	// The parse name decides where an include resolves: the file's absolute
	// path INSIDE THE WORKSPACE, never the request's own spelling of it — a
	// relative name is refused by the compiler, and an absolute one outside
	// the workspace is not accepted as an include base either (the request
	// would otherwise pick any directory on this host to read beside).
	// Anything else parses under a synthetic name, whose includes are
	// refused.
	parserPath := ""
	if req.FilePath != "" {
		if abs, err := s.safePath(req.FilePath); err == nil {
			parserPath = abs
		}
	}
	if src == "" && req.FilePath != "" {
		path, err := s.resolveWorkflowPath(req.FilePath, "")
		if err != nil {
			httpError(w, http.StatusBadRequest, "resolve workflow: %v", err)
			return
		}
		// The read proves the candidate is there and readable, with the 400
		// the client expects for a path that is not. Its BYTES are not the
		// program: the unit load below reads the file beside the fragments
		// its `import` lines reach, which is what a launch of this path
		// compiles.
		if _, err := os.ReadFile(path); err != nil {
			httpError(w, http.StatusBadRequest, "read workflow: %v", err)
			return
		}
		parserPath = path
	}
	if src == "" && parserPath == "" {
		httpError(w, http.StatusBadRequest, "missing source or file_path")
		return
	}
	if parserPath == "" {
		parserPath = "studio.bot"
	}

	// The preview prices the program a LAUNCH of this candidate compiles,
	// through the launch's own load: a bot in several files is its whole
	// unit, merged. Parsing the main by itself leaves the `import` lines
	// unresolved, which the compiler refuses as such (C030) — so the chip
	// answered "incomplete" and showed NOTHING for every multi-file bot,
	// the one shape whose price is worth previewing.
	bundleDir := ""
	if req.FilePath != "" {
		// A path the server cannot place is a label with no bundle behind
		// it, exactly as the launch reads it (launchBundleDirFor), and the
		// load falls back to the candidate's own bundle resolution. A bundle
		// that EXISTS and does not open is the launch's 422; here it is one
		// more reason there is no price to answer, and a chip that hides is
		// not where a broken manifest gets explained — /api/validate and the
		// Launch button are.
		if dir, derr := s.launchBundleDirFor(req.FilePath); derr == nil {
			bundleDir = dir
		}
	}
	u, _, err := runview.LoadLaunchUnit(parserPath, src, bundleDir)
	if err != nil || u == nil || u.Merged == nil {
		// Never 5xx on bad input — the chip silently hides and /api/validate
		// remains the authoritative blocker for the Launch button. A unit
		// that does not load is one note, not a parse verdict on a file that
		// may well parse: an inline main whose fragments did not travel is
		// refused by name, and the chip is not where that is explained.
		writeJSON(w, previewCostResponse{Notes: []string{unitNoteFor(err)}})
		return
	}
	// The MERGED program, every file's declarations in it — not the main's
	// own AST, whose `import` lines resolve against nothing. The bundle's
	// prompts are deliberately NOT merged here: a `system:` naming a
	// prompts/*.md leaves a C003 this endpoint's lenient verdict (a workflow
	// or nothing) does not read, so merging them would change no answer it
	// gives, and a line that changes no answer is one no test can hold.
	cr := ir.Compile(u.Merged)
	if cr.Workflow == nil {
		writeJSON(w, previewCostResponse{Notes: []string{"workflow_incomplete"}})
		return
	}

	resp := previewCostResponse{
		Effective: buildEffectiveSettings(cr.Workflow),
		BackendOptions: buildBackendOverrideOptions(
			cr.Workflow,
			req.Permission,
			req.Backend,
			req.BackendNames,
		),
	}
	hasPricing := false
	for _, node := range cr.Workflow.Nodes {
		var model, effort string
		var maxTok int
		var kind string
		switch n := node.(type) {
		case *ir.AgentNode:
			kind = "agent"
			model = n.Model
			effort = n.ReasoningEffort
			maxTok = n.MaxTokens
		case *ir.JudgeNode:
			kind = "judge"
			model = n.Model
			effort = n.ReasoningEffort
			maxTok = n.MaxTokens
		default:
			continue
		}
		effort = ir.ResolveEffortLiteral(effort)
		in, out := estimateNodeTokens(model, effort, maxTok)
		costPer := cost.EstimateUSD(model, in, out)
		if costPer > 0 {
			hasPricing = true
		}
		resp.Nodes = append(resp.Nodes, previewCostNode{
			NodeID:     node.NodeID(),
			Kind:       kind,
			Model:      model,
			Effort:     effort,
			TokensIn:   in,
			TokensOut:  out,
			CostMinUSD: costPer,
			// 2x bracket covers retries + plausible second-pass loops
			// without ballooning into worst-case-of-worst-case territory.
			CostMaxUSD: costPer * 2,
		})
		resp.TokensMin += in + out
		resp.TokensMax += (in + out) * 2
		resp.CostMinUSD += costPer
		resp.CostMaxUSD += costPer * 2
	}

	if len(resp.Nodes) == 0 {
		resp.Notes = append(resp.Notes, "no_llm_nodes")
	} else if !hasPricing {
		resp.Notes = append(resp.Notes, "no_pricing_data")
	}

	writeJSON(w, resp)
}
