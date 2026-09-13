package ir

// PublicWorkflowView is the contract and data-flow projection of a compiled
// native workflow. It deliberately carries no implementation names, prompts,
// providers, tool configuration or technical policies. Authoring clients can
// inspect this view before requesting the full document.
type PublicWorkflowView struct {
	RuntimeSemantics string                  `json:"runtime_semantics"`
	Contract         *PublicContract         `json:"contract"`
	Nodes            []PublicNodeView        `json:"nodes"`
	Bindings         []PortBinding           `json:"bindings"`
	Exports          map[string]PortEndpoint `json:"exports"`
	Products         []string                `json:"products"`
	GraphIdentity    string                  `json:"graph_identity"`
}

type PublicNodeView struct {
	ID           string                  `json:"id"`
	Contract     *PublicContract         `json:"contract"`
	Inputs       map[string]PortEndpoint `json:"inputs"`
	OutputTypes  map[string]PortType     `json:"output_types"`
	Dependencies []string                `json:"dependencies"`
	MapInput     string                  `json:"map_input,omitempty"`
}

// PublicView returns nil for a legacy workflow. Order follows the compiled
// topological order, which is stable for inspection but does not serialize
// independent jobs at runtime.
func (w *Workflow) PublicView() *PublicWorkflowView {
	if w == nil || w.Ports == nil {
		return nil
	}
	view := &PublicWorkflowView{
		RuntimeSemantics: w.RuntimeSemantics,
		Contract:         w.PublicContract,
		Bindings:         append([]PortBinding(nil), w.Ports.Bindings...),
		Exports:          make(map[string]PortEndpoint, len(w.Ports.Exports)),
		Products:         append([]string(nil), w.Ports.Products...),
		GraphIdentity:    w.Ports.Identity,
	}
	for i := range view.Bindings {
		view.Bindings[i].Source = PortSource{}
	}
	for name, endpoint := range w.Ports.Exports {
		view.Exports[name] = endpoint
	}
	for _, id := range w.Ports.Order {
		instance := w.Ports.Nodes[id]
		if instance == nil {
			continue
		}
		node := PublicNodeView{
			ID: id, Contract: instance.Contract,
			Inputs:       make(map[string]PortEndpoint, len(instance.Inputs)),
			OutputTypes:  make(map[string]PortType, len(instance.OutputTypes)),
			Dependencies: append([]string(nil), instance.Dependencies...),
			MapInput:     instance.MapInput,
		}
		for name, endpoint := range instance.Inputs {
			node.Inputs[name] = endpoint
		}
		for name, typ := range instance.OutputTypes {
			node.OutputTypes[name] = typ
		}
		view.Nodes = append(view.Nodes, node)
	}
	return view
}
