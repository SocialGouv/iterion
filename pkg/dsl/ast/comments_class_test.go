package ast

import (
	goast "go/ast"
	goparser "go/parser"
	"go/token"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestEveryDeclarationKindCarriesComments holds the CLASS: every kind of
// declaration a file holds must be able to carry the `##` lines written
// around it, and must be in CommentCarriers — the one list the parser
// attaches through and the writer places through. A declaration kind added
// to File without a `Comments` field, or with one CommentCarriers does not
// return, silently drops every comment written around it on the first save
// (the whole of #1282, one kind at a time).
//
// `Imports` is the one deliberate exception: its transport is a list of
// paths with nothing to hang a comment on, so a comment written around an
// import stays at the file's head.
func TestEveryDeclarationKindCarriesComments(t *testing.T) {
	skip := map[string]string{
		"Imports":  "a list of paths in the transport: nothing carries a comment",
		"Comments": "the file's own",
		"Span":     "a position, not a declaration",
		"Profile":  "the `dsl:` header, not a declaration",
	}
	ft := reflect.TypeOf(File{})
	var missing []string
	kinds := 0
	for i := 0; i < ft.NumField(); i++ {
		f := ft.Field(i)
		if !f.IsExported() {
			continue
		}
		if _, ok := skip[f.Name]; ok {
			continue
		}
		el := f.Type
		for el.Kind() == reflect.Slice || el.Kind() == reflect.Pointer {
			el = el.Elem()
		}
		if el.Kind() != reflect.Struct {
			t.Errorf("File.%s is a %s — the sweep does not know whether it declares anything", f.Name, f.Type)
			continue
		}
		kinds++
		if _, has := el.FieldByName("Comments"); !has {
			missing = append(missing, "ast."+el.Name()+" (File."+f.Name+")")
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d declaration kinds carry no comments: %s", len(missing), strings.Join(missing, ", "))
	}
	// An edge is a declaration's line too, and carries its own.
	if _, has := reflect.TypeOf(Edge{}).FieldByName("Comments"); !has {
		t.Error("ast.Edge carries no comments: every comment written around an edge would be dropped")
	}
	if kinds < 20 {
		t.Errorf("the sweep found only %d declaration kinds on ast.File — it has gone blind", kinds)
	}
	t.Logf("%d declaration kinds + ast.Edge carry their comments", kinds)
}

// TestCommentCarriersCoversEveryKind: the enumeration the parser and the
// writer share returns one entry per declaration a file holds, under the
// keyword that opens it — read against the file's own fields, so a kind
// added to File and forgotten in CommentCarriers is named here.
func TestCommentCarriersCoversEveryKind(t *testing.T) {
	f := &File{}
	// One declaration of every kind, allocated reflectively so a kind
	// added to File is in the fixture without anyone listing it.
	fv := reflect.ValueOf(f).Elem()
	ft := fv.Type()
	want := map[string]bool{}
	for i := 0; i < ft.NumField(); i++ {
		fd := ft.Field(i)
		if !fd.IsExported() || fd.Name == "Comments" || fd.Name == "Imports" || fd.Name == "Span" || fd.Name == "Profile" {
			continue
		}
		switch fd.Type.Kind() {
		case reflect.Pointer:
			fv.Field(i).Set(reflect.New(fd.Type.Elem()))
		case reflect.Slice:
			s := reflect.MakeSlice(fd.Type, 1, 1)
			s.Index(0).Set(reflect.New(fd.Type.Elem().Elem()))
			fv.Field(i).Set(s)
		default:
			t.Fatalf("File.%s is a %s — the fixture does not know how to make one", fd.Name, fd.Type)
		}
		want[fd.Name] = true
	}
	got := CommentCarriers(f)
	if len(got) != len(want) {
		var kinds []string
		for _, c := range got {
			kinds = append(kinds, c.Kind)
		}
		sort.Strings(kinds)
		t.Fatalf("CommentCarriers returned %d carriers for %d declaration kinds: %v", len(got), len(want), kinds)
	}
	seen := map[string]bool{}
	for _, c := range got {
		if c.Comments == nil {
			t.Errorf("carrier %q has no comment list", c.Kind)
		}
		if seen[c.Kind] {
			t.Errorf("two carriers under the keyword %q: an address cannot name one of them", c.Kind)
		}
		seen[c.Kind] = true
	}
}

// TestCommentsTravelOnEveryCarrier: a comment put on each declaration of a
// file comes back through the JSON transport on the SAME declaration, with
// its address. The copy is done by reflection (comments.go), so this is
// what holds it: a mirror struct that lost its `Comments` field, or a
// rename on either side, stops the walk there and the comments of that
// declaration never travel.
func TestCommentsTravelOnEveryCarrier(t *testing.T) {
	f := &File{}
	fv := reflect.ValueOf(f).Elem()
	ft := fv.Type()
	for i := 0; i < ft.NumField(); i++ {
		fd := ft.Field(i)
		if !fd.IsExported() || fd.Name == "Comments" || fd.Name == "Imports" || fd.Name == "Span" || fd.Name == "Profile" {
			continue
		}
		switch fd.Type.Kind() {
		case reflect.Pointer:
			fv.Field(i).Set(reflect.New(fd.Type.Elem()))
		case reflect.Slice:
			s := reflect.MakeSlice(fd.Type, 1, 1)
			s.Index(0).Set(reflect.New(fd.Type.Elem().Elem()))
			fv.Field(i).Set(s)
		}
	}
	for i, c := range CommentCarriers(f) {
		*c.Comments = []*Comment{{Text: "on " + c.Kind, Anchor: "a" + itoa(i), Place: CommentAtEnd}}
	}
	f.Comments = []*Comment{{Text: "the head"}}
	raw, err := MarshalFile(f)
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Comments) != 1 || back.Comments[0].Text != "the head" {
		t.Errorf("the file's head came back as %v", back.Comments)
	}
	ca, cb := CommentCarriers(f), CommentCarriers(back)
	if len(ca) != len(cb) {
		t.Fatalf("%d carriers went out, %d came back", len(ca), len(cb))
	}
	for i := range ca {
		want, got := *ca[i].Comments, *cb[i].Comments
		if len(got) != 1 {
			t.Errorf("%s: %d comments came back, want 1", ca[i].Kind, len(got))
			continue
		}
		if got[0].Text != want[0].Text || got[0].Anchor != want[0].Anchor || got[0].Place != want[0].Place {
			t.Errorf("%s: %+v came back as %+v", ca[i].Kind, *want[0], *got[0])
		}
	}
	// An edge carries its own through the same walk.
	f.Workflows[0].Edges = []*Edge{{From: "a", To: "b", Comments: []*Comment{{Text: "on the edge"}}}}
	raw, err = MarshalFile(f)
	if err != nil {
		t.Fatal(err)
	}
	back, err = UnmarshalFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Workflows[0].Edges) != 1 || len(back.Workflows[0].Edges[0].Comments) != 1 {
		t.Fatalf("the edge's comment did not travel: %+v", back.Workflows[0].Edges)
	}
}

