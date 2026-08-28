package botregistry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// EntryWithSchema augments Entry with the bot workflow's declared
// vars and presets so the studio can render a typed form per bot.
// Vars + Presets carry the AST JSON shape the studio's VarFieldInput
// already consumes — no field renaming, no wire-format translation.
//
// A bot whose source fails to parse keeps Vars/Presets nil. SchemaError
// is non-empty in that case so the UI can surface "schema unavailable"
// without confusing it with "no vars declared".
type EntryWithSchema struct {
	Entry
	Vars        *VarsBlock    `json:"vars,omitempty"`
	Presets     *PresetsBlock `json:"presets,omitempty"`
	SchemaError string        `json:"schema_error,omitempty"`

	// InvocationWarnings flags manifest invocations whose args_var names a
	// var the bot's workflow doesn't declare — a soft authoring mistake the
	// studio surfaces, never a hard error. Empty when the schema couldn't
	// be parsed (vars unknown) so a parse failure isn't reported as a
	// missing-var warning.
	InvocationWarnings []string `json:"invocation_warnings,omitempty"`
}

// VarsBlock matches the AST jsonenc output for a workflow's vars
// declaration (pkg/dsl/ast/jsonenc.go jsonVarsBlock).
type VarsBlock struct {
	Fields []*VarField `json:"fields,omitempty"`
}

// VarField mirrors ast.jsonVarField — same JSON tags so the studio's
// existing VarField TypeScript type accepts our output unchanged.
type VarField struct {
	Name    string   `json:"name,omitempty"`
	Type    string   `json:"type,omitempty"`
	Enum    []string `json:"enum,omitempty"`
	Default *Literal `json:"default,omitempty"`
}

// Literal mirrors ast.jsonLiteral.
type Literal struct {
	Kind     string  `json:"kind,omitempty"`
	Raw      string  `json:"raw,omitempty"`
	StrVal   string  `json:"str_val,omitempty"`
	IntVal   int64   `json:"int_val,omitempty"`
	FloatVal float64 `json:"float_val,omitempty"`
	BoolVal  bool    `json:"bool_val,omitempty"`
}

// PresetsBlock matches the AST jsonenc output for a workflow's presets
// declaration.
type PresetsBlock struct {
	Entries []*Preset `json:"entries,omitempty"`
}

// Preset is a named "sous-bot": a launch-time specialization of a bot. For an
// in-source `presets:` block only Name + Values are set (var overrides). A
// file-based preset (a bundle's presets/<name>.md) additionally carries
// DisplayName / Description / Prompt / Skills — the studio Launch picker shows
// these and previews the prompt bias.
type Preset struct {
	Name        string         `json:"name,omitempty"`
	DisplayName string         `json:"display_name,omitempty"`
	Description string         `json:"description,omitempty"`
	Prompt      string         `json:"prompt,omitempty"`
	Skills      []string       `json:"skills,omitempty"`
	Values      []*PresetValue `json:"values,omitempty"`
}

// PresetValue binds one variable to a literal within a Preset.
type PresetValue struct {
	Key   string   `json:"key,omitempty"`
	Value *Literal `json:"value,omitempty"`
}

// schemaCache memoises the (vars, presets) extracted from a workflow
// source file. Keyed by absolute path. The cached entry is invalidated
// when the file's modtime advances, so an editor save picks up
// instantly without a server restart.
type cachedSchema struct {
	mtime   int64
	size    int64
	vars    *VarsBlock
	presets *PresetsBlock
	err     string
}

var schemaCache sync.Map // map[string]cachedSchema

// ListWithSchema returns one EntryWithSchema per discovered bot. Each
// entry's schema is loaded lazily through LoadSchema, with caching by
// path+mtime — repeated calls are cheap. Parse errors do not abort the
// list: the failing entry surfaces SchemaError and Vars/Presets nil
// so the UI can still show the bot in the picker. Discovery-level
// failures (a bundle whose manifest does not load at all) are skipped
// the same way; they are available through ListWithSchemaDiagnostics.
func ListWithSchema(opts ListOptions) ([]EntryWithSchema, error) {
	entries, _, err := ListWithSchemaDiagnostics(opts)
	return entries, err
}

