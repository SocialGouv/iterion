package server

import (
	"net/http"
	"os"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// previewCostRequest is the studio Launch-form payload for the inline
// cost-estimate chip. The handler parses the .iter source (preferring
// the inline body when supplied — same precedence as POST /api/runs),
// walks the IR for agent/judge nodes, and estimates a USD range using
// effort tiers + the backend/cost pricing table.
type previewCostRequest struct {
	FilePath string `json:"file_path,omitempty"`
	Source   string `json:"source,omitempty"`
}

// previewCostNode is the per-node breakdown displayed under the chip
// tooltip. CostMin == 0 with CostMax == 0 signals "no pricing data" so
// the studio can render "—" instead of "$0".
type previewCostNode struct {
	NodeID     string  `json:"node_id"`
	Kind       string  `json:"kind"`             // "agent" | "judge"
	Model      string  `json:"model,omitempty"`  // resolved model identifier
	Effort     string  `json:"effort,omitempty"` // "low"|"medium"|"high"|"max"
	TokensIn   int     `json:"tokens_in"`        // estimated input
	TokensOut  int     `json:"tokens_out"`       // estimated output
	CostMinUSD float64 `json:"cost_min_usd"`
	CostMaxUSD float64 `json:"cost_max_usd"`
}

// previewCostResponse is the JSON shape returned by POST /api/runs/preview-cost.
// Min/max bracket reflects best-case (no retries, no loops) and a 2x
// pessimistic fan-out factor. All cost numbers are best-effort hints —
// see pkg/backend/cost for the pricing table caveats.
type previewCostResponse struct {
	TokensMin  int               `json:"tokens_min"`
	TokensMax  int               `json:"tokens_max"`
	CostMinUSD float64           `json:"cost_min_usd"`
	CostMaxUSD float64           `json:"cost_max_usd"`
	Nodes      []previewCostNode `json:"nodes"`
	Notes      []string          `json:"notes,omitempty"`
}

// effortTokens maps a reasoning_effort tier to an indicative token
// envelope (input, output). Numbers are intentionally generous — the
// goal is to flag $5 workflows before launch, not to predict to the
// cent. Refresh when provider pricing or claw effort defaults shift.
var effortTokens = map[string]struct{ in, out int }{
	"low":    {3_000, 1_500},
	"medium": {12_000, 4_000},
	"high":   {32_000, 8_000},
	"xhigh":  {60_000, 12_000},
	"max":    {80_000, 16_000},
}

// defaultEffortTokens is used when a node has no declared effort and
// the IR resolver did not substitute one. Mid-tier on purpose: a fresh
// .iter with no annotations should map to a "medium" estimate.
var defaultEffortTokens = effortTokens["medium"]

func estimateNodeTokens(model, effort string, maxTokensHint int) (in, out int) {
	tier, ok := effortTokens[effort]
	if !ok {
		tier = defaultEffortTokens
	}
	in = tier.in
	// MaxTokens caps the output budget; honour it if smaller than the
	// effort-tier default. Treat 0 (backend default) as "no cap".
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
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}

	// Source > file_path, mirroring the launch endpoint. Without either
	// we can't estimate, so 400 with a clear hint.
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
		parserPath = "studio.iter"
	}

	pr := parser.Parse(parserPath, src)
	if pr.File == nil {
		// The chip should never block the form: when the workflow is
		// unparseable we return an empty estimate and let the studio
		// silently hide the chip. The Launch button will fail validation
		// later via the existing /api/validate path.
		writeJSON(w, previewCostResponse{Notes: []string{"workflow_unparseable"}})
		return
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		writeJSON(w, previewCostResponse{Notes: []string{"workflow_incomplete"}})
		return
	}

	resp := previewCostResponse{}
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
