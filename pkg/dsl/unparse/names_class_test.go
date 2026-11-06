package unparse

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// namesReferencedNotDeclared are the string fields written bare that
// checkNames does not hold, each for its reason. A field written bare that
// is in neither checkNames nor this list fails the test below.
var namesReferencedNotDeclared = map[string]bool{
	// References to a name declared elsewhere — held at its declaration;
	// the studio picks them from what exists.
	"Edge.From":              true,
	"Edge.To":                true,
	"UseDecl.Group":          true,
	"LLMDecl.Input":          true,
	"HumanDecl.Input":        true,
	"HumanDecl.Output":       true,
	"ToolNodeDecl.Input":     true,
	"ToolNodeDecl.Output":    true,
	"ComputeDecl.Input":      true,
	"SubbotDecl.Output":      true,
	"WaitDecl.Output":        true,
	"SupervisorDecl.Watches": true,
	"LLMDecl.Output":         true,
	"ComputeDecl.Output":     true,
	"AwaitAnswersDecl.From":  true,
	"WorkflowDecl.Entry":     true,
	"WorkflowDecl.Contract":  true,
	"MCPConfigDecl.Servers":  true,
	// A path, refused by the parser as a path (E045), not as a name.
	"ImportDecl.Path": true,
	// Enumerated values the studio offers as a closed list, refused by the
	// parser with a message that names the property and the values.
	"SecretField.As":                   true,
	"SandboxBlock.Mode":                true,
	"WorkflowDecl.Worktree":            true,
	"HumanDecl.Posture":                true,
	"HumanDecl.MergeStrategy":          true,
	"LLMDecl.ReasoningEffort":          true,
	"FallbackDecl.On":                  true,
	"FallbackDecl.Action":              true,
	"ToolNodeDecl.Language":            true,
	"WorkflowDecl.RepoDevbox":          true,
	"WorkflowDecl.Permission":          true,
	"WorkflowDecl.WorkspaceCheckpoint": true,
	"WorkflowDecl.AmbientContext":      true,
	// Expressions, refused with an expression error that names the field.
	"WhenClause.Condition":         true,
	"LoopClause.MaxIterationsExpr": true,
}

// The class, not the sites: every string a bot in the tree carries is set,
// in turn, to a name with a space. Whatever the writer puts down bare and
// the re-read then tears apart must be refused by checkNames, by name —
// never as the re-read's parse errors. A field added to the AST and written
// bare without a check fails here.
func TestVerify_EveryBareNameIsSaidByName(t *testing.T) {
	if testing.Short() {
		t.Skip("walks every string field of the tree's bots")
	}
	root := filepath.Join("..", "..", "..")
	var files []string
	for _, dir := range []string{"examples", "bots"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".bot") {
				files = append(files, p)
			}
			return nil
		})
	}
	if len(files) == 0 {
		t.Fatal("no .bot file found under examples/ or bots/")
	}
	// A field is tried at three of its occurrences: one tears the text if
	// the writer puts it down bare, and three keep the walk fast.
	seen, tried := map[string]bool{}, map[string]int{}
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		res, err := parser.Parse(path, string(src))
		if err != nil || res.File == nil || hasParseError(res.Diagnostics) {
			continue
		}
		f := res.File
		// No bot of the tree is refused for its names: the check holds only
		// what the syntax cannot read, never what a working bot writes.
		if err := checkNames(f); err != nil {
			t.Errorf("%s: a working bot is refused: %v", path, err)
			continue
		}
		if Verify(f, Unparse(f)) != nil {
			continue // a bot the writer cannot hold as it is says nothing here
		}
		forEachString(reflect.ValueOf(f), "", func(field string, v reflect.Value) {
			if namesReferencedNotDeclared[field] || seen[field] || v.String() == "" || tried[field] >= 3 {
				return
			}
			tried[field]++
			old := v.String()
			v.SetString("x y")
			err := Verify(f, Unparse(f))
			v.SetString(old)
			if err != nil && strings.Contains(err.Error(), "does not parse") {
				seen[field] = true
				t.Errorf("%s: %s = %q tears the text and is not said by name: %v", filepath.Base(path), field, "x y", err)
			}
		})
	}
}

func hasParseError(ds []parser.Diagnostic) bool {
	for _, d := range ds {
		if d.Severity == parser.SeverityError {
			return true
		}
	}
	return false
}

// forEachString calls fn on every settable string field reachable from v,
// named Type.Field, skipping spans and comments.
func forEachString(v reflect.Value, field string, fn func(string, reflect.Value)) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			forEachString(v.Elem(), field, fn)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			forEachString(v.Index(i), field, fn)
		}
	case reflect.Struct:
		t := v.Type()
		if t == reflect.TypeOf(ast.Span{}) || t == reflect.TypeOf(ast.Comment{}) {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			name := t.Name() + "." + sf.Name
			if sf.Anonymous {
				name = field
			}
			forEachString(v.Field(i), name, fn)
		}
	case reflect.String:
		if v.CanSet() {
			fn(field, v)
		}
	}
}
