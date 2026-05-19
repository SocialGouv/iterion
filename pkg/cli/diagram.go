package cli

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// DiagramOptions holds options for the diagram command.
type DiagramOptions struct {
	File string // .iter file path
	View string // "compact" (default), "detailed", or "full"
}

// DiagramResult holds the output of a diagram command.
type DiagramResult struct {
	File         string `json:"file"`
	WorkflowName string `json:"workflow_name"`
	View         string `json:"view"`
	Mermaid      string `json:"mermaid"`
}

// RunDiagram compiles an .iter file and outputs its Mermaid diagram.
func RunDiagram(opts DiagramOptions, p *Printer) error {
	if opts.File == "" {
		return fmt.Errorf("no file specified")
	}
	opts.File = ResolveRecipePath(opts.File)

	wf, err := runview.CompileWorkflow(opts.File)
	if err != nil {
		return err
	}

	var view ir.MermaidView
	switch opts.View {
	case "", "compact":
		view = ir.MermaidCompact
		opts.View = "compact"
	case "detailed":
		view = ir.MermaidDetailed
	case "full":
		view = ir.MermaidFull
	default:
		// Refuse unknown values rather than silently coercing to
		// compact: the previous behaviour ignored typos like
		// --view detaild and gave the operator no signal.
		return fmt.Errorf("invalid --view %q: expected one of compact, detailed, full", opts.View)
	}

	mermaid := wf.ToMermaid(view)

	result := &DiagramResult{
		File:         opts.File,
		WorkflowName: wf.Name,
		View:         opts.View,
		Mermaid:      mermaid,
	}

	if p.Format == OutputJSON {
		p.JSON(result)
	} else {
		p.Header("Diagram: " + opts.File)
		p.KV("Workflow", wf.Name)
		p.KV("View", opts.View)
		p.Blank()
		fmt.Fprint(p.W, mermaid)
	}

	return nil
}
