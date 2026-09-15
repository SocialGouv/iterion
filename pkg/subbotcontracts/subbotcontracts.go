// Package subbotcontracts reads the `subbot` children of a workflow for
// the contracts they keep — what bundlelint holds a parent's `with:` to
// (C255). It sits beside pkg/bundle (which confines the read to the
// bundle's collection) and pkg/dsl/ir (which compiles a child): bundle
// cannot import ir, and bundlelint stays I/O-free.
package subbotcontracts

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// Read resolves each `subbot` child of w within the bundle's collection
// (bundle.ResolveChild), compiles it as its own unit and returns the
// contract its workflow keeps, by the parent's node id. A child beyond the
// collection, unreadable, or compiling with an error is left out: C253
// names an unread child, and a child's own errors are the child's to show.
// A source several nodes name is read and compiled once.
func Read(dir, parent string, w *ir.Workflow) map[string]*ir.PublicContract {
	if w == nil {
		return nil
	}
	out := map[string]*ir.PublicContract{}
	byPath := map[string]*ir.PublicContract{}
	for id, n := range w.Nodes {
		sb, ok := n.(*ir.SubbotNode)
		if !ok || strings.HasPrefix(sb.Source, "bot://") {
			continue
		}
		path, src, ok := bundle.ResolveChild(dir, parent, sb.Source)
		if !ok {
			continue
		}
		contract, seen := byPath[path]
		if !seen {
			contract = compiledContract(path, src)
			byPath[path] = contract
		}
		if contract != nil {
			out[id] = contract
		}
	}
	return out
}

// compiledContract is the contract a child's workflow keeps — nil when the
// child does not load or compile clean, or keeps none.
func compiledContract(path string, src []byte) *ir.PublicContract {
	u := unit.LoadDirWithMain(path, path, src)
	if u.HasErrors() || u.Merged == nil {
		return nil
	}
	child := ir.Compile(u.Merged)
	if child.Workflow == nil || child.Workflow.Contract == nil {
		return nil
	}
	for _, d := range child.Diagnostics {
		if d.Severity == ir.SeverityError {
			return nil
		}
	}
	return child.Workflow.Contract
}
