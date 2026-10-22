package ir

import "github.com/SocialGouv/iterion/pkg/dsl/expr"

// walkRootVarsSubscripts finds vars[key] locally: Refs reports the receiver as
// a root vars path with no key, so changing it would affect every ref consumer.
func walkRootVarsSubscripts(ast *expr.AST, yield func(index *expr.Snapshot)) {
	if ast == nil || yield == nil {
		return
	}
	var walk func(*expr.Snapshot)
	walk = func(n *expr.Snapshot) {
		if n == nil {
			return
		}
		if n.Kind == expr.SnapIndex && len(n.Children) == 2 {
			recv := n.Children[0]
			if recv.Kind == expr.SnapPath && recv.Namespace == "vars" && len(recv.Path) == 0 {
				yield(n.Children[1])
			}
		}
		for _, child := range n.Children {
			walk(child)
		}
		walk(n.LambdaBody)
	}
	walk(expr.ToSnapshot(ast))
}

func hasRootVarsSubscript(ast *expr.AST) bool {
	found := false
	walkRootVarsSubscripts(ast, func(*expr.Snapshot) {
		found = true
	})
	return found
}
