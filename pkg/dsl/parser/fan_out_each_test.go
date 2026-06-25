package parser_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// fan_out_each router with the full data-driven + DAG surface parses into the
// RouterDecl fields.
func TestRouterDeclFanOutEach(t *testing.T) {
	src := `router dispatch:
  mode: fan_out_each
  over: "{{outputs.plan.tasks}}"
  as: task
  key: id
  depends_on: deps
`
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)

	r := res.File.Routers[0]
	assertEq(t, "Name", r.Name, "dispatch")
	assertEq(t, "Mode", r.Mode, ast.RouterFanOutEach)
	assertEq(t, "Over", r.Over, "{{outputs.plan.tasks}}")
	assertEq(t, "As", r.As, "task")
	assertEq(t, "Key", r.Key, "id")
	assertEq(t, "DependsOn", r.DependsOn, "deps")
}

// fan_out_each without the optional DAG fields still parses (plain data-driven
// fan-out); key/depends_on default to empty.
func TestRouterDeclFanOutEachPlain(t *testing.T) {
	src := `router dispatch:
  mode: fan_out_each
  over: "{{outputs.plan.items}}"
`
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)

	r := res.File.Routers[0]
	assertEq(t, "Mode", r.Mode, ast.RouterFanOutEach)
	assertEq(t, "Over", r.Over, "{{outputs.plan.items}}")
	assertEq(t, "Key", r.Key, "")
	assertEq(t, "DependsOn", r.DependsOn, "")
}
