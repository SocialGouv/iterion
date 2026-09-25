package cli

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// DiagramOptions holds options for the diagram command.
type DiagramOptions struct {
	File string // .bot file path, or an author document (x.bot.yaml)
	View string // "compact" (default), "detailed", or "full"
}

// DiagramResult holds the output of a diagram command.
type DiagramResult struct {
	File         string `json:"file"`
	WorkflowName string `json:"workflow_name"`
	View         string `json:"view"`
	Mermaid      string `json:"mermaid"`
	// SourceKind is "author" when the file drawn is an author document —
	// the diagram is then the one of the .bot it stands for, BotPath, which
	// the diagram never writes. As on validate.
	SourceKind string `json:"source_kind,omitempty"`
	BotPath    string `json:"bot_path,omitempty"`
}

// RunDiagram compiles a .bot file and outputs its Mermaid diagram. An
// author document is drawn as the .bot it stands for
// (compileAuthorDocument): the diagram `diagram x.bot` draws once
// `fmt --to bot` has written it.
func RunDiagram(opts DiagramOptions, p *Printer) error {
	if opts.File == "" {
		return fmt.Errorf("no file specified")
	}
	opts.File = ResolveRecipePath(opts.File)
	document := workflowfile.IsAuthorDocument(opts.File)
	if !document && !workflowfile.IsWorkflowFile(opts.File) {
		return fmt.Errorf("diagram file %q must end in .bot, or be an author document (.bot.yaml)", opts.File)
	}
	// The .bot a document stands for is one `fmt --to bot` writes and
	// `diagram` then takes by name — never `UPPER.BOT`, which neither does.
	if document {
		if bot, ok := twinNameReadBack("bot", opts.File); !ok {
			return fmt.Errorf("%s", twinNameRefusal("diagram", opts.File, bot))
		}
	}
	if err := requireWorkflowPathExists(opts.File); err != nil {
		return err
	}

	var wf *ir.Workflow
	var doc *authorDocument
	if document {
		var err error
		if wf, doc, err = compileAuthorDocument(opts.File); err != nil {
			return err
		}
	} else {
		// A bundle's main.bot is promoted to its bundle, as validate does: its
		// prompts/*.md in scope, or the diagram of a multi-file bot fails C003.
		var err error
		if wf, _, _, err = runview.CompileWorkflowPath(opts.File); err != nil {
			return err
		}
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
	if doc != nil {
		result.SourceKind = "author"
		result.BotPath = doc.botPath
	}

	if p.Format == OutputJSON {
		p.JSON(result)
	} else {
		p.Header("Diagram: " + opts.File)
		if doc != nil {
			p.KV("Reads as", doc.botPath)
		}
		p.KV("Workflow", wf.Name)
		p.KV("View", opts.View)
		p.Blank()
		fmt.Fprint(p.W, mermaid)
	}

	return nil
}
