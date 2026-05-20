package botregistry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
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

// Preset is a named bundle of variable values.
type Preset struct {
	Name   string         `json:"name,omitempty"`
	Values []*PresetValue `json:"values,omitempty"`
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
// so the UI can still show the bot in the picker.
func ListWithSchema(opts ListOptions) ([]EntryWithSchema, error) {
	entries, err := List(opts)
	if err != nil {
		return nil, err
	}
	out := make([]EntryWithSchema, 0, len(entries))
	for _, e := range entries {
		es := EntryWithSchema{Entry: e}
		vars, presets, schemaErr := LoadSchema(e)
		es.Vars = vars
		es.Presets = presets
		if schemaErr != nil {
			es.SchemaError = schemaErr.Error()
		}
		out = append(out, es)
	}
	return out, nil
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
			if c.err != "" {
				return nil, nil, fmt.Errorf("%s", c.err)
			}
			return c.vars, c.presets, nil
		}
	}
	vars, presets, schemaErr := loadSchemaUncached(abs)
	cached := cachedSchema{
		mtime:   info.ModTime().UnixNano(),
		size:    info.Size(),
		vars:    vars,
		presets: presets,
	}
	if schemaErr != nil {
		cached.err = schemaErr.Error()
	}
	schemaCache.Store(abs, cached)
	return vars, presets, schemaErr
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