// ListWithSchemaDiagnostics is ListWithSchema plus the per-entry
// discovery errors from the underlying walk (see ListWithDiagnostics) —
// the channel the studio's bot gallery surfaces so a malformed bundle is
// visible instead of silently absent.
func ListWithSchemaDiagnostics(opts ListOptions) ([]EntryWithSchema, []DiscoveryError, error) {
	entries, diags, err := ListWithDiagnostics(opts)
	if err != nil {
		return nil, nil, err
	}
	out := make([]EntryWithSchema, 0, len(entries))
	for _, e := range entries {
		es := EntryWithSchema{Entry: e}
		vars, presets, schemaErr := LoadSchema(e)
		es.Vars = vars
		es.Presets = presets
		if schemaErr != nil {
			es.SchemaError = schemaErr.Error()
		} else {
			es.InvocationWarnings = invocationVarWarnings(e, vars)
		}
		out = append(out, es)
	}
	return out, diags, nil
}

// invocationVarWarnings flags each invocation whose ArgsVar names a var the
// bot's workflow does not declare — a manifest authoring mistake that would
// silently drop the trigger payload. Soft: surfaced to the studio, never
// fails the list. Only called when the schema parsed cleanly, so a nil vars
// block here means the bot genuinely declares no vars.
func invocationVarWarnings(e Entry, vars *VarsBlock) []string {
	if len(e.Invocations) == 0 {
		return nil
	}
	// Delegate to bundlelint so args_var checking has a single implementation
	// shared with `iterion validate`. Build the minimal manifest + workflow
	// shapes the check needs — the invocations to scan and the declared var
	// names to resolve against — then keep only the args_var findings (the
	// surface the studio has always shown here).
	w := &ir.Workflow{Vars: map[string]*ir.Var{}}
	if vars != nil {
		for _, f := range vars.Fields {
			w.Vars[f.Name] = &ir.Var{Name: f.Name}
		}
	}
	m := &bundle.Manifest{Invocations: e.Invocations}
	var warns []string
	for _, d := range bundlelint.CheckConsistency(bundlelint.Input{Manifest: m, Workflow: w}) {
		if d.Code == bundlelint.DiagArgsVarUnknown {
			warns = append(warns, d.Message)
		}
	}
	return warns
}

// LoadSchema parses the bot's main file, compiles to AST, and returns
// the vars + presets blocks from the first workflow declaration. Uses
// the package-level cache keyed by absolute path + mtime so repeated
// calls within a request batch are O(1).
func LoadSchema(e Entry) (*VarsBlock, *PresetsBlock, error) {
	mainFile := e.MainFile()
	abs, err := filepath.Abs(mainFile)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, nil, err
	}
	if v, ok := schemaCache.Load(abs); ok {
		c := v.(cachedSchema)
		if c.mtime == info.ModTime().UnixNano() && c.size == info.Size() {
			var schemaErr error
			if c.err != "" {
				schemaErr = fmt.Errorf("%s", c.err)
			}
			// File-based presets are merged AFTER the cache: they live in
			// sibling files, so a preset edit must reflect without bumping
			// main.bot's mtime. The per-call reads are a handful per bot.
			return c.vars, mergeFilePresets(e, c.presets), schemaErr
		}
	}
	vars, presets, schemaErr := loadSchemaUncached(abs)
	cached := cachedSchema{
		mtime:   info.ModTime().UnixNano(),
		size:    info.Size(),
		vars:    vars,
		presets: presets, // cache the in-source block only; file presets merge fresh
	}
	if schemaErr != nil {
		cached.err = schemaErr.Error()
	}
	schemaCache.Store(abs, cached)
	return vars, mergeFilePresets(e, presets), schemaErr
}

