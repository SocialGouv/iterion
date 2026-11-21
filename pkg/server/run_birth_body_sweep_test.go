package server

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// T7: every shape a function of this package DECODES — a named type or a
// function-local struct handed to a JSON decoder, in a handler or in a body
// helper it hands the bytes to — is walked field by field, through nested structs,
// pointers, slices and maps. A field whose JSON name carries a run ID
// (`run_id`, `<x>_run_id`, `run_ids`) needs a `<x>_run_birth` sibling, so the
// server compares the run it loads with the one the caller meant; a
// reference list (`references`) is an indirect run address and needs a
// reasoned entry too. Everything else is a named exemption with its reason.
// A new body-carried run ID fails here until it is classified.

// bodyRunExemptions names the body-carried run IDs that carry no birth, and
// why — each one checked to still exist, so the list cannot rot.
var bodyRunExemptions = map[string]string{
	"launchRunRequest.RunID":                    "creates: names the run to create, not an existing one",
	"launchRunRequest.ParentRunID":              "CLI shards: `iterion __scan-shards` names its own parent; no studio sender",
	"dispatchWorkflowRequest.RunID":             "bot-only: the run presenting its forge_publish_token (X-Iterion-Run), authenticated by that token",
	"assistantContextResolveRequest.References": "typed references carry `@<birth>` (cutReferenceBirth)",
	"runJSONShape.ParentRunID":                  "not a request body: the global-active listing decodes run.json files it reads from disk",
	"ForgePublishGrant.Dispatches.RunID":        "not a request body: the persisted forge publish grant, decoded from its own store",
	"workspaceHandoff.SourceRunID":              "not a request body: the durable handoff record WorkspaceHost reloads from disk",
	"workspaceHandoff.DestinationRunID":         "not a request body: the durable handoff record WorkspaceHost reloads from disk",
}

// bodyBirthCarriers are the shapes whose run IDs DO carry a birth sibling —
// asserted to stay found, so the sweep is seen walking the real ones.
var bodyBirthCarriers = []string{
	"launchRunRequest.SourceRunID",
	"createAssistantWatchRequest.AssistantRunID",
	"createAssistantMissionRequest.AssistantRunID",
	"(*WorkspaceHost).createHandoff.req.SourceRunID",
	"(*WorkspaceHost).bindHandoff.req.DestinationRunID",
	"(*WorkspaceHost).completeHandoff.req.DestinationRunID",
}

type bodyRunField struct {
	key      string // Type.Field or func.var.Field
	carrier  bool   // has a birth sibling
	indirect bool   // a reference list
}

// BODY_RUN_SWEEP_OVERLAY names a JSON map of file name → replacement file,
// substituted before the parse the way `go test -overlay` does, so a mutant
// is swept without editing the tree.
func bodyRunSweepOverlay(t *testing.T) map[string]string {
	t.Helper()
	path := os.Getenv("BODY_RUN_SWEEP_OVERLAY")
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	overlay := map[string]string{}
	if err := json.Unmarshal(raw, &overlay); err != nil {
		t.Fatal(err)
	}
	return overlay
}

