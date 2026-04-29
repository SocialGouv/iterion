package ir

import (
	"fmt"
	"sort"
	"strings"
)

// MermaidView controls the level of detail in the generated diagram.
type MermaidView int

const (
	// MermaidCompact shows nodes with kind icons and simple edge labels.
	MermaidCompact MermaidView = iota
	// MermaidDetailed shows nodes with full metadata and annotated edges.
	MermaidDetailed
	// MermaidFull shows all available metadata including schemas fields,
	// prompts, tools, budget, variables, and loops.
	MermaidFull
)

// ToMermaid renders the workflow IR as a Mermaid flowchart string.
func (w *Workflow) ToMermaid(view MermaidView) string {
	var b strings.Builder
	b.WriteString("flowchart TD\n")

	// Emit workflow metadata subgraph for the full view.
	if view == MermaidFull {
		b.WriteString(workflowMetadata(w))
	}

	// Collect and sort node IDs for deterministic output.
	ids := make([]string, 0, len(w.Nodes))
	for id := range w.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// Emit nodes.
	for _, id := range ids {
		node := w.Nodes[id]
		b.WriteString("    ")
		b.WriteString(nodeDecl(node, w, view))
		b.WriteString("\n")
	}

	b.WriteString("\n")

	// Emit edges.
	for _, edge := range w.Edges {
		b.WriteString("    ")
		b.WriteString(edgeDecl(edge, w, view))
		b.WriteString("\n")
	}

	// Emit style classes for node kinds.
	b.WriteString("\n")
	b.WriteString(styleClasses(w))

	return b.String()
}

// nodeDecl returns the Mermaid node declaration for a given node.
func nodeDecl(n Node, w *Workflow, view MermaidView) string {
	id := sanitizeID(n.NodeID())

	switch view {
	case MermaidFull:
		return id + fullShape(n, w)
	case MermaidDetailed:
		return id + detailedShape(n)
	default:
		return id + compactShape(n)
	}
}

// compactShape returns a compact Mermaid shape with a kind icon prefix.
func compactShape(n Node) string {
	kind := n.NodeKind()
	icon := kindIcon(kind)
	label := icon + " " + n.NodeID()

	switch kind {
	case NodeDone:
		return fmt.Sprintf(`(["%s"])`, label)
	case NodeFail:
		return fmt.Sprintf(`(["%s"])`, label)
	case NodeRouter:
		return fmt.Sprintf(`{"%s"}`, label)
	case NodeHuman:
		return fmt.Sprintf(`>"%s"]`, label)
	default:
		if NodeAwaitMode(n) != AwaitNone {
			return fmt.Sprintf(`[["%s"]]`, label)
		}
		return fmt.Sprintf(`["%s"]`, label)
	}
}

// detailedShape returns a detailed Mermaid shape with metadata.
func detailedShape(node Node) string {
	kind := node.NodeKind()
	icon := kindIcon(kind)

	var lines []string
	lines = append(lines, icon+" "+node.NodeID())

	switch n := node.(type) {
	case *AgentNode:
		lines = appendLLMDetailedLines(lines, n.Model, n.InputSchema, n.OutputSchema, n.Publish, n.Session, n.Interaction)
	case *JudgeNode:
		lines = appendLLMDetailedLines(lines, n.Model, n.InputSchema, n.OutputSchema, n.Publish, n.Session, n.Interaction)
	case *RouterNode:
		lines = append(lines, "mode: "+n.RouterMode.String())
	case *HumanNode:
		lines = append(lines, "interaction: "+n.Interaction.String())
		if n.MinAnswers > 0 {
			lines = append(lines, fmt.Sprintf("min_answers: %d", n.MinAnswers))
		}
	case *ToolNode:
		if n.Command != "" {
			lines = append(lines, "cmd: "+n.Command)
		}
	case *ComputeNode:
		if n.OutputSchema != "" {
			lines = append(lines, "out: "+n.OutputSchema)
		}
		for _, e := range n.Exprs {
			lines = append(lines, e.Key+" = "+e.Raw)
		}
	}

	await := NodeAwaitMode(node)
	if await != AwaitNone {
		lines = append(lines, "await: "+await.String())
	}

	label := strings.Join(lines, "<br/>")

	switch kind {
	case NodeDone, NodeFail:
		return fmt.Sprintf(`(["%s"])`, label)
	case NodeRouter:
		return fmt.Sprintf(`{"%s"}`, label)
	case NodeHuman:
		return fmt.Sprintf(`>"%s"]`, label)
	default:
		if await != AwaitNone {
			return fmt.Sprintf(`[["%s"]]`, label)
		}
		return fmt.Sprintf(`["%s"]`, label)
	}
}

