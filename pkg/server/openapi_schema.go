package server

import (
	"reflect"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/pat"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// This file turns the v1 route inventory (openapi.go) into a typed spec: a
// reflection-based Go-type → JSON Schema generator plus a route→types registry.
// The registry references the EXACT structs the handlers decode/encode, so the
// schemas are a projection of the real types — single source of truth, zero
// duplication. Enriching a route = one line in routeSchemas().

// routeOp names the request/response Go types for a single route, by zero
// value. nil means "no body" (e.g. a GET with no request, or a 204).
type routeOp struct {
	request  any
	response any
}

// routeSchemas maps "METHOD /pattern" (matching the recorded route key) to the
// Go types of its request/response bodies. Seeded with the high-value,
// CLI-driven surface; extend it route-by-route as schemas are needed.
func routeSchemas() map[string]routeOp {
	return map[string]routeOp{
		// Auth + identity.
		"POST /api/auth/login": {request: loginReq{}, response: authResponse{}},
		"GET /api/auth/me":     {response: authResponse{}},

		// Personal access tokens (the CLI mints/lists these).
		"POST /api/me/tokens": {
			request: createPATReq{},
			response: struct {
				PAT   pat.Token `json:"pat"`
				Token string    `json:"token"`
			}{},
		},
		"GET /api/me/tokens": {
			response: struct {
				Tokens []pat.Token `json:"tokens"`
			}{},
		},

		// Org administration (super-admin).
		"GET /api/admin/orgs": {
			response: struct {
				Orgs []orgView `json:"orgs"`
			}{},
		},
		"POST /api/admin/orgs":       {request: createOrgReq{}, response: orgView{}},
		"GET /api/admin/orgs/{id}":   {response: orgView{}},
		"PATCH /api/admin/orgs/{id}": {request: updateOrgReq{}, response: orgView{}},

		// Forge integrations (connections + self-service OAuth/GitHub apps).
		"GET /api/teams/{id}/forge/connections": {
			response: struct {
				Connections []forge.Connection `json:"connections"`
			}{},
		},
		"POST /api/teams/{id}/forge/connections": {request: forgeConnectReq{}, response: forgeConnectResp{}},
		"GET /api/teams/{id}/forge/connections/{conn_id}/repos": {
			response: struct {
				Repos []forge.RepoSummary `json:"repos"`
			}{},
		},
		"GET /api/teams/{id}/forge/oauth-apps": {
			response: struct {
				Apps []forge.ForgeOAuthApp `json:"apps"`
			}{},
		},
		"DELETE /api/admin/orgs/{id}":       {response: orgView{}},
		"POST /api/admin/orgs/{id}/restore": {response: orgView{}},
		"POST /api/admin/orgs/{id}/status":  {request: setOrgStatusReq{}, response: orgView{}},

		// Run console — the studio's run list / snapshot / child-subtree /
		// IR-overlay reads. Typed so the generated client sees the subbot
		// projection (WireNode.source/isolated, parent_node_id — C2/C3).
		"GET /api/runs": {
			response: struct {
				Runs []runview.RunSummary `json:"runs"`
			}{},
		},
		"GET /api/runs/{id}": {response: runview.RunSnapshot{}},
		"GET /api/runs/{id}/children": {
			response: struct {
				Runs []runview.RunSummary `json:"runs"`
			}{},
		},
		"GET /api/runs/{id}/workflow": {response: runview.WireWorkflow{}},

		// Global pipeline board — a single execution projection of every
		// root pipeline (ADR-074). Additive to the native backlog (/board).
		"GET /api/v1/pipeline-board": {response: PipelineBoardResponse{}},
		"POST /api/v1/pipeline-board/tasks": {
			request:  pipelineBoardTaskRequest{},
			response: native.Issue{},
		},
		"POST /api/v1/pipeline-board/tasks/{id}/ready": {
			request:  pipelineBoardReadyRequest{},
			response: native.Issue{},
		},
		"PATCH /api/v1/pipeline-board/tasks/{id}": {
			request:  pipelineBoardUpdateRequest{},
			response: native.Issue{},
		},
		"GET /api/v1/pipeline-board/tasks/{id}/dependency-graph": {response: DependencyGraphResponse{}},
		"GET /api/v1/native/issues/{id}/dependency-graph":        {response: DependencyGraphResponse{}},
		"POST /api/v1/pipeline-board/bulk/ready": {
			request:  pipelineBulkReadyRequest{},
			response: pipelineBulkReadyResponse{},
		},
		"POST /api/v1/pipeline-board/bulk/delete": {
			request:  pipelineBulkDeleteRequest{},
			response: pipelineBulkDeleteResponse{},
		},
		"POST /api/v1/pipeline-board/bulk/recompute-deps": {
			request:  pipelineRecomputeDepsRequest{},
			response: pipelineRecomputeDepsResponse{},
		},

		"POST /api/teams/{id}/forge/oauth-apps": {request: forgeOAuthAppReq{}, response: forge.ForgeOAuthApp{}},
		"POST /api/teams/{id}/forge/oauth-apps/github-manifest": {
			request: forgeOAuthAppReq{},
			response: struct {
				PostURL  string `json:"post_url"`
				Manifest any    `json:"manifest"`
				State    string `json:"state"`
			}{},
		},
	}
}

// schemaGen builds OpenAPI 3.1 schemas, collecting named struct schemas into a
// reusable components map and referencing them with $ref so generators emit
// named types. Anonymous structs are inlined.
type schemaGen struct {
	components map[string]any          // schema name → JSON Schema
	names      map[reflect.Type]string // type → assigned component name (dedupe + cycle guard)
}

func newSchemaGen() *schemaGen {
	return &schemaGen{components: map[string]any{}, names: map[reflect.Type]string{}}
}

var timeType = reflect.TypeOf(time.Time{})

// schema returns the JSON Schema for a value (passed as a zero value). For named
// structs it registers a component and returns a $ref.
func (g *schemaGen) schema(v any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return g.schemaForType(reflect.TypeOf(v))
}

func (g *schemaGen) schemaForType(t reflect.Type) map[string]any {
	switch t.Kind() {
	case reflect.Pointer:
		// Unwrap; a pointer field is simply optional/nullable.
		return g.schemaForType(t.Elem())
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 { // []byte → base64 string
			return map[string]any{"type": "string", "format": "byte"}
		}
		return map[string]any{"type": "array", "items": g.schemaForType(t.Elem())}
	case reflect.Map:
		return map[string]any{
			"type":                 "object",
			"additionalProperties": g.schemaForType(t.Elem()),
		}
	case reflect.Struct:
		if t == timeType {
			return map[string]any{"type": "string", "format": "date-time"}
		}
		if t.Name() == "" { // anonymous struct → inline
			return g.objectSchema(t)
		}
		return g.refForStruct(t)
	case reflect.Interface:
		return map[string]any{} // any
	default:
		return map[string]any{}
	}
}

// refForStruct registers (once) a named struct as a component and returns a $ref
// to it. The placeholder registered before recursion makes recursive types safe.
func (g *schemaGen) refForStruct(t reflect.Type) map[string]any {
	name, ok := g.names[t]
	if !ok {
		name = g.uniqueName(t)
		g.names[t] = name
		g.components[name] = map[string]any{} // placeholder (cycle guard)
		g.components[name] = g.objectSchema(t)
	}
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

// uniqueName assigns a component name, disambiguating same-named types from
// different packages with a package-suffix.
func (g *schemaGen) uniqueName(t reflect.Type) string {
	base := t.Name()
	taken := false
	for _, n := range g.names {
		if n == base {
			taken = true
			break
		}
	}
	if !taken {
		return base
	}
	pkg := t.PkgPath()
	if i := strings.LastIndexByte(pkg, '/'); i >= 0 {
		pkg = pkg[i+1:]
	}
	if pkg != "" {
		pkg = strings.ToUpper(pkg[:1]) + pkg[1:]
	}
	return pkg + base
}

// objectSchema reflects a struct's exported fields into an object schema,
// honoring json tags (name, omitempty, "-") and flattening embedded structs.
func (g *schemaGen) objectSchema(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string
	g.collectFields(t, props, &required)
	obj := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		obj["required"] = required
	}
	return obj
}

func (g *schemaGen) collectFields(t reflect.Type, props map[string]any, required *[]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		tag := f.Tag.Get("json")
		name, opts := parseJSONTag(tag)
		if name == "-" {
			continue
		}
		// Embedded struct without a json name → flatten its fields (Go's
		// encoding/json promotion).
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && ft != timeType {
				g.collectFields(ft, props, required)
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		props[name] = g.schemaForType(f.Type)
		// Required = no omitempty and not a pointer (pointers are optional).
		if !strings.Contains(opts, "omitempty") && f.Type.Kind() != reflect.Pointer {
			*required = append(*required, name)
		}
	}
}

func parseJSONTag(tag string) (name, opts string) {
	if tag == "" {
		return "", ""
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		return tag[:i], tag[i+1:]
	}
	return tag, ""
}
