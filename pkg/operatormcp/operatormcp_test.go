package operatormcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// isolateHome points HOME at an empty dir so tests never read the
// developer's real ~/.iterion/cli-auth.json, and clears the env-only
// remote config path.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_REMOTE_URL", "")
	t.Setenv("ITERION_REMOTE_TOKEN", "")
	t.Setenv("ITERION_TOKEN", "")
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{StoreDir: t.TempDir(), WorkDir: t.TempDir()}
}

func TestToolsRegistry(t *testing.T) {
	s := newTestServer(t)
	tools := s.Tools()
	if len(tools) == 0 {
		t.Fatal("no tools registered")
	}
	seen := map[string]bool{}
	var hasLocal, hasBoard, hasRemote bool
	for i, tool := range tools {
		if tool.Name == "" || tool.Description == "" || tool.handler == nil {
			t.Fatalf("tool %d is incomplete: %+v", i, tool)
		}
		if seen[tool.Name] {
			t.Fatalf("duplicate tool name %q", tool.Name)
		}
		seen[tool.Name] = true
		if !json.Valid(tool.InputSchema) {
			t.Fatalf("tool %s has invalid JSON schema", tool.Name)
		}
		if i > 0 && tools[i-1].Name >= tool.Name {
			t.Fatalf("tools not sorted: %s >= %s", tools[i-1].Name, tool.Name)
		}
		switch {
		case strings.HasPrefix(tool.Name, localBoardPrefix):
			hasBoard = true
		case strings.HasPrefix(tool.Name, "local_"):
			hasLocal = true
		case strings.HasPrefix(tool.Name, "remote_"):
			hasRemote = true
		default:
			t.Fatalf("tool %s has neither local_ nor remote_ prefix", tool.Name)
		}
	}
	if !hasLocal || !hasBoard || !hasRemote {
		t.Fatalf("missing a family: local=%v board=%v remote=%v", hasLocal, hasBoard, hasRemote)
	}
}

func TestToolsReadOnlyFiltering(t *testing.T) {
	s := &Server{StoreDir: t.TempDir(), WorkDir: t.TempDir(), ReadOnly: true}
	names := map[string]bool{}
	for _, tool := range s.Tools() {
		if !tool.ReadOnly && !tool.ListedInReadOnly {
			t.Fatalf("mutating tool %s exposed in read-only mode", tool.Name)
		}
		names[tool.Name] = true
	}
	for _, gone := range []string{"local_run", "local_resume", "local_run_cancel", "local_answer", "local_board_create_issue", "remote_runs_launch", "remote_issue_create"} {
		if names[gone] {
			t.Fatalf("tool %s should be hidden in read-only mode", gone)
		}
	}
	// The escape hatch stays listed (its handler enforces GET-only).
	for _, kept := range []string{"remote_api", "local_runs_list", "local_board_list_issues", "remote_status"} {
		if !names[kept] {
			t.Fatalf("tool %s should stay listed in read-only mode", kept)
		}
	}
}

func TestToolsFamilyFiltering(t *testing.T) {
	local := &Server{StoreDir: t.TempDir(), WorkDir: t.TempDir(), Only: FamilyLocal}
	for _, tool := range local.Tools() {
		if strings.HasPrefix(tool.Name, "remote_") {
			t.Fatalf("remote tool %s exposed with --only local", tool.Name)
		}
	}
	remote := &Server{StoreDir: t.TempDir(), WorkDir: t.TempDir(), Only: FamilyRemote}
	for _, tool := range remote.Tools() {
		if strings.HasPrefix(tool.Name, "local_") {
			t.Fatalf("local tool %s exposed with --only remote", tool.Name)
		}
	}
	if len(remote.Tools()) == 0 || len(local.Tools()) == 0 {
		t.Fatal("family filtering emptied the registry")
	}
}

