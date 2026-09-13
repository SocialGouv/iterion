package ir

import (
	"fmt"
	"mime"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

func (c *compiler) compilePortWorkflow(w *Workflow, decl *ast.WorkflowDecl, contracts map[string]*PublicContract, policies map[string]*PortPolicy) {
	w.RuntimeSemantics = decl.RuntimeSemantics
	switch decl.RuntimeSemantics {
	case "":
		if decl.Graph != nil || decl.Contract != "" || decl.PortPolicy != "" {
			c.contractError(DiagRuntimeSemantics, decl.Span, "graph, contract and port_policy require runtime_semantics: %q", RuntimeSemanticsPortsV1)
		}
		return
	case RuntimeSemanticsPortsV1:
	default:
		c.contractError(DiagRuntimeSemantics, decl.Span, "unsupported runtime_semantics %q", decl.RuntimeSemantics)
		return
	}
	w.PublicContract = contracts[decl.Contract]
	if w.PublicContract == nil {
		c.contractError(DiagPublicContract, decl.Span, "workflow %q needs a declared public contract; %q is unresolved", decl.Name, decl.Contract)
	}
	if decl.Entry != "" || len(decl.Edges) > 0 {
		c.contractError(DiagPortControl, decl.Span, "native port graphs cannot declare entry or control edges; encapsulate control flow in a verified legacy adapter")
	}
	if decl.Graph == nil {
		c.contractError(DiagPortReference, decl.Span, "workflow %q requires a graph block", decl.Name)
		return
	}
	rootPolicy := &PortPolicy{}
	if decl.PortPolicy != "" {
		if p := policies[decl.PortPolicy]; p != nil {
			rootPolicy = p
		} else {
			c.contractError(DiagPortPolicy, decl.Span, "unknown workflow port_policy %q", decl.PortPolicy)
		}
	}
	w.PortPolicy = rootPolicy
	g := &PortGraph{
		Nodes: map[string]*PortInstance{}, Exports: map[string]PortEndpoint{},
		Products: append([]string(nil), decl.Graph.Products...),
	}
	w.Ports = g
	executable := map[string]Node{}
	for _, nodeDecl := range decl.Graph.Nodes {
		if nodeDecl == nil {
			c.contractError(DiagPortReference, decl.Graph.Span, "missing graph node declaration")
			continue
		}
		id := nodeDecl.Name
		if !validPublicName(id) || id == "input" || id == "output" || ast.ReservedTargets[id] || g.Nodes[id] != nil {
			c.contractError(DiagPortReference, nodeDecl.Span, "invalid, reserved or duplicate graph instance %q", id)
			continue
		}
		contract := contracts[nodeDecl.Contract]
		if contract == nil {
			c.contractError(DiagPublicContract, nodeDecl.Span, "instance %q references unknown contract %q", id, nodeDecl.Contract)
			continue
		}
		implementation := w.Nodes[nodeDecl.Implementation]
		if implementation == nil {
			c.contractError(DiagPortImplementation, nodeDecl.Span, "instance %q references unknown implementation %q", id, nodeDecl.Implementation)
			continue
		}
		node, err := clonePortNode(implementation, id)
		if err != nil {
			c.contractError(DiagPortControl, nodeDecl.Span, "%v; use a verified legacy adapter", err)
			continue
		}
		policy := c.resolveInstancePortPolicy(rootPolicy, nodeDecl, contract, policies, w.Resources)
		instance := &PortInstance{
			ID: id, Implementation: nodeDecl.Implementation, Contract: contract, Policy: policy,
			Inputs: map[string]PortEndpoint{}, OutputTypes: map[string]PortType{}, Source: portSource(nodeDecl.Span),
		}
		for _, output := range contract.Outputs {
			instance.OutputTypes[output.Name] = output.Type
		}
		g.Nodes[id], executable[id] = instance, node
		c.attachPublicNodeSchemas(w, node, contract, nodeDecl.Span)
	}
	// Public instances are the executable nodes. Technical declarations may be
	// reused under different instance IDs; unused implementations are not jobs.
	w.Nodes, c.nodes = executable, executable
	for _, binding := range decl.Graph.Bindings {
		if binding == nil {
			c.contractError(DiagPortReference, decl.Graph.Span, "missing port binding")
			continue
		}
		from, errFrom := ParsePortEndpoint(binding.From)
		to, errTo := ParsePortEndpoint(binding.To)
		if errFrom != nil || errTo != nil {
			c.contractError(DiagPortReference, binding.Span, "invalid binding %q -> %q; endpoints must be node.port", binding.From, binding.To)
			continue
		}
		target := g.Nodes[to.Node]
		if target == nil || FindPublicPort(target.Contract.Inputs, to.Port) == nil {
			c.contractError(DiagPortReference, binding.Span, "unknown consumer input %q", to)
			continue
		}
		if _, _, ok := publicSupplier(w, from); !ok {
			c.contractError(DiagPortReference, binding.Span, "unknown supplier output %q", from)
			continue
		}
		if previous, exists := target.Inputs[to.Port]; exists {
			c.contractError(DiagPortSupplier, binding.Span, "input %q has multiple suppliers: %s and %s", to, previous, from)
			continue
		}
		target.Inputs[to.Port] = from
		g.Bindings = append(g.Bindings, PortBinding{From: from, To: to, Source: portSource(binding.Span)})
	}
	for _, id := range sortedPortInstances(g) {
		instance := g.Nodes[id]
		dependencies := map[string]bool{}
		for _, input := range instance.Contract.Inputs {
			from, connected := instance.Inputs[input.Name]
			if !connected && input.Required {
				c.portInstanceError(DiagPortSupplier, instance, "required input %s.%s has no supplier", id, input.Name)
			}
			if connected && from.Node != WorkflowInputPortNode {
				dependencies[from.Node] = true
			}
		}
		for dependency := range dependencies {
			instance.Dependencies = append(instance.Dependencies, dependency)
		}
		sort.Strings(instance.Dependencies)
	}
	order, cycle := portTopologicalOrder(g)
	g.Order = order
	if len(cycle) != 0 {
		c.contractError(DiagPortCycle, decl.Graph.Span, "native data graph contains a cycle involving %s", strings.Join(cycle, ", "))
	}
	// Mapping changes effective OUTPUT cardinality, so infer it in dependency
	// order. A later node sees U[] from a mapped U producer, not the template U.
	for _, id := range order {
		instance := g.Nodes[id]
		for i := range g.Bindings {
			binding := &g.Bindings[i]
			if binding.To.Node != id {
				continue
			}
			supplier, actual, ok := publicSupplier(w, binding.From)
			if !ok {
				continue
			}
			input := FindPublicPort(instance.Contract.Inputs, binding.To.Port)
			element, array := actual.Element()
			mapped := !actual.Equivalent(input.Type) && array && element.Equivalent(input.Type)
			if !actual.Equivalent(input.Type) && !mapped {
				c.portBindingError(DiagPortCompatibility, binding, "cannot connect %s (%s) to %s (%s); no implicit conversion", binding.From, actual, binding.To, input.Type)
				continue
			}
			if supplier.Nullable && (!input.Nullable || mapped) {
				c.portBindingError(DiagPortCompatibility, binding, "nullable supplier %s cannot guarantee the value required by %s", binding.From, binding.To)
			}
			if !supplier.Required && supplier.Default == nil && (input.Required || mapped) {
				c.portBindingError(DiagPortCompatibility, binding, "optional supplier %s cannot guarantee required input or map axis %s", binding.From, binding.To)
			}
			if !mapped {
				if err := checkPublicSupplierConstraints(w, binding.From, input); err != nil {
					c.portBindingError(DiagPortCompatibility, binding, "%s -> %s: %v", binding.From, binding.To, err)
				}
			}
			if mapped {
				if instance.MapInput != "" && instance.Inputs[instance.MapInput] != binding.From {
					c.portBindingError(DiagPortMapAxes, binding, "instance %q has multiple implicit map axes (%s, %s); declare composition explicitly instead of guessing zip or Cartesian behavior", id, instance.MapInput, input.Name)
				} else if instance.MapInput == "" {
					instance.MapInput = input.Name
				}
				instance.MapInputs = append(instance.MapInputs, input.Name)
				binding.Map = true
			}
		}
		if instance.MapInput != "" {
			for name, t := range instance.OutputTypes {
				instance.OutputTypes[name] = t.Array()
			}
		}
	}
	c.compilePortExports(w, decl)
	c.validatePortWorkflowEffects(w, decl)
	c.validateNativePublicRefs(w)
	cp := *g
	cp.Nodes = make(map[string]*PortInstance, len(g.Nodes))
	for id, instance := range g.Nodes {
		copy := *instance
		copy.Source = PortSource{}
		// The public identity already excludes diagnostic source positions.
		copy.Contract = &PublicContract{Identity: instance.Contract.Identity}
		cp.Nodes[id] = &copy
	}
	cp.Bindings = append([]PortBinding(nil), cp.Bindings...)
	for i := range cp.Bindings {
		cp.Bindings[i].Source = PortSource{}
	}
	g.Identity = publicDigest(cp)
}

func (c *compiler) validatePortWorkflowEffects(w *Workflow, decl *ast.WorkflowDecl) {
	if w.PublicContract == nil {
		return
	}
	exposed := map[string]PublicEffect{}
	for _, effect := range w.PublicContract.Effects {
		exposed[effect.Name] = effect
	}
	for _, id := range sortedPortInstances(w.Ports) {
		instance := w.Ports.Nodes[id]
		for _, effect := range instance.Contract.Effects {
			public, exists := exposed[effect.Name]
			if !exists || (effect.Paid && !public.Paid) {
				c.contractError(DiagPublicContract, decl.Span,
					"workflow contract must expose effect %q of instance %q and retain its paid declaration", effect.Name, id)
			}
		}
	}
}

func (c *compiler) portInstanceError(code DiagCode, n *PortInstance, format string, args ...any) {
	sp := ast.Span{Start: ast.Pos{File: n.Source.File, Line: n.Source.Line, Column: n.Source.Column}}
	c.emit(SeverityError, code, n.ID, "", sp, "", format, args...)
}

func (c *compiler) portBindingError(code DiagCode, b *PortBinding, format string, args ...any) {
	sp := ast.Span{Start: ast.Pos{File: b.Source.File, Line: b.Source.Line, Column: b.Source.Column}}
	c.emit(SeverityError, code, b.To.Node, b.From.String()+"->"+b.To.String(), sp, "", format, args...)
}

func publicSupplier(w *Workflow, endpoint PortEndpoint) (*PublicPort, PortType, bool) {
	if endpoint.Node == WorkflowInputPortNode {
		if w.PublicContract != nil {
			if port := FindPublicPort(w.PublicContract.Inputs, endpoint.Port); port != nil {
				return port, port.Type, true
			}
		}
		return nil, PortType{}, false
	}
	if w.Ports != nil {
		if node := w.Ports.Nodes[endpoint.Node]; node != nil {
			if port := FindPublicPort(node.Contract.Outputs, endpoint.Port); port != nil {
				return port, node.OutputTypes[endpoint.Port], true
			}
		}
	}
	return nil, PortType{}, false
}

func sortedPortInstances(g *PortGraph) []string {
	ids := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func portTopologicalOrder(g *PortGraph) (order, cycle []string) {
	indegree := map[string]int{}
	next := map[string][]string{}
	var ready []string
	for _, id := range sortedPortInstances(g) {
		indegree[id] = len(g.Nodes[id].Dependencies)
		if indegree[id] == 0 {
			ready = append(ready, id)
		}
		for _, dependency := range g.Nodes[id].Dependencies {
			next[dependency] = append(next[dependency], id)
		}
	}
	for len(ready) != 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, target := range next[id] {
			indegree[target]--
			if indegree[target] == 0 {
				ready = append(ready, target)
			}
		}
		sort.Strings(ready)
	}
	for _, id := range sortedPortInstances(g) {
		if indegree[id] != 0 {
			cycle = append(cycle, id)
		}
	}
	return order, cycle
}

func (c *compiler) compilePortExports(w *Workflow, decl *ast.WorkflowDecl) {
	g := w.Ports
	for _, export := range decl.Graph.Exports {
		if export == nil {
			c.contractError(DiagPortExport, decl.Graph.Span, "missing public export")
			continue
		}
		var target *PublicPort
		if w.PublicContract != nil {
			target = FindPublicPort(w.PublicContract.Outputs, export.Name)
		}
		if target == nil {
			c.contractError(DiagPortExport, export.Span, "export %q is not a declared public workflow output", export.Name)
			continue
		}
		if _, duplicate := g.Exports[export.Name]; duplicate {
			c.contractError(DiagPortExport, export.Span, "public output %q has multiple exports", export.Name)
			continue
		}
		endpoint, err := ParsePortEndpoint(export.From)
		supplier, actual, ok := publicSupplier(w, endpoint)
		if err != nil || !ok {
			c.contractError(DiagPortExport, export.Span, "export %q references unknown supplier %q", export.Name, export.From)
			continue
		}
		if !actual.Equivalent(target.Type) || (supplier.Nullable && !target.Nullable) ||
			(!supplier.Required && supplier.Default == nil && target.Required) {
			c.contractError(DiagPortCompatibility, export.Span, "export %q cannot satisfy public output %s (%s) from %s (%s)", export.Name, export.Name, target.Type, endpoint, actual)
		}
		if err := checkPublicSupplierConstraints(w, endpoint, target); err != nil {
			c.contractError(DiagPortCompatibility, export.Span, "export %q: %v", export.Name, err)
		}
		g.Exports[export.Name] = endpoint
	}
	if w.PublicContract != nil {
		for _, port := range w.PublicContract.Outputs {
			if _, exported := g.Exports[port.Name]; port.Required && !exported {
				c.contractError(DiagPortExport, decl.Graph.Span, "required public output %q has no export", port.Name)
			}
		}
	}
	products := map[string]bool{}
	for _, name := range g.Products {
		_, exported := g.Exports[name]
		if !exported || products[name] {
			c.contractError(DiagPortExport, decl.Graph.Span, "product %q needs exactly one declared public export", name)
		}
		products[name] = true
	}
}

func publicSupplierBounds(w *Workflow, endpoint PortEndpoint) (min, max *int) {
	if endpoint.Node != WorkflowInputPortNode {
		if node := w.Ports.Nodes[endpoint.Node]; node != nil && node.MapInput != "" {
			return publicSupplierBounds(w, node.Inputs[node.MapInput])
		}
	}
	port, _, ok := publicSupplier(w, endpoint)
	if ok {
		return port.MinItems, port.MaxItems
	}
	return nil, nil
}

// Reject constraints with no possible common value. Overlapping ranges still
// require validation of the actual value before admission; a producer allowed
// to emit [] does not guarantee that a consumer requiring one item can run.
func checkPublicSupplierConstraints(w *Workflow, endpoint PortEndpoint, target *PublicPort) error {
	source, actual, ok := publicSupplier(w, endpoint)
	if !ok {
		return nil // the reference diagnostic names the missing endpoint
	}
	if actual.ArrayDepth > 0 && target.Type.ArrayDepth > 0 {
		min, max := publicSupplierBounds(w, endpoint)
		if (min != nil && target.MaxItems != nil && *min > *target.MaxItems) ||
			(max != nil && target.MinItems != nil && *max < *target.MinItems) {
			return fmt.Errorf("array cardinality constraints have no common value")
		}
	}
	if source.File != nil && target.File != nil {
		a, b := source.File, target.File
		if a.MediaType != "" && b.MediaType != "" {
			sourceMIME, _, _ := mime.ParseMediaType(a.MediaType)
			targetMIME, _, _ := mime.ParseMediaType(b.MediaType)
			if sourceMIME != targetMIME {
				return fmt.Errorf("file media types %q and %q are incompatible", a.MediaType, b.MediaType)
			}
		}
		if a.ShapeHash != "" && b.ShapeHash != "" && a.ShapeHash != b.ShapeHash {
			return fmt.Errorf("file schemas have different resolved shapes")
		}
	}
	return nil
}

func (c *compiler) resolveInstancePortPolicy(root *PortPolicy, decl *ast.PortNodeDecl, contract *PublicContract, policies map[string]*PortPolicy, resources map[string]int) *PortPolicy {
	effective := &PortPolicy{MaxMapItems: root.MaxMapItems}
	byName := map[string]PortEffectPolicy{}
	for _, effect := range root.Effects {
		byName[effect.Name] = effect
	}
	if decl.Policy != "" {
		local := policies[decl.Policy]
		if local == nil {
			c.contractError(DiagPortPolicy, decl.Span, "unknown instance policy %q", decl.Policy)
		} else {
			effective.Name = local.Name
			if local.MaxMapItems != 0 {
				effective.MaxMapItems = local.MaxMapItems
			}
			for _, effect := range local.Effects {
				byName[effect.Name] = effect
			}
		}
	}
	for _, public := range contract.Effects {
		policy, found := byName[public.Name]
		if !found {
			c.contractError(DiagPortPolicy, decl.Span, "public effect %q of instance %q has no technical recovery policy", public.Name, decl.Name)
			continue
		}
		if policy.Resource != "" && resources[policy.Resource] <= 0 {
			c.contractError(DiagPortPolicy, decl.Span, "effect %q references undeclared resource %q", public.Name, policy.Resource)
		}
		effective.Effects = append(effective.Effects, policy)
	}
	effective.Identity = publicDigest(effective)
	return effective
}

func clonePortNode(node Node, id string) (Node, error) {
	switch n := node.(type) {
	case *AgentNode:
		copy := *n
		copy.ID = id
		return &copy, nil
	case *JudgeNode:
		copy := *n
		copy.ID = id
		return &copy, nil
	case *ToolNode:
		copy := *n
		copy.ID = id
		return &copy, nil
	case *ComputeNode:
		copy := *n
		copy.ID = id
		return &copy, nil
	case *SubbotNode:
		copy := *n
		copy.ID = id
		return &copy, nil
	case *EmitNode:
		copy := *n
		copy.ID = id
		return &copy, nil
	case *WaitNode:
		copy := *n
		copy.ID = id
		return &copy, nil
	case *AwaitAnswersNode:
		copy := *n
		copy.ID = id
		return &copy, nil
	default:
		return nil, fmt.Errorf("node kind %s is a control construct, not a native data operation", node.NodeKind())
	}
}