// mergeFilePresets overlays a bundle's file-based presets (presets/<name>.md)
// onto the in-source presets block so the studio Launch picker shows both. A
// file preset WINS over an in-source entry of the same name (the richer,
// explicit artifact). No-op for loose .bot files (no bundle dir) and bundles
// without a presets/ directory.
func mergeFilePresets(e Entry, inSource *PresetsBlock) *PresetsBlock {
	if !e.IsBundleDir {
		return inSource
	}
	specs, _ := bundle.LoadPresets(filepath.Join(e.Path, bundle.DirPresets))
	if len(specs) == 0 {
		return inSource
	}
	byName := map[string]*Preset{}
	if inSource != nil {
		for _, p := range inSource.Entries {
			byName[p.Name] = p
		}
	}
	for _, ps := range specs {
		byName[ps.Name] = presetSpecToWire(ps) // file preset wins on name collision
	}
	out := &PresetsBlock{Entries: make([]*Preset, 0, len(byName))}
	for _, p := range byName {
		out.Entries = append(out.Entries, p)
	}
	sort.SliceStable(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	return out
}

// presetSpecToWire converts a bundle's on-disk preset into the studio wire
// shape, encoding var values as typed literals identical to the AST encoder
// so the Launch form consumes them without translation.
func presetSpecToWire(ps bundle.PresetSpec) *Preset {
	p := &Preset{
		Name:        ps.Name,
		DisplayName: ps.DisplayName,
		Description: ps.Description,
		Prompt:      ps.Prompt,
		Skills:      ps.Skills,
	}
	for k, v := range ps.Vars {
		p.Values = append(p.Values, &PresetValue{Key: k, Value: presetLiteralFromYAML(v)})
	}
	sort.SliceStable(p.Values, func(i, j int) bool { return p.Values[i].Key < p.Values[j].Key })
	return p
}

// presetLiteralFromYAML maps a YAML-native preset var value to the typed
// literal wire shape (kind + typed field + raw), matching ast jsonenc so the
// studio renders file-preset values exactly like in-source ones.
func presetLiteralFromYAML(v any) *Literal {
	switch t := v.(type) {
	case string:
		return &Literal{Kind: "string", StrVal: t, Raw: t}
	case bool:
		return &Literal{Kind: "bool", BoolVal: t, Raw: strconv.FormatBool(t)}
	case int:
		return &Literal{Kind: "int", IntVal: int64(t), Raw: strconv.FormatInt(int64(t), 10)}
	case int64:
		return &Literal{Kind: "int", IntVal: t, Raw: strconv.FormatInt(t, 10)}
	case float64:
		return &Literal{Kind: "float", FloatVal: t, Raw: strconv.FormatFloat(t, 'g', -1, 64)}
	default:
		s := fmt.Sprintf("%v", v)
		return &Literal{Kind: "string", StrVal: s, Raw: s}
	}
}

func loadSchemaUncached(path string) (*VarsBlock, *PresetsBlock, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	pr := parser.Parse(path, string(src))
	if pr.File == nil {
		return nil, nil, fmt.Errorf("parse failed: file empty")
	}
	// Serialize the AST and pluck out workflow vars + presets. Re-using
	// the AST's existing jsonenc keeps the wire shape identical to what
	// /api/files/open already returns to the studio — VarFieldInput
	// reads the same struct without any translation.
	raw, err := ast.MarshalFile(pr.File)
	if err != nil {
		return nil, nil, err
	}
	var doc struct {
		Vars      json.RawMessage `json:"vars,omitempty"`
		Workflows []struct {
			Vars    json.RawMessage `json:"vars,omitempty"`
			Presets json.RawMessage `json:"presets,omitempty"`
		} `json:"workflows,omitempty"`
		Presets json.RawMessage `json:"presets,omitempty"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, err
	}
	var varsRaw, presetsRaw json.RawMessage
	if len(doc.Workflows) > 0 {
		varsRaw = doc.Workflows[0].Vars
		presetsRaw = doc.Workflows[0].Presets
	}
	if len(varsRaw) == 0 {
		varsRaw = doc.Vars
	}
	if len(presetsRaw) == 0 {
		presetsRaw = doc.Presets
	}
	var vars *VarsBlock
	if len(varsRaw) > 0 {
		vars = &VarsBlock{}
		if err := json.Unmarshal(varsRaw, vars); err != nil {
			return nil, nil, fmt.Errorf("decode vars: %w", err)
		}
		sort.SliceStable(vars.Fields, func(i, j int) bool {
			return vars.Fields[i].Name < vars.Fields[j].Name
		})
	}
	var presets *PresetsBlock
	if len(presetsRaw) > 0 {
		presets = &PresetsBlock{}
		if err := json.Unmarshal(presetsRaw, presets); err != nil {
			return nil, nil, fmt.Errorf("decode presets: %w", err)
		}
	}
	return vars, presets, nil
}

// ClearSchemaCache drops every cached entry. Intended for tests that
// rewrite the same temp file across cases.
func ClearSchemaCache() {
	schemaCache.Range(func(k, _ any) bool {
		schemaCache.Delete(k)
		return true
	})
}

// PurgeSchemaCacheUnder drops the cached entries whose path sits under dir.
// For a caller that materializes bundles into an EPHEMERAL directory (a
// fresh path per call), the path-keyed entries can never be hit again —
// without the purge each pass leaks them into the process-global cache
// for the replica's whole lifetime.
func PurgeSchemaCacheUnder(dir string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	prefix := abs + string(filepath.Separator)
	schemaCache.Range(func(k, _ any) bool {
		if p, ok := k.(string); ok && strings.HasPrefix(p, prefix) {
			schemaCache.Delete(k)
		}
		return true
	})
}