func TestParseFamily(t *testing.T) {
	for in, want := range map[string]Family{"": FamilyAll, "local": FamilyLocal, "REMOTE": FamilyRemote} {
		got, err := ParseFamily(in)
		if err != nil || got != want {
			t.Fatalf("ParseFamily(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseFamily("bogus"); err == nil {
		t.Fatal("ParseFamily(bogus) should error")
	}
}

func TestCallUnknownTool(t *testing.T) {
	s := newTestServer(t)
	_, err := s.Call(context.Background(), "nope", nil)
	var unknown *ErrUnknownTool
	if !errors.As(err, &unknown) {
		t.Fatalf("want ErrUnknownTool, got %v", err)
	}

	// A mutating tool hidden by read-only mode is unknown too — the
	// gate is structural, not advisory.
	ro := &Server{StoreDir: t.TempDir(), WorkDir: t.TempDir(), ReadOnly: true}
	if _, err := ro.Call(context.Background(), "local_run", nil); !errors.As(err, &unknown) {
		t.Fatalf("read-only Call(local_run) should be unknown, got %v", err)
	}
}

func TestReadOnlyAnnotationIsTruthful(t *testing.T) {
	s := newTestServer(t)
	for _, tool := range s.Tools() {
		if tool.Name == "remote_api" {
			if tool.ReadOnly {
				t.Fatal("remote_api can mutate — its ReadOnly flag (the readOnlyHint source) must be false")
			}
			if !tool.ListedInReadOnly {
				t.Fatal("remote_api must stay listed in read-only mode (handler enforces GET-only)")
			}
			return
		}
	}
	t.Fatal("remote_api not found")
}

func TestReadOnlyModeCreatesNothingOnDisk(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "absent-store")
	s := &Server{StoreDir: storeDir, WorkDir: dir, ReadOnly: true}

	for _, name := range []string{"local_runs_list", "local_board_list_issues"} {
		res, err := s.Call(context.Background(), name, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Call(%s): %v", name, err)
		}
		if !res.IsError {
			t.Fatalf("%s on an absent store should error in read-only mode: %+v", name, res)
		}
		if !strings.Contains(res.Content[0].Text, "read-only mode") {
			t.Fatalf("%s should explain the read-only refusal: %s", name, res.Content[0].Text)
		}
	}
	if _, err := os.Stat(storeDir); !os.IsNotExist(err) {
		t.Fatalf("read-only mode created the store directory (stat err: %v)", err)
	}
}

func TestCallReportsToolErrorsInBand(t *testing.T) {
	s := newTestServer(t)
	res, err := s.Call(context.Background(), "local_run_get", json.RawMessage(`{"run_id":"missing-run"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want isError=true for a missing run, got %+v", res)
	}
	if len(res.Content) != 1 || res.Content[0].Text == "" {
		t.Fatalf("want one non-empty text block, got %+v", res.Content)
	}
}

// TestUnknownArgumentIsAToolErrorWithAcceptedKeys pins the promise
// that every tool's inputSchema advertises (additionalProperties:
// false): a misspelt or misplaced argument is a tool error, not a
// silent drop (issue #1335).
//
// The class is the whole server — every tool decoded through the
// operator MCP's shared unmarshalArgs choke point. This table
// iterates over Server.Tools() so the next tool cannot regress the
// promise: a newly-added tool that fails to reject an unknown key
// reddens on the first assertion.
//
// Board tools (local_board_*) route through a separate boardops
// decoder and their schemas do not declare additionalProperties:
// false, so they are excluded — a different contract.
//
// Mutation: revert unmarshalArgs to plain json.Unmarshal, and this
// reddens on the isError check (silent drop → err=nil → no tool
// error).
func TestUnknownArgumentIsAToolErrorWithAcceptedKeys(t *testing.T) {
	isolateHome(t)
	s := newTestServer(t)
	for _, tool := range s.Tools() {
		if strings.HasPrefix(tool.Name, "local_board_") {
			// boardops has its own decoder and its schemas do not
			// declare additionalProperties: false.
			continue
		}
		t.Run(tool.Name, func(t *testing.T) {
			// The forbidden alternative in this test: a JSON object
			// carrying an unknown key. Sent alone (no accepted keys)
			// so the strict decoder MUST catch it at decode time,
			// before any missing-required check or business logic
			// can mask the drop.
			raw := json.RawMessage(`{"__unknown_iterion_vras__": "sentinel"}`)
			res, callErr := s.Call(context.Background(), tool.Name, raw)
			if callErr != nil {
				t.Fatalf("Call: %v", callErr)
			}
			if !res.IsError {
				t.Fatalf("unknown key silently dropped by %s: %+v", tool.Name, res)
			}
			body := res.Content[0].Text
			if !strings.Contains(body, "__unknown_iterion_vras__") {
				t.Errorf("%s error must name the unknown key: %q", tool.Name, body)
			}
			if !strings.Contains(body, "additionalProperties") {
				t.Errorf("%s error must cite the schema promise: %q", tool.Name, body)
			}
			// The accepted-keys hint is what makes the error
			// self-correcting for an LLM caller: the ticket
			// specifically asks for it.
			if !strings.Contains(body, "accepted keys:") {
				t.Errorf("%s error must list the accepted keys: %q", tool.Name, body)
			}
		})
	}
}

// TestEverySchemaKeyRoundTripsThroughItsHandler pins the sibling of the
// unknown-key promise, by EXECUTION: every property each tool's
// inputSchema declares must decode through the tool's real handler.
// Under DisallowUnknownFields, a declared property with no matching
// struct field (a rename drift, a typo, an old property left behind)
// hard-fails a documented operator call; a declared type the struct
// does not accept fails the same way. For each tool, `{prop: a sample
// of the declared type}` is sent per declared property and the result
// must carry no decode error — the tool's own business errors (missing
// required, not logged in, invalid path) are fine.
//
// Mutation seen red: drop a struct field or rename its JSON tag — the
// decoder rejects the declared key; change a struct field's type — the
// sample of the declared type no longer decodes.
func TestEverySchemaKeyRoundTripsThroughItsHandler(t *testing.T) {
	isolateHome(t)
	s := newTestServer(t)
	for _, tool := range s.Tools() {
		if strings.HasPrefix(tool.Name, "local_board_") {
			// boardops has its own decoder; schemas don't declare
			// additionalProperties: false. Out of scope.
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s: schema does not parse: %v", tool.Name, err)
		}
		if len(schema.Properties) == 0 {
			continue // no-argument tools (remote_status, remote_bots_list)
		}
		for propName, propSchema := range schema.Properties {
			t.Run(tool.Name+"/"+propName, func(t *testing.T) {
				// Pick a value the type declared by the property
				// accepts. Fall back to a JSON null on unknown
				// types — nulls decode to zero values for every
				// Go type, so the parity test stays valid.
				value := sampleForSchemaType(propSchema)
				payload := map[string]json.RawMessage{propName: value}
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatalf("marshal payload: %v", err)
				}
				res, callErr := s.Call(context.Background(), tool.Name, raw)
				if callErr != nil {
					t.Fatalf("Call: %v", callErr)
				}
				body := ""
				if len(res.Content) > 0 {
					body = res.Content[0].Text
				}
				// The DECODE must not fail on this key. Business
				// errors (missing required, not logged in, path
				// invalid) are acceptable; a decode error — every
				// one is rendered as "invalid arguments: …" by
				// wrapArgsError — is a schema/struct drift.
				if strings.Contains(body, "unknown field") || strings.Contains(body, "invalid arguments:") {
					t.Fatalf("declared schema key %q (sample %s) rejected by %s's decoder — schema/struct drift\n  body: %s",
						propName, value, tool.Name, body)
				}
			})
		}
	}
}

// sampleForSchemaType returns a JSON value the given property schema
// accepts. Object properties honour a declared additionalProperties type —
// the sample carries a value of the kind the handler actually reads — and
// a number-valued object otherwise, so a schema that promises "any value"
// over a handler that reads only strings reddens here instead of shipping
// a documented call that hard-fails. Falls back to `null` on unrecognised
// types (nulls decode into every Go type).
func sampleForSchemaType(propSchema json.RawMessage) json.RawMessage {
	var meta struct {
		Type                 string `json:"type"`
		AdditionalProperties *struct {
			Type string `json:"type"`
		} `json:"additionalProperties"`
	}
	_ = json.Unmarshal(propSchema, &meta)
	switch meta.Type {
	case "string":
		return json.RawMessage(`"x"`)
	case "integer":
		return json.RawMessage(`1`)
	case "boolean":
		return json.RawMessage(`false`)
	case "array":
		return json.RawMessage(`[]`)
	case "object":
		if meta.AdditionalProperties != nil && meta.AdditionalProperties.Type == "string" {
			return json.RawMessage(`{"k":"v"}`)
		}
		return json.RawMessage(`{"k":1}`)
	default:
		return json.RawMessage(`null`)
	}
}

// TestEveryHandlerArgumentMatchesItsSchemaInBothDirections reads, for
// every non-board tool, the argument type its handler decodes into —
// captured at the one chokepoint every tool crosses (Server.unmarshalArgs)
// rather than from a parallel list — and compares its JSON field names
// with the tool's inputSchema.properties, both ways. A declared property
// with no struct field is a documented argument that hard-fails (the
// strict decoder refuses it); a struct field with no declared property is
// an argument the schema hides, which clients and their validators never
// learn. Neither is visible to the unknown-key test. A handler that
// returns without decoding through the chokepoint fails too: that is an
// argument path the strict decoder does not cover.
//
// Mutation seen red: drop a field from any tool's args struct, or a
// property from its schema — the two sets are printed for that tool.
func TestEveryHandlerArgumentMatchesItsSchemaInBothDirections(t *testing.T) {
	isolateHome(t)
	s := newTestServer(t)
	decodedInto := map[string]reflect.Type{}
	s.argsDecodeObserver = func(toolName string, dest any) {
		decodedInto[toolName] = reflect.TypeOf(dest)
	}
	for _, tool := range s.Tools() {
		if strings.HasPrefix(tool.Name, "local_board_") {
			// boardops decodes on its own and declares no
			// additionalProperties: false.
			continue
		}
		delete(decodedInto, tool.Name)
		if _, err := s.Call(context.Background(), tool.Name, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("%s: Call: %v", tool.Name, err)
		}
		dest, ok := decodedInto[tool.Name]
		if !ok {
			t.Errorf("%s: the handler returned without decoding its arguments through Server.unmarshalArgs under its own name", tool.Name)
			continue
		}
		if dest.Kind() != reflect.Pointer {
			t.Fatalf("%s: decode target %s is not a pointer", tool.Name, dest)
		}
		var accepted []string
		switch dest.Elem().Kind() {
		case reflect.Struct:
			accepted = jsonFieldNames(dest.Elem())
		case reflect.Map:
			// The one map decode: remote_issue_update keeps its
			// PATCH semantics open and checks keys against its own
			// allow-list, which is what the schema must match.
			if tool.Name != "remote_issue_update" {
				t.Fatalf("%s: decodes into a map, which DisallowUnknownFields cannot police, without a declared allow-list", tool.Name)
			}
			accepted = append([]string{"id"}, remoteIssueUpdatableFields...)
		default:
			t.Fatalf("%s: unexpected decode target %s", tool.Name, dest)
		}
		sort.Strings(accepted)
		declared := s.acceptedArgKeys(tool.Name)
		if !slices.Equal(accepted, declared) {
			t.Errorf("%s: schema/handler drift\n  schema declares: %v\n  handler accepts: %v", tool.Name, declared, accepted)
		}
	}
}

// jsonFieldNames lists the JSON keys encoding/json decodes into t's
// exported fields: the tag name when tagged, the field name otherwise,
// `json:"-"` skipped, untagged anonymous struct fields flattened.
func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct && f.Tag.Get("json") == "" {
			names = append(names, jsonFieldNames(f.Type)...)
			continue
		}
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		names = append(names, name)
	}
	return names
}

// TestTrailingDataAfterArgumentsIsAnError: arguments are ONE JSON value;
// trailing garbage after it is refused like any other malformed input —
// Decoder.Decode alone would read the first value and silently ignore
// the rest (the same silent-tolerance class the unknown-key check exists
// for).
//
// Mutation seen red: drop the dec.More() check in unmarshalArgs — the
// trailing bytes are ignored and the call proceeds.
func TestTrailingDataAfterArgumentsIsAnError(t *testing.T) {
	isolateHome(t)
	s := newTestServer(t)
	res, err := s.Call(context.Background(), "local_run_get", json.RawMessage(`{"run_id":"x"} trailing garbage`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].Text, "invalid arguments") {
		t.Fatalf("trailing data after the arguments value was not refused: isError=%v body=%s", res.IsError, res.Content[0].Text)
	}
	res, err = s.Call(context.Background(), "local_bots_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatalf("clean arguments refused: %s", res.Content[0].Text)
	}
}