// edgeDecl returns the Mermaid edge declaration.
func edgeDecl(e *Edge, w *Workflow, view MermaidView) string {
	from := sanitizeID(e.From)
	to := sanitizeID(e.To)

	label := edgeLabel(e, w, view)
	if label == "" {
		return fmt.Sprintf("%s --> %s", from, to)
	}
	return fmt.Sprintf(`%s -->|"%s"| %s`, from, label, to)
}

// edgeLabel builds the label for an edge.
func edgeLabel(e *Edge, w *Workflow, view MermaidView) string {
	var parts []string

	if e.Condition != "" {
		cond := e.Condition
		if e.Negated {
			cond = "NOT " + cond
		}
		parts = append(parts, cond)
	} else if e.ExpressionSrc != "" {
		parts = append(parts, "expr: "+e.ExpressionSrc)
	}

	if e.LoopName != "" {
		loop, ok := w.Loops[e.LoopName]
		if ok {
			parts = append(parts, fmt.Sprintf("loop:%s(%d)", loop.Name, loop.MaxIterations))
		} else {
			parts = append(parts, "loop:"+e.LoopName)
		}
	}

	if (view == MermaidDetailed || view == MermaidFull) && len(e.With) > 0 {
		var mappings []string
		for _, dm := range e.With {
			mappings = append(mappings, dm.Key+"="+dm.Raw)
		}
		parts = append(parts, "with: "+strings.Join(mappings, ", "))
	}

	return strings.Join(parts, " / ")
}

// styleClasses emits Mermaid classDef and class assignments for node kinds.
func styleClasses(w *Workflow) string {
	var b strings.Builder

	b.WriteString("    classDef agent fill:#4A90D9,stroke:#2C5F8A,color:#fff\n")
	b.WriteString("    classDef judge fill:#7B68EE,stroke:#5A4CB5,color:#fff\n")
	b.WriteString("    classDef router fill:#F5A623,stroke:#C47D0E,color:#fff\n")
	b.WriteString("    classDef human fill:#FF6B6B,stroke:#CC4444,color:#fff\n")
	b.WriteString("    classDef tool fill:#A0522D,stroke:#6E3720,color:#fff\n")
	b.WriteString("    classDef compute fill:#6BB7B7,stroke:#3D7A7A,color:#fff\n")
	b.WriteString("    classDef done fill:#2ECC71,stroke:#1A8B4C,color:#fff\n")
	b.WriteString("    classDef fail fill:#E74C3C,stroke:#A93226,color:#fff\n")

	// Group nodes by kind.
	groups := map[NodeKind][]string{}
	for id, node := range w.Nodes {
		groups[node.NodeKind()] = append(groups[node.NodeKind()], sanitizeID(id))
	}

	for kind, nodeIDs := range groups {
		sort.Strings(nodeIDs)
		b.WriteString(fmt.Sprintf("    class %s %s\n", strings.Join(nodeIDs, ","), kind.String()))
	}

	return b.String()
}

// kindIcon returns a text icon prefix for each node kind.
func kindIcon(k NodeKind) string {
	switch k {
	case NodeAgent:
		return "🤖"
	case NodeJudge:
		return "⚖️"
	case NodeRouter:
		return "🔀"
	case NodeHuman:
		return "👤"
	case NodeTool:
		return "🔧"
	case NodeCompute:
		return "🧮"
	case NodeDone:
		return "✅"
	case NodeFail:
		return "❌"
	default:
		return "?"
	}
}