func TestEveryBodyCarriedRunIDCarriesItsBirth(t *testing.T) {
	fset := token.NewFileSet()
	var files []*ast.File
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	overlay := bodyRunSweepOverlay(t)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	for name := range overlay {
		if _, err := os.Stat(name); os.IsNotExist(err) {
			names = append(names, name)
		}
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if info, err := os.Stat(name); err == nil && info.IsDir() {
			continue
		}
		var src any
		if replacement, ok := overlay[name]; ok {
			b, err := os.ReadFile(replacement)
			if err != nil {
				t.Fatal(err)
			}
			src = b
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	found := sweepBodyRunFields(files)
	if len(found) == 0 {
		t.Fatal("the sweep found no body-carried run ID: it is not reading the decoders")
	}
	seen := map[string]bool{}
	for _, f := range found {
		seen[f.key] = true
		if f.carrier {
			continue
		}
		if _, ok := bodyRunExemptions[f.key]; ok {
			continue
		}
		if f.indirect {
			t.Errorf("%s: a decoded reference list — an indirect run address needs a birth in its syntax and an entry in bodyRunExemptions naming its parser", f.key)
			continue
		}
		t.Errorf("%s: a decoded run ID with no <x>_run_birth sibling — compare the run the handler loads with checkCarriedBirth, or name the exemption and why", f.key)
	}
	for key := range bodyRunExemptions {
		if !seen[key] {
			t.Errorf("exemption %s matches no decoded field: drop it", key)
		}
	}
	for _, key := range bodyBirthCarriers {
		if !seen[key] {
			t.Errorf("carrier %s was not found by the sweep", key)
		}
	}
}

// The sweep can fail: planted shapes — a slice of structs, a map of structs, a
// pointer, a named nested type and a reference list — are each reported, as
// is each decode form (&req, a pointer from &T{} or new(T), `var x = T{}`)
// and a body helper with no *http.Request; a sibling birth silences only its
// own field.
func TestBodyRunSweepFindsPlantedShapes(t *testing.T) {
	const src = `package server
type nestedItem struct { TargetRunID string ` + "`json:\"target_run_id\"`" + ` }
type plantedRequest struct {
	Items  []nestedItem ` + "`json:\"items\"`" + `
	ByName map[string]struct{ ParentRunID string ` + "`json:\"parent_run_id\"`" + ` } ` + "`json:\"by_name\"`" + `
	Ptr    *struct{ RunIDs []string ` + "`json:\"run_ids\"`" + ` } ` + "`json:\"ptr\"`" + `
	Refs   []string ` + "`json:\"references\"`" + `
	SourceRunID    string ` + "`json:\"source_run_id\"`" + `
	SourceRunBirth string ` + "`json:\"source_run_birth\"`" + `
}
func handlePlanted(w http.ResponseWriter, r *http.Request) {
	var req plantedRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	var inline struct { OtherRunID string ` + "`json:\"other_run_id\"`" + ` }
	_ = decodeJSONCapped(s, w, r, &inline, 1)
}
func parseHelper(data []byte) {
	var helper struct { FarRunID string ` + "`json:\"far_run_id\"`" + ` }
	_ = json.Unmarshal(data, &helper)
}
type ptrReq struct { PtrRunID string ` + "`json:\"ptr_run_id\"`" + ` }
type valReq struct { ValRunID string ` + "`json:\"val_run_id\"`" + ` }
type newReq struct { NewRunID string ` + "`json:\"new_run_id\"`" + ` }
func handleForms(w http.ResponseWriter, r *http.Request) {
	a := &ptrReq{}
	_ = json.NewDecoder(r.Body).Decode(a)
	var b = valReq{}
	_ = json.NewDecoder(r.Body).Decode(&b)
	c := new(newReq)
	_ = json.NewDecoder(r.Body).Decode(c)
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bodyRunField{}
	for _, field := range sweepBodyRunFields([]*ast.File{f}) {
		got[field.key] = field
	}
	want := map[string]bodyRunField{
		"plantedRequest.Items.TargetRunID":  {key: "plantedRequest.Items.TargetRunID"},
		"plantedRequest.ByName.ParentRunID": {key: "plantedRequest.ByName.ParentRunID"},
		"plantedRequest.Ptr.RunIDs":         {key: "plantedRequest.Ptr.RunIDs"},
		"plantedRequest.Refs":               {key: "plantedRequest.Refs", indirect: true},
		"plantedRequest.SourceRunID":        {key: "plantedRequest.SourceRunID", carrier: true},
		"handlePlanted.inline.OtherRunID":   {key: "handlePlanted.inline.OtherRunID"},
		"parseHelper.helper.FarRunID":       {key: "parseHelper.helper.FarRunID"},
		"ptrReq.PtrRunID":                   {key: "ptrReq.PtrRunID"},
		"valReq.ValRunID":                   {key: "valReq.ValRunID"},
		"newReq.NewRunID":                   {key: "newReq.NewRunID"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("planted sweep:\n got %v\nwant %v", got, want)
	}
}

// sweepBodyRunFields finds the run-carrying fields of every shape a function
// of files decodes.
func sweepBodyRunFields(files []*ast.File) []bodyRunField {
	types := map[string]ast.Expr{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				types[ts.Name.Name] = ts.Type
			}
		}
	}
	var out []bodyRunField
	done := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			// Every function of the package, not only those taking the
			// *http.Request: a body helper handed the bytes decodes a request
			// as surely as the handler does. A decode of something else (a
			// file, a forge's answer) is a named exemption.
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			vars := localVarTypes(fn.Body)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isDecodeCall(call) {
					return true
				}
				for _, arg := range call.Args {
					// &req, or req itself when it holds a pointer (req := &T{},
					// req := new(T)).
					var id *ast.Ident
					switch a := arg.(type) {
					case *ast.UnaryExpr:
						if a.Op == token.AND {
							id, _ = a.X.(*ast.Ident)
						}
					case *ast.Ident:
						id = a
					}
					if id == nil {
						continue
					}
					typ, ok := vars[id.Name]
					if !ok {
						continue
					}
					var root string
					if named, ok := typ.(*ast.Ident); ok {
						root = named.Name
					} else {
						root = funcLabel(fn) + "." + id.Name
					}
					if done[root] {
						continue
					}
					done[root] = true
					out = append(out, walkRunFields(types, root, typ, map[string]bool{})...)
				}
				return true
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// constructedType is the type an initializer builds: T{} and &T{} give T,
// new(T) gives T; anything else nil.
func constructedType(e ast.Expr) ast.Expr {
	switch v := e.(type) {
	case *ast.CompositeLit:
		return v.Type
	case *ast.UnaryExpr:
		if lit, ok := v.X.(*ast.CompositeLit); ok && v.Op == token.AND {
			return lit.Type
		}
	case *ast.CallExpr:
		if fn, ok := v.Fun.(*ast.Ident); ok && fn.Name == "new" && len(v.Args) == 1 {
			return v.Args[0]
		}
	}
	return nil
}

func isDecodeCall(call *ast.CallExpr) bool {
	name := ""
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		name = fn.Sel.Name
	}
	lower := strings.ToLower(name)
	return strings.Contains(lower, "decode") || strings.Contains(lower, "unmarshal") || strings.Contains(lower, "readjson")
}

// localVarTypes maps each local of a body to the type it holds or points to:
// `var x T`, `var x = T{}`, `x := T{}`, `x := &T{}`, `x := new(T)`.
func localVarTypes(body *ast.BlockStmt) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.ValueSpec:
			for i, name := range s.Names {
				if s.Type != nil {
					out[name.Name] = s.Type
				} else if i < len(s.Values) {
					if typ := constructedType(s.Values[i]); typ != nil {
						out[name.Name] = typ
					}
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || i >= len(s.Rhs) {
					continue
				}
				if typ := constructedType(s.Rhs[i]); typ != nil {
					out[id.Name] = typ
				}
			}
		}
		return true
	})
	return out
}

func funcLabel(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		if id, ok := star.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fn.Name.Name
		}
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// walkRunFields lists the run-carrying fields under typ, recursing through
// named types of the package, pointers, slices, arrays and map values.
func walkRunFields(types map[string]ast.Expr, path string, typ ast.Expr, visiting map[string]bool) []bodyRunField {
	switch t := typ.(type) {
	case *ast.Ident:
		def, ok := types[t.Name]
		if !ok || visiting[t.Name] {
			return nil
		}
		visiting[t.Name] = true
		defer delete(visiting, t.Name)
		return walkRunFields(types, path, def, visiting)
	case *ast.StarExpr:
		return walkRunFields(types, path, t.X, visiting)
	case *ast.ArrayType:
		return walkRunFields(types, path, t.Elt, visiting)
	case *ast.MapType:
		return walkRunFields(types, path, t.Value, visiting)
	case *ast.StructType:
		names := map[string]bool{}
		for _, field := range t.Fields.List {
			for _, n := range field.Names {
				names[jsonName(field, n.Name)] = true
			}
		}
		var out []bodyRunField
		for _, field := range t.Fields.List {
			if len(field.Names) == 0 {
				// An embedded type's fields are promoted into this shape.
				out = append(out, walkRunFields(types, path, field.Type, visiting)...)
				continue
			}
			for _, n := range field.Names {
				name := jsonName(field, n.Name)
				key := path + "." + n.Name
				switch {
				case name == "-":
				case name == "run_id" || strings.HasSuffix(name, "_run_id") || name == "run_ids" || strings.HasSuffix(name, "_run_ids"):
					birth := strings.TrimSuffix(strings.TrimSuffix(name, "s"), "_id") + "_birth"
					if strings.HasSuffix(name, "s") {
						birth += "s"
					}
					out = append(out, bodyRunField{key: key, carrier: names[birth]})
				case name == "references" || strings.HasSuffix(name, "_references"):
					out = append(out, bodyRunField{key: key, indirect: true})
				default:
					out = append(out, walkRunFields(types, key, field.Type, visiting)...)
				}
			}
		}
		return out
	}
	return nil
}

// jsonName is the field's JSON key: its tag's name, else the Go name the
// decoder matches case-insensitively, folded to snake-ish lower case.
func jsonName(field *ast.Field, goName string) string {
	if field.Tag != nil {
		tag, err := strconv.Unquote(field.Tag.Value)
		if err == nil {
			if v, ok := reflect.StructTag(tag).Lookup("json"); ok {
				name, _, _ := strings.Cut(v, ",")
				if name != "" {
					return name
				}
			}
		}
	}
	var b strings.Builder
	for i, r := range goName {
		if i > 0 && r >= 'A' && r <= 'Z' && (goName[i-1] < 'A' || goName[i-1] > 'Z') {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}
