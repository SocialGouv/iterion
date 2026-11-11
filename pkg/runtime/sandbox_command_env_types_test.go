package runtime

import (
	"fmt"
	"go/types"
	"testing"

	"github.com/SocialGouv/iterion/internal/modtypes"
)

func TestCommandEnvironmentInventoryComposedTypes(t *testing.T) {
	m, err := modtypes.Load(repoRootForInventory(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, decl, body string
		want             int
	}{
		{"nested-result-alias", `type C = ex.Cmd`, `func caller(ctx context.Context,f func(context.Context,[]string,sb.ExecOpts)*C){ _=f(ctx,nil,sb.ExecOpts{}) }`, 1},
		{"nested-slice-alias", `type S = string`, `func caller(ctx context.Context,f func(context.Context,[]S,sb.ExecOpts)*ex.Cmd){ _=f(ctx,nil,sb.ExecOpts{}) }`, 1},
		{"top-context-alias", `type C = context.Context`, `func caller(ctx C,f func(C,[]string,sb.ExecOpts)*ex.Cmd){ _=f(ctx,nil,sb.ExecOpts{}) }`, 1},
		{"named-callback", `type F func(context.Context,[]string,sb.ExecOpts)*ex.Cmd`, `func caller(ctx context.Context,f F){ _=f(ctx,nil,sb.ExecOpts{}) }`, 1},
		{"callback-map-index", `var fs map[string]func(context.Context,[]string,sb.ExecOpts)*ex.Cmd`, `func caller(ctx context.Context){ _=fs["chosen"](ctx,nil,sb.ExecOpts{}) }`, 1},
		{"callback-slice-index", `var fs []func(context.Context,[]string,sb.ExecOpts)*ex.Cmd`, `func caller(ctx context.Context){ _=fs[0](ctx,nil,sb.ExecOpts{}) }`, 1},
		{"callback-returned", `func factory()func(context.Context,[]string,sb.ExecOpts)*ex.Cmd{return nil}`, `func caller(ctx context.Context){ _=factory()(ctx,nil,sb.ExecOpts{}) }`, 1},
		{"named-import", ``, `func caller(ctx context.Context,r sb.Run){ _=r.Command(ctx,nil,sb.ExecOpts{});_ = ex.Cmd{} }`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := fmt.Sprintf("package probe\nimport(\"context\"; ex \"os/exec\"; sb \"github.com/SocialGouv/iterion/pkg/sandbox\")\n%s\n%s", tc.decl, tc.body)
			p, e := m.CheckSource("review/probe", "probe.go", src)
			if e != nil {
				t.Fatal(e)
			}
			got := commandEnvReferences(p)
			t.Logf("counts=%v", got)
			if got["probe.go:caller"] != tc.want {
				t.Errorf("ARRIVAL_MISSED: caller references=%d want=%d", got["probe.go:caller"], tc.want)
			}
		})
	}
}
func TestCommandEnvironmentInventoryOppositeValues(t *testing.T) {
	m, err := modtypes.Load(repoRootForInventory(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, src string
		want      map[string]int
	}{
		{"dot imports", `package probe; import("context"; . "github.com/SocialGouv/iterion/pkg/sandbox");func caller(ctx context.Context,r Run){_=r.Command(ctx,nil,ExecOpts{})}`, map[string]int{"probe.go:caller": 1}},
		{"package variable", `package probe; import("context"; sb "github.com/SocialGouv/iterion/pkg/sandbox");var entry=sb.Run.Command;func caller(ctx context.Context,r sb.Run){_=entry(r,ctx,nil,sb.ExecOpts{})}`, map[string]int{"probe.go:<package>": 1, "probe.go:caller": 1}},
		{"pointer callback", `package probe; import("context"; ex "os/exec"; sb "github.com/SocialGouv/iterion/pkg/sandbox");func caller(ctx context.Context,p *func(context.Context,[]string,sb.ExecOpts)*ex.Cmd){_=(*p)(ctx,nil,sb.ExecOpts{})}`, map[string]int{"probe.go:caller": 1}},
		{"asserted callback", `package probe; import("context"; ex "os/exec"; sb "github.com/SocialGouv/iterion/pkg/sandbox");func caller(ctx context.Context,p any){_=p.(func(context.Context,[]string,sb.ExecOpts)*ex.Cmd)(ctx,nil,sb.ExecOpts{})}`, map[string]int{"probe.go:caller": 1}},
		{"factory definitions are not references", `package probe; import("context"; ex "os/exec"; sb "github.com/SocialGouv/iterion/pkg/sandbox");type F func(context.Context,[]string,sb.ExecOpts)*ex.Cmd;type Data struct{ Command F }; var _ F`, map[string]int{}},
		{"different named options", `package probe; import("context"; ex "os/exec"; sb "github.com/SocialGouv/iterion/pkg/sandbox");type Different sb.ExecOpts;func caller(ctx context.Context,f func(context.Context,[]string,Different)*ex.Cmd){_=f(ctx,nil,Different{})}`, map[string]int{}},
		{"SDK aliases recursive", `package probe; import("context"; ex "os/exec");type S=string;type B=bool;type C=ex.Cmd;func caller(ctx context.Context,f func(context.Context,S,[]S,S,map[S]S,B)*C){_=f(ctx,"",nil,"",nil,false)}`, map[string]int{"probe.go:caller": 1}},
		{"distinct named command", `package probe; import("context"; ex "os/exec"; sb "github.com/SocialGouv/iterion/pkg/sandbox");type C ex.Cmd;func caller(ctx context.Context,f func(context.Context,[]string,sb.ExecOpts)*C){_=f(ctx,nil,sb.ExecOpts{})}`, map[string]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, e := m.CheckSource("review/opposite", "probe.go", tc.src)
			if e != nil {
				t.Fatal(e)
			}
			got := commandEnvReferences(p)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("COUNTS got %v want %v", got, tc.want)
			}
		})
	}
}