// TestAnUnknownCommentPlaceIsRefused: the transport names a place with a
// word; a word the enum does not have is refused rather than read as
// another place, which would move the comment onto its own line or off it.
func TestAnUnknownCommentPlaceIsRefused(t *testing.T) {
	_, err := UnmarshalFile([]byte(`{"comments":[{"text":"x","place":"trailng"}]}`))
	if err == nil {
		t.Fatal("a comment place the enum does not name was accepted")
	}
	if !strings.Contains(err.Error(), "place") {
		t.Fatalf("refused for another reason: %v", err)
	}
	for _, place := range []string{"", "before", "end", "trailing"} {
		if _, err := UnmarshalFile([]byte(`{"comments":[{"text":"x","place":"` + place + `"}]}`)); err != nil {
			t.Errorf("place %q refused: %v", place, err)
		}
	}
}

// TestEveryCarrierMirrorHasTheCommentsField reads the JSON mirror structs
// of ast's own source: a carrier whose mirror lost the field is where the
// reflective walk stops, and the sweep above would only see it if the
// fixture happened to reach that declaration.
func TestEveryCarrierMirrorHasTheCommentsField(t *testing.T) {
	fset := token.NewFileSet()
	var missing []string
	for _, file := range []string{"jsonenc.go", "jsonenc_contract.go"} {
		src, err := goparser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		goast.Inspect(src, func(n goast.Node) bool {
			ts, ok := n.(*goast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*goast.StructType)
			if !ok || !strings.HasPrefix(ts.Name.Name, "json") {
				return true
			}
			if !strings.HasSuffix(ts.Name.Name, "Decl") && !strings.HasSuffix(ts.Name.Name, "Block") && ts.Name.Name != "jsonEdge" {
				return true
			}
			// Only the mirrors of the carriers: the AST type of the same
			// name has a Comments field.
			astName := strings.TrimPrefix(ts.Name.Name, "json")
			if !astTypeCarriesComments(astName) {
				return true
			}
			for _, f := range st.Fields.List {
				for _, nm := range f.Names {
					if nm.Name == "Comments" {
						return true
					}
				}
			}
			missing = append(missing, ts.Name.Name)
			return true
		})
	}
	if len(missing) > 0 {
		t.Errorf("mirrors of comment carriers with no Comments field: %s", strings.Join(missing, ", "))
	}
}

