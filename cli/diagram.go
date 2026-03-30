package cli

import (
	"fmt"

	"github.com/SocialGouv/iterion/ir"
)

// DiagramOptions holds options for the diagram command.
type DiagramOptions struct {
	File     string // .iter file path
	Detailed bool   // use detailed view (default: compact)
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

	wf, err := compileWorkflow(opts.File)
	if err != nil {
		return err
	}

	view := ir.MermaidCompact
	viewName := "compact"
	if opts.Detailed {
		view = ir.MermaidDetailed
		viewName = "detailed"
	}

	mermaid := wf.ToMermaid(view)

	result := &DiagramResult{
		File:         opts.File,
		WorkflowName: wf.Name,
		View:         viewName,
		Mermaid:      mermaid,
	}

	if p.Format == OutputJSON {
		p.JSON(result)
	} else {
		p.Header("Diagram: " + opts.File)
		p.KV("Workflow", wf.Name)
		p.KV("View", viewName)
		p.Blank()
		fmt.Fprint(p.W, mermaid)
	}

	return nil
}