func TestCommandEnvironmentInventoryNamedScope(t *testing.T) {
	m, err := modtypes.Load(repoRootForInventory(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, source string
		want               int
	}{
		{"local unrelated ExecOpts", "github.com/SocialGouv/iterion/pkg/sandbox", `package sandbox
import("context";"os/exec")
type ExecOpts struct{ Public bool }
func unrelated(ctx context.Context){
 type ExecOpts struct{ command string }
 f:=func(ctx context.Context,args []string,opts ExecOpts)*exec.Cmd{return exec.CommandContext(ctx,opts.command,args...)}
 _=f(ctx,nil,ExecOpts{command:"true"})
}`, 0},
		{"local alias real ExecOpts", "review/aliases", `package probe
import("context";"os/exec";sb "github.com/SocialGouv/iterion/pkg/sandbox")
func unrelated(ctx context.Context,f func(context.Context,[]string,sb.ExecOpts)*exec.Cmd){
 type ExecOpts = sb.ExecOpts
 _=f(ctx,nil,ExecOpts{})
}`, 1},
		{"ordinary real factory", "review/ordinary", `package probe
import("context";sb "github.com/SocialGouv/iterion/pkg/sandbox")
func unrelated(ctx context.Context,r sb.Run){_=r.Command(ctx,nil,sb.ExecOpts{})}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, e := m.CheckSource(tc.path, "review.go", tc.source)
			if e != nil {
				t.Fatal(e)
			}
			c := commandEnvReferences(p)
			t.Logf("counts=%v", c)
			if c["review.go:unrelated"] != tc.want {
				t.Fatalf("NAMED_SCOPE_COLLISION: got %v, want %d", c, tc.want)
			}
		})
	}
}

func TestCommandEnvironmentInventoryExportScope(t *testing.T) {
	m, err := modtypes.Load(repoRootForInventory(t))
	if err != nil {
		t.Fatal(err)
	}
	pkg := m.Package("github.com/SocialGouv/iterion/pkg/sandbox")
	source := pkg.Types.Scope().Lookup("ExecOpts")
	if source == nil {
		t.Fatal("missing source ExecOpts")
	}
	p, err := m.CheckSource("review/imports", "scope.go", `package probe;import sb "github.com/SocialGouv/iterion/pkg/sandbox";func caller(opts sb.ExecOpts){type Alias=sb.ExecOpts;var local Alias;_=local;_=opts}`)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for id, obj := range p.Info.Defs {
		if obj == nil || (id.Name != "opts" && id.Name != "local") {
			continue
		}
		named, ok := types.Unalias(obj.Type()).(*types.Named)
		if !ok {
			t.Fatalf("not named: %T", obj.Type())
		}
		imported := named.Obj()
		sameParent := imported.Parent() == imported.Pkg().Scope()
		sameLookup := imported.Pkg().Scope().Lookup(imported.Name()) == imported
		t.Logf("%s imported Parent==Pkg.Scope=%v lookup-self=%v source/import pointer identical=%v", id.Name, sameParent, sameLookup, source == imported)
		if !sameParent || !sameLookup {
			t.Fatal("imported package type rejected by scope test")
		}
		seen++
	}
	t.Logf("source Parent==Pkg.Scope=%v lookup-self=%v", source.Parent() == source.Pkg().Scope(), source.Pkg().Scope().Lookup(source.Name()) == source)
	if seen != 2 || source.Parent() != source.Pkg().Scope() {
		t.Fatal("source/local-alias controls missing")
	}
}