// fullShape returns a Mermaid shape with all available metadata.
func fullShape(node Node, w *Workflow) string {
	kind := node.NodeKind()
	icon := kindIcon(kind)

	var lines []string
	lines = append(lines, icon+" "+node.NodeID())

	switch n := node.(type) {
	case *AgentNode:
		lines = appendLLMFullLines(lines, w, n.LLMFields, n.SchemaFields, n.Publish, n.Session, n.Tools, n.ToolMaxSteps)
	case *JudgeNode:
		lines = appendLLMFullLines(lines, w, n.LLMFields, n.SchemaFields, n.Publish, n.Session, n.Tools, n.ToolMaxSteps)
	case *RouterNode:
		lines = append(lines, "mode: "+n.RouterMode.String())
	case *HumanNode:
		lines = append(lines, "interaction: "+n.Interaction.String())
		if n.Model != "" {
			lines = append(lines, "model: "+n.Model)
		}
		if n.InputSchema != "" {
			lines = append(lines, "in: "+expandSchema(n.InputSchema, w))
		}
		if n.OutputSchema != "" {
			lines = append(lines, "out: "+expandSchema(n.OutputSchema, w))
		}
		if n.Instructions != "" {
			lines = append(lines, "instructions: "+n.Instructions)
		}
		if n.MinAnswers > 0 {
			lines = append(lines, fmt.Sprintf("min_answers: %d", n.MinAnswers))
		}
	case *ToolNode:
		if n.Command != "" {
			lines = append(lines, "cmd: "+n.Command)
		}
		if n.OutputSchema != "" {
			lines = append(lines, "out: "+expandSchema(n.OutputSchema, w))
		}
	}

	await := NodeAwaitMode(node)
	if await != AwaitNone {
		lines = append(lines, "await: "+await.String())
	}

	label := strings.Join(lines, "<br/>")

	switch kind {
	case NodeDone, NodeFail:
		return fmt.Sprintf(`(["%s"])`, label)
	case NodeRouter:
		return fmt.Sprintf(`{"%s"}`, label)
	case NodeHuman:
		return fmt.Sprintf(`>"%s"]`, label)
	default:
		if await != AwaitNone {
			return fmt.Sprintf(`[["%s"]]`, label)
		}
		return fmt.Sprintf(`["%s"]`, label)
	}
}

// appendLLMDetailedLines appends the shared metadata lines for Agent/Judge in detailed view.
func appendLLMDetailedLines(lines []string, model, inSchema, outSchema, publish string, session SessionMode, interaction InteractionMode) []string {
	if model != "" {
		lines = append(lines, "model: "+model)
	}
	if inSchema != "" {
		lines = append(lines, "in: "+inSchema)
	}
	if outSchema != "" {
		lines = append(lines, "out: "+outSchema)
	}
	if publish != "" {
		lines = append(lines, "publish: "+publish)
	}
	if session != SessionFresh {
		lines = append(lines, "session: "+session.String())
	}
	if interaction != InteractionNone {
		lines = append(lines, "interaction: "+interaction.String())
	}
	return lines
}

// appendLLMFullLines appends the shared metadata lines for Agent/Judge in full view.
func appendLLMFullLines(lines []string, w *Workflow, llm LLMFields, schema SchemaFields, publish string, session SessionMode, tools []string, toolMaxSteps int) []string {
	if llm.Model != "" {
		lines = append(lines, "model: "+llm.Model)
	}
	if llm.Backend != "" {
		lines = append(lines, "backend: "+llm.Backend)
	}
	if schema.InputSchema != "" {
		lines = append(lines, "in: "+expandSchema(schema.InputSchema, w))
	}
	if schema.OutputSchema != "" {
		lines = append(lines, "out: "+expandSchema(schema.OutputSchema, w))
	}
	if publish != "" {
		lines = append(lines, "publish: "+publish)
	}
	lines = append(lines, "session: "+session.String())
	if llm.SystemPrompt != "" {
		lines = append(lines, "system: "+llm.SystemPrompt)
	}
	if llm.UserPrompt != "" {
		lines = append(lines, "user: "+llm.UserPrompt)
	}
	if len(tools) > 0 {
		lines = append(lines, "tools: "+strings.Join(tools, ", "))
	}
	if toolMaxSteps > 0 {
		lines = append(lines, fmt.Sprintf("tool_max_steps: %d", toolMaxSteps))
	}
	if llm.MaxTokens > 0 {
		lines = append(lines, fmt.Sprintf("max_tokens: %d", llm.MaxTokens))
	}
	if llm.ReasoningEffort != "" {
		lines = append(lines, "reasoning_effort: "+llm.ReasoningEffort)
	}
	return lines
}

