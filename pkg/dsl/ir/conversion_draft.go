package ir

import "sort"

// LegacyConversionDraft is an inspection aid, never an executable public
// contract. In particular, a legacy control edge cannot prove a typed data
// binding or a completed product. Every draft remains incomplete until its
// mappings, effects and guarantees are verified explicitly.
type LegacyConversionDraft struct {
	Status          string                 `json:"status"`
	WorkflowName    string                 `json:"workflow_name"`
	CandidateInputs []LegacyInputCandidate `json:"candidate_inputs"`
	CandidateNodes  []LegacyNodeCandidate  `json:"candidate_nodes"`
	Unresolved      []LegacyConversionGap  `json:"unresolved"`
}

type LegacyInputCandidate struct {
	Name       string `json:"name"`
	LegacyType string `json:"legacy_type"`
	Required   bool   `json:"required"`
}

type LegacyNodeCandidate struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type LegacyConversionGap struct {
	Kind   string `json:"kind"`
	NodeID string `json:"node_id,omitempty"`
	Action string `json:"action"`
}

// ConversionDraft extracts only facts the compiled legacy workflow already
// establishes. It deliberately leaves public output types, file guarantees,
// effect policies and control-to-data bindings unresolved. The caller must not
// treat the draft as activation or migration evidence.
func (w *Workflow) ConversionDraft() *LegacyConversionDraft {
	if w == nil || w.RuntimeSemantics != "" || w.Ports != nil {
		return nil
	}
	draft := &LegacyConversionDraft{Status: "incomplete", WorkflowName: w.Name}
	for name, variable := range w.Vars {
		if variable == nil {
			continue
		}
		draft.CandidateInputs = append(draft.CandidateInputs, LegacyInputCandidate{
			Name: name, LegacyType: variable.Type.String(), Required: !variable.HasDefault,
		})
		if variable.Type == VarJSON {
			draft.Unresolved = append(draft.Unresolved, LegacyConversionGap{
				Kind: "input_type", NodeID: name,
				Action: "replace the legacy json variable with an explicit public type and schema",
			})
		}
	}
	sort.Slice(draft.CandidateInputs, func(i, j int) bool { return draft.CandidateInputs[i].Name < draft.CandidateInputs[j].Name })
	for id, node := range w.Nodes {
		if node == nil {
			continue
		}
		draft.CandidateNodes = append(draft.CandidateNodes, LegacyNodeCandidate{ID: id, Kind: node.NodeKind().String()})
		switch node.NodeKind() {
		case NodeAgent, NodeJudge, NodeTool, NodeSubbot, NodeHuman, NodeEmit, NodeWait, NodeAwaitAnswers:
			draft.Unresolved = append(draft.Unresolved, LegacyConversionGap{
				Kind: "effect_review", NodeID: id,
				Action: "declare paid and external effects, recovery policy, resource use and a verifier where needed",
			})
		}
		if node.NodeKind() == NodeSubbot {
			draft.Unresolved = append(draft.Unresolved, LegacyConversionGap{
				Kind: "child_contract", NodeID: id,
				Action: "capture and verify the child source, input/output mapping and inherited root policies",
			})
		}
	}
	sort.Slice(draft.CandidateNodes, func(i, j int) bool { return draft.CandidateNodes[i].ID < draft.CandidateNodes[j].ID })
	draft.Unresolved = append(draft.Unresolved,
		LegacyConversionGap{Kind: "node_bindings", Action: "assign named typed input/output ports and verify every supplier; control edges do not imply data flow"},
		LegacyConversionGap{Kind: "workflow_outputs", Action: "declare typed workflow outputs and required product exports explicitly"},
		LegacyConversionGap{Kind: "file_guarantees", Action: "declare produced files, freshness, media type and validation criteria where applicable"},
		LegacyConversionGap{Kind: "control_semantics", Action: "keep loops, gates and events in a verified legacy adapter or redesign them explicitly"},
	)
	sort.Slice(draft.Unresolved, func(i, j int) bool {
		if draft.Unresolved[i].Kind != draft.Unresolved[j].Kind {
			return draft.Unresolved[i].Kind < draft.Unresolved[j].Kind
		}
		return draft.Unresolved[i].NodeID < draft.Unresolved[j].NodeID
	})
	return draft
}