func astTypeCarriesComments(name string) bool {
	for _, v := range []any{VarsBlock{}, PresetsBlock{}, AttachmentsBlock{}, SecretsBlock{}, MCPServerDecl{}, PromptDecl{},
		SchemaDecl{}, ContractDecl{}, CursorDecl{}, SupervisorDecl{}, AgentDecl{}, JudgeDecl{}, RouterDecl{}, HumanDecl{},
		ToolNodeDecl{}, ComputeDecl{}, EmitDecl{}, WaitDecl{}, AwaitAnswersDecl{}, FailDecl{}, GroupDecl{}, UseDecl{},
		SubbotDecl{}, WorkflowDecl{}, Edge{}} {
		if reflect.TypeOf(v).Name() == name {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// TestNoCommentTheTransportTakesIsLost is the class guarantee, stated where
// it can be checked without naming a kind: a comment set on ANY `Comments`
// field the transport reaches must still be in the document after a round
// trip, and must be reachable from the one enumeration the writer places
// through (AllCommentTexts / CommentCarriers).
//
// The shape it was written for: a group's MEMBERS have a `Comments` field
// the reflective transport copies, and the writer places through the GROUP,
// not through them. A document setting one was accepted and the comment
// written nowhere. The decoder now folds it onto the group under the path
// the parser gives it, and this test is what holds that — and what would
// catch the next declaration type added in the same position.
func TestNoCommentTheTransportTakesIsLost(t *testing.T) {
	f := &File{}
	fv := reflect.ValueOf(f).Elem()
	ft := fv.Type()
	for i := 0; i < ft.NumField(); i++ {
		fd := ft.Field(i)
		if !fd.IsExported() || fd.Name == "Comments" || fd.Name == "Imports" || fd.Name == "Span" || fd.Name == "Profile" {
			continue
		}
		switch fd.Type.Kind() {
		case reflect.Pointer:
			fv.Field(i).Set(reflect.New(fd.Type.Elem()))
		case reflect.Slice:
			s := reflect.MakeSlice(fd.Type, 1, 1)
			s.Index(0).Set(reflect.New(fd.Type.Elem().Elem()))
			fv.Field(i).Set(s)
		}
	}
	// A group with one member of every kind it can hold, and an edge.
	g := f.Groups[0]
	g.Name = "pair"
	g.Agents = []*AgentDecl{{Name: "ga"}}
	g.Judges = []*JudgeDecl{{Name: "gj"}}
	g.Routers = []*RouterDecl{{Name: "gr"}}
	g.Humans = []*HumanDecl{{Name: "gh"}}
	g.Tools = []*ToolNodeDecl{{Name: "gt"}}
	g.Computes = []*ComputeDecl{{Name: "gc"}}
	g.Edges = []*Edge{{From: "ga", To: "gj"}}
	f.Workflows[0].Name = "w"
	f.Workflows[0].Edges = []*Edge{{From: "a", To: "b"}}

	// Stamp a distinct comment on every Comments field there is.
	want := map[string]bool{}
	n := 0
	var stamp func(v reflect.Value)
	stamp = func(v reflect.Value) {
		v = derefValue(v)
		if !v.IsValid() {
			return
		}
		switch v.Kind() {
		case reflect.Struct:
			if v.Type() == reflect.TypeOf(Comment{}) {
				return
			}
			for i := 0; i < v.NumField(); i++ {
				fd := v.Type().Field(i)
				if !fd.IsExported() {
					continue
				}
				if fd.Name == commentsField && v.Field(i).Kind() == reflect.Slice && v.Field(i).CanSet() {
					n++
					text := "comment " + itoa(n)
					want[text] = true
					v.Field(i).Set(reflect.ValueOf([]*Comment{{Text: text}}))
					continue
				}
				stamp(v.Field(i))
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				stamp(v.Index(i))
			}
		}
	}
	stamp(reflect.ValueOf(f))
	if n < 25 {
		t.Fatalf("only %d Comments fields were found — the walk has gone blind", n)
	}

	raw, err := MarshalFile(f)
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tx := range AllCommentTexts(back) {
		got[tx] = true
	}
	var lost []string
	for tx := range want {
		if !got[tx] {
			lost = append(lost, tx)
		}
	}
	if len(lost) > 0 {
		sort.Strings(lost)
		t.Errorf("%d of %d comments the transport took are reachable from no carrier: %s",
			len(lost), n, strings.Join(lost, ", "))
	}
	t.Logf("%d Comments fields stamped, all reachable from CommentCarriers", n)
}

// TestAGroupsCommentsKeepTheirFileThroughTheFold: the fold that moves a
// group MEMBER's comments onto the group changes slice lengths, and
// provenance's walk pairs slices element for element — it stops where they
// differ. Run before provenance, the fold cost the group every comment's
// file of origin, which is what a save of a bot in several files routes on.
func TestAGroupsCommentsKeepTheirFileThroughTheFold(t *testing.T) {
	f := &File{
		Groups: []*GroupDecl{{
			Name:     "pair",
			Span:     Span{Start: Pos{File: "lib/x.bot"}, End: Pos{File: "lib/x.bot"}},
			Comments: []*Comment{{Text: "on the group", Span: Span{Start: Pos{File: "lib/x.bot"}, End: Pos{File: "lib/x.bot"}}}},
			Agents: []*AgentDecl{{
				Name:     "ga",
				Span:     Span{Start: Pos{File: "lib/x.bot"}, End: Pos{File: "lib/x.bot"}},
				Comments: []*Comment{{Text: "on the member", Span: Span{Start: Pos{File: "lib/x.bot"}, End: Pos{File: "lib/x.bot"}}}},
			}},
		}},
	}
	raw, err := MarshalFileWithProvenance(f, "")
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	g := back.Groups[0]
	if len(g.Comments) != 2 {
		t.Fatalf("the group carries %d comments after the fold, want 2: %+v", len(g.Comments), g.Comments)
	}
	for _, c := range g.Comments {
		if c.Span.Start.File != "lib/x.bot" {
			t.Errorf("the comment %q came back from %q, want lib/x.bot", c.Text, c.Span.Start.File)
		}
	}
	if len(g.Agents[0].Comments) != 0 {
		t.Errorf("the member kept its comments as well as the group: %+v", g.Agents[0].Comments)
	}
}

// TestACommentTextIsCanonicalisedOnTheWayIn: the text a comment can be
// written back as has one space after the hashes and no trailing
// whitespace (workflowfile.CommentBody). A canvas holding the untrimmed
// form made its WHOLE document unsaveable — the round trip could not
// produce that text, so the guard refused every save, naming a character
// nobody can see.
func TestACommentTextIsCanonicalisedOnTheWayIn(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"hello ", "hello"},
		{"hello\t", "hello"},
		{"hello\r", "hello"},
		{"   ", ""},
		{"hello", "hello"},
		{" leading stays", " leading stays"},
	} {
		raw := []byte(`{"comments":[{"text":` + quoteJSON(tc.in) + `}]}`)
		f, err := UnmarshalFile(raw)
		if err != nil {
			t.Fatalf("%q refused: %v", tc.in, err)
		}
		if len(f.Comments) != 1 || f.Comments[0].Text != tc.want {
			t.Errorf("%q came in as %q, want %q", tc.in, f.Comments[0].Text, tc.want)
		}
	}
}

func quoteJSON(s string) string {
	var b []byte
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\t':
			b = append(b, '\\', 't')
		case '\r':
			b = append(b, '\\', 'r')
		default:
			b = append(b, string(r)...)
		}
	}
	return string(append(b, '"'))
}
