package server

import (
	"net/http"
	"os"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

type previewCostRequest struct {
	FilePath string `json:"file_path,omitempty"`
	Source   string `json:"source,omitempty"`
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
}

// previewEffectiveKnob is one knob's provenance below run-override.
type previewEffectiveKnob struct {
	// Effective value at this resolution ("auto" when nothing decides).
	Effective string `json:"effective"`
	// Source: "workflow" | "env" | "default".
	Source string `json:"source"`
	// NodePinned is true when at least one node sets its OWN value —
	// a run override will not affect those nodes.
	NodePinned bool `json:"node_pinned,omitempty"`
}

type previewEffectiveSettings struct {
	Compress   previewEffectiveKnob `json:"compress"`
	Permission previewEffectiveKnob `json:"permission"`
	Backend    previewEffectiveKnob `json:"backend"`
}

// Token envelopes are intentionally generous — the goal is to flag
// $5 workflows before launch, not to predict to the cent. Refresh
// when provider pricing or claw effort defaults shift.
var effortTokens = map[string]struct{ in, out int }{
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
	parserPath := req.FilePath
	if src == "" && req.FilePath != "" {
		path, err := s.resolveWorkflowPath(req.FilePath, "")
		if err != nil {
			httpError(w, http.StatusBadRequest, "resolve workflow: %v", err)
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			httpError(w, http.StatusBadRequest, "read workflow: %v", err)
			return
		}
		src = string(data)
		parserPath = path
	}
	if src == "" {
		httpError(w, http.StatusBadRequest, "missing source or file_path")
		return
	}
	if parserPath == "" {
		parserPath = "studio.bot"
	}

	pr := parser.Parse(parserPath, src)
	if pr.File == nil {
		// Never 5xx on bad input — the chip silently hides and /api/validate
		// remains the authoritative blocker for the Launch button.
		writeJSON(w, previewCostResponse{Notes: []string{"workflow_unparseable"}})
		return
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		writeJSON(w, previewCostResponse{Notes: []string{"workflow_incomplete"}})
		return
	}

	resp := previewCostResponse{Effective: buildEffectiveSettings(cr.Workflow)}
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