// expandSchema returns the schema name with inline field definitions.
// At most maxInlineFields fields are shown; remaining are summarized.
func expandSchema(name string, w *Workflow) string {
	s, ok := w.Schemas[name]
	if !ok || len(s.Fields) == 0 {
		return name
	}
	const maxInlineFields = 4
	var fields []string
	for i, f := range s.Fields {
		if i >= maxInlineFields {
			fields = append(fields, fmt.Sprintf("+%d more", len(s.Fields)-maxInlineFields))
			break
		}
		entry := f.Name + ": " + f.Type.String()
		if len(f.EnumValues) > 0 {
			entry += " [" + strings.Join(f.EnumValues, "|") + "]"
		}
		fields = append(fields, entry)
	}
	return name + " (" + strings.Join(fields, ", ") + ")"
}

// workflowMetadata emits a Mermaid subgraph with workflow-level metadata
// (name, entry, variables, budget, loops).
func workflowMetadata(w *Workflow) string {
	var b strings.Builder

	// Collect metadata nodes.
	var metaNodes []string

	// Variables.
	if len(w.Vars) > 0 {
		varNames := make([]string, 0, len(w.Vars))
		for name := range w.Vars {
			varNames = append(varNames, name)
		}
		sort.Strings(varNames)
		var varLines []string
		varLines = append(varLines, "<b>Variables</b>")
		for _, name := range varNames {
			v := w.Vars[name]
			line := name + ": " + v.Type.String()
			if v.HasDefault {
				line += fmt.Sprintf(" = %v", v.Default)
			}
			varLines = append(varLines, line)
		}
		metaNodes = append(metaNodes, fmt.Sprintf(`        meta_vars["%s"]`, strings.Join(varLines, "<br/>")))
	}

	// Budget.
	if w.Budget != nil {
		var budgetLines []string
		budgetLines = append(budgetLines, "<b>Budget</b>")
		if w.Budget.MaxParallelBranches > 0 {
			budgetLines = append(budgetLines, fmt.Sprintf("max_parallel: %d", w.Budget.MaxParallelBranches))
		}
		if w.Budget.MaxDuration != "" {
			budgetLines = append(budgetLines, "max_duration: "+w.Budget.MaxDuration)
		}
		if w.Budget.MaxCostUSD > 0 {
			budgetLines = append(budgetLines, fmt.Sprintf("max_cost: $%.2f", w.Budget.MaxCostUSD))
		}
		if w.Budget.MaxTokens > 0 {
			budgetLines = append(budgetLines, fmt.Sprintf("max_tokens: %d", w.Budget.MaxTokens))
		}
		if w.Budget.MaxIterations > 0 {
			budgetLines = append(budgetLines, fmt.Sprintf("max_iterations: %d", w.Budget.MaxIterations))
		}
		if len(budgetLines) > 1 {
			metaNodes = append(metaNodes, fmt.Sprintf(`        meta_budget["%s"]`, strings.Join(budgetLines, "<br/>")))
		}
	}

	// Loops.
	if len(w.Loops) > 0 {
		loopNames := make([]string, 0, len(w.Loops))
		for name := range w.Loops {
			loopNames = append(loopNames, name)
		}
		sort.Strings(loopNames)
		var loopLines []string
		loopLines = append(loopLines, "<b>Loops</b>")
		for _, name := range loopNames {
			l := w.Loops[name]
			loopLines = append(loopLines, fmt.Sprintf("%s: max %d", l.Name, l.MaxIterations))
		}
		metaNodes = append(metaNodes, fmt.Sprintf(`        meta_loops["%s"]`, strings.Join(loopLines, "<br/>")))
	}

	if len(metaNodes) == 0 {
		return ""
	}

	title := w.Name
	if w.Entry != "" {
		title += " (entry: " + w.Entry + ")"
	}

	b.WriteString(fmt.Sprintf("    subgraph workflow_meta[\"%s\"]\n", title))
	b.WriteString("        direction LR\n")
	for _, node := range metaNodes {
		b.WriteString(node)
		b.WriteString("\n")
	}
	b.WriteString("    end\n\n")

	return b.String()
}

// sanitizeID replaces characters that Mermaid cannot handle in node IDs.
func sanitizeID(id string) string {
	return strings.ReplaceAll(id, "-", "_")
}
