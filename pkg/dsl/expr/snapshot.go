package expr

// SnapKind classifies a Snapshot node.
type SnapKind int

const (
	SnapBool SnapKind = iota
	SnapInt
	SnapFloat
	SnapString
	SnapPath
	SnapUnary
	SnapBinary
	SnapFuncCall
	SnapIndex      // recv[index] — Children: [recv, index]
	SnapLambdaComb // map/filter/reduce — Func set; Children: [coll] or [coll, init]
)

// Snapshot is a read-only, exported mirror of one expression-AST node. The
// concrete node types (litString, pathNode, binaryNode, …) are unexported, so
// packages outside expr — notably dsl/ir's static type checker — walk the
// tree through this projection instead of reaching into expr internals. It is
// built by ToSnapshot and is otherwise inert (the evaluator never reads it).
//
// Literal node Kinds (SnapBool/SnapInt/SnapFloat) carry no value field on
// purpose: the only consumer needs the Kind to infer a type, and the string
// literal value (the one case where the value matters — enum comparison) is
// kept in Str. Reintroduce a typed value field if a future check needs it.
type Snapshot struct {
	Kind      SnapKind
	Op        string // operator for SnapUnary ("!"/"-") and SnapBinary ("&&","==","+",…)
	Func      string // function name for SnapFuncCall
	Namespace string // path namespace for SnapPath (vars, input, outputs, …)
	Path      []string
	Str       string      // string-literal value (SnapString)
	Children  []*Snapshot // operands (unary: 1, binary: 2, funccall: N)
}

// ToSnapshot returns a read-only mirror of the AST's root node, or nil for a
// nil/empty AST. The returned tree shares no mutable state with the AST and
// faithfully mirrors it — path namespaces are NOT normalized; callers that
// resolve paths must apply NormalizePath the way the evaluator does.
func ToSnapshot(a *AST) *Snapshot {
	if a == nil || a.root == nil {
		return nil
	}
	return snap(a.root)
}

func snap(n node) *Snapshot {
	switch t := n.(type) {
	case litBool:
		return &Snapshot{Kind: SnapBool}
	case litInt:
		return &Snapshot{Kind: SnapInt}
	case litFloat:
		return &Snapshot{Kind: SnapFloat}
	case litString:
		return &Snapshot{Kind: SnapString, Str: t.v}
	case pathNode:
		return &Snapshot{Kind: SnapPath, Namespace: t.namespace, Path: append([]string(nil), t.path...)}
	case *unaryNode:
		return &Snapshot{Kind: SnapUnary, Op: t.op, Children: []*Snapshot{snap(t.child)}}
	case *binaryNode:
		return &Snapshot{Kind: SnapBinary, Op: t.op, Children: []*Snapshot{snap(t.left), snap(t.right)}}
	case *funcCallNode:
		kids := make([]*Snapshot, len(t.args))
		for i, a := range t.args {
			kids[i] = snap(a)
		}
		return &Snapshot{Kind: SnapFuncCall, Func: t.name, Children: kids}
	case *indexNode:
		return &Snapshot{Kind: SnapIndex, Children: []*Snapshot{snap(t.recv), snap(t.index)}}
	case *lambdaCombNode:
		// The lambda body is deliberately NOT mirrored: it references
		// lambda-bound parameters that are not real schema refs, so the
		// conservative static type-walk must not descend into it. Only the
		// collection (and reduce's init) are external expressions.
		kids := []*Snapshot{snap(t.coll)}
		if t.init != nil {
			kids = append(kids, snap(t.init))
		}
		return &Snapshot{Kind: SnapLambdaComb, Func: t.name, Children: kids}
	}
	return nil
}

// evalNamespaces is the canonical set of namespaces the evaluator resolves
// explicitly in resolvePath; it is the single source of truth for "is this a
// known namespace" shared with NormalizePath. Keep it in sync with the
// switch in resolvePath (expr.go). Note: `secrets` and `attachments` are NOT
// here — they are template-substitution namespaces, never resolved inside an
// expression, so a leading `secrets`/`attachments` identifier in an
// expression falls through to the implicit-input rule below.
var evalNamespaces = map[string]bool{
	"vars": true, "input": true, "outputs": true,
	"artifacts": true, "loop": true, "run": true,
}

// NormalizePath applies the evaluator's bare-identifier rule: a leading
// identifier that is not a known expression namespace is really a field of
// the implicit `input` namespace (so `approved` means `input.approved`, and
// `outputs.x.y` stays as-is). Static consumers MUST normalize paths this way
// to resolve them exactly as the runtime does. Mirrors the bare-identifier
// fallback in evalNode/resolvePath.
func NormalizePath(namespace string, path []string) (string, []string) {
	if evalNamespaces[namespace] {
		return namespace, path
	}
	return "input", append([]string{namespace}, path...)
}
