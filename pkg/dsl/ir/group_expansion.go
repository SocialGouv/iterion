package ir

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// Every instance owns its entire declaration tree. Walking all string fields
// keeps newly added scalar, slice and nested-block properties parameterizable;
// source spans are metadata and must remain attached to the original source.
type groupExpansion struct {
	c           *compiler
	internal    map[string]bool
	prefix      string
	binds       map[string]string
	prompts     map[string]*ast.PromptDecl
	specialized map[string]string
}

func newGroupExpansion(c *compiler, internal map[string]bool, prefix string, binds map[string]string) *groupExpansion {
	x := &groupExpansion{c: c, internal: internal, prefix: prefix, binds: binds, prompts: map[string]*ast.PromptDecl{}, specialized: map[string]string{}}
	for _, p := range c.file.Prompts {
		x.prompts[p.Name] = p
	}
	return x
}

func (x *groupExpansion) clone(v reflect.Value, owner, field string) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(x.clone(v.Elem(), owner, field))
		return out
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[ast.Span]() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			out.Field(i).Set(x.clone(v.Field(i), v.Type().Name(), v.Type().Field(i).Name))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(x.clone(v.Index(i), owner, field))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(x.clone(iter.Key(), owner, field), x.clone(iter.Value(), owner, field))
		}
		return out
	case reflect.String:
		s := v.String()
		if (owner == "ComputeExpr" || owner == "WhenClause") && field == "Expr" || owner == "FallbackDecl" && field == "When" {
			s = x.expression(s)
		} else {
			s = x.templates(s)
		}
		// Rewrite authored references before substitution: bound values are caller
		// text, inserted once, and must never be captured as group-local references.
		s = substParams(s, x.binds)
		if (owner == "LLMDecl" || owner == "RouterDecl" || owner == "HumanDecl") && (field == "System" || field == "User" || field == "Instructions" || field == "InteractionPrompt") {
			s = x.prompt(s)
		}
		out := reflect.New(v.Type()).Elem()
		out.SetString(s)
		return out
	default:
		return v
	}
}

func (x *groupExpansion) local(path []string) bool {
	// Dotted member IDs use the longest prefix at runtime. Any matching member
	// is sufficient here: the entire output path gets the instance prefix.
	for i := len(path); i > 0; i-- {
		if x.internal[strings.Join(path[:i], ".")] {
			return true
		}
	}
	return false
}

func (x *groupExpansion) templates(s string) string {
	var out strings.Builder
	for {
		start := strings.Index(s, "{{")
		if start < 0 {
			out.WriteString(s)
			break
		}
		end := strings.Index(s[start+2:], "}}")
		if end < 0 {
			out.WriteString(s)
			break
		}
		end += start + 2
		out.WriteString(s[:start+2])
		body := s[start+2 : end]
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), "!"))
		if path, ok := strings.CutPrefix(trimmed, "outputs."); ok && x.local(strings.Split(path, ".")) {
			offset := strings.Index(body, "outputs.") + len("outputs.")
			body = body[:offset] + x.prefix + "." + body[offset:]
		}
		out.WriteString(body)
		out.WriteString("}}")
		s = s[end+2:]
	}
	return out.String()
}

// expression scans authored path tokens without needing to parse unexpanded
// parameter syntax (a parameter may supply an operator or a function name).
// Quoted strings and parameter markers are opaque. "outputs" is a reserved
// namespace: the expression parser rejects it as a lambda parameter.
func (x *groupExpansion) expression(s string) string {
	var out strings.Builder
	copied := 0
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "{{") {
			end := strings.Index(s[i+2:], "}}")
			if end < 0 {
				break
			}
			i += end + 4
			continue
		}
		if s[i] == '\'' || s[i] == '"' {
			quote := s[i]
			i++
			for i < len(s) {
				ch := s[i]
				i++
				if ch == '\\' && i < len(s) {
					i++
					continue
				}
				if ch == quote {
					break
				}
			}
			continue
		}
		if !groupExprIdentStart(s[i]) {
			i++
			continue
		}
		start := i
		for i < len(s) && groupExprIdentCont(s[i]) {
			i++
		}
		namespaceEnd := i
		var path []string
		for {
			dot := i
			for dot < len(s) && groupExprSpace(s[dot]) {
				dot++
			}
			if dot >= len(s) || s[dot] != '.' {
				break
			}
			member := dot + 1
			for member < len(s) && groupExprSpace(s[member]) {
				member++
			}
			if member >= len(s) || !groupExprIdentStart(s[member]) {
				break
			}
			end := member + 1
			for end < len(s) && groupExprIdentCont(s[end]) {
				end++
			}
			path = append(path, s[member:end])
			i = end
		}
		if s[start:namespaceEnd] == "outputs" && x.local(path) {
			out.WriteString(s[copied:namespaceEnd])
			out.WriteString("." + x.prefix)
			copied = namespaceEnd
		}
	}
	out.WriteString(s[copied:])
	return out.String()
}

func groupExprIdentStart(ch byte) bool {
	return ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z'
}
func groupExprIdentCont(ch byte) bool {
	return groupExprIdentStart(ch) || ch >= '0' && ch <= '9'
}
func groupExprSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'
}

func (x *groupExpansion) prompt(ref string) string {
	if name, ok := x.specialized[ref]; ok {
		return name
	}
	p := x.prompts[ref]
	if p == nil {
		return ref
	} // regular unknown-prompt diagnostic
	body := substParams(x.templates(p.Body), x.binds)
	if body == p.Body {
		return ref
	}
	cp := *p
	cp.Body = body
	base := "_group_" + x.prefix + "_" + ref
	cp.Name = base
	for i := 1; x.prompts[cp.Name] != nil; i++ {
		cp.Name = fmt.Sprintf("%s_%d", base, i)
	}
	x.prompts[cp.Name] = &cp
	x.specialized[ref] = cp.Name
	x.c.groupPromptTemplates[ref] = true
	x.c.file.Prompts = append(x.c.file.Prompts, &cp)
	return cp.Name
}

// Prompts used exclusively as group templates do not reach the IR with their
// unbound parameters. A concrete consumer still needs the original declaration
// and its normal diagnostics; duplicate declarations are checked before skipping.
func (c *compiler) retainConcreteGroupPrompts() {
	retain := func(names ...string) {
		for _, name := range names {
			delete(c.groupPromptTemplates, name)
		}
	}
	for _, n := range c.file.Agents {
		retain(n.System, n.User, n.InteractionPrompt)
	}
	for _, n := range c.file.Judges {
		retain(n.System, n.User, n.InteractionPrompt)
	}
	for _, n := range c.file.Routers {
		retain(n.System, n.User)
	}
	for _, n := range c.file.Humans {
		retain(n.System, n.Instructions, n.InteractionPrompt)
	}
	for _, n := range c.file.Supervisors {
		retain(n.System)
	}
}
