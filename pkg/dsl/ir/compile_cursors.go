package ir

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// Cursor diagnostic codes (slot C083–C085, reserved alongside the
// capability codes C080–C082 in validate_capabilities.go).
const (
	DiagUnknownCursor    DiagCode = "C083" // agent/judge references a cursor name not declared at workflow scope
	DiagInvalidCursorVal DiagCode = "C084" // cursor invocation value is invalid (not in enum / out of [0,1] / no matching band)
	DiagMalformedCursor  DiagCode = "C085" // cursor decl is malformed (missing values+bands, bad range, overlapping bands)
	DiagDuplicateCursor  DiagCode = "C086" // duplicate cursor name in workflow
)

// compileCursors converts every top-level `cursor NAME:` declaration
// into a normalized CursorDef. Malformed declarations surface C085 /
// C086 here; invocations are validated separately in
// validateCursorInvocations (so an agent referencing a misnamed
// cursor still gets a precise diagnostic at the call site).
func (c *compiler) compileCursors() map[string]*CursorDef {
	if len(c.file.Cursors) == 0 {
		return nil
	}
	out := make(map[string]*CursorDef, len(c.file.Cursors))
	for _, decl := range c.file.Cursors {
		if _, dup := out[decl.Name]; dup {
			c.errorf(DiagDuplicateCursor,
				"duplicate cursor name %q: cursors must be unique within a file", decl.Name)
			continue
		}
		def, err := compileOneCursor(decl)
		if err != "" {
			c.errorf(DiagMalformedCursor, "cursor %q: %s", decl.Name, err)
			continue
		}
		out[decl.Name] = def
	}
	return out
}

func compileOneCursor(decl *ast.CursorDecl) (*CursorDef, string) {
	def := &CursorDef{Name: decl.Name, Description: decl.Description}
	hasValues := len(decl.Values) > 0
	hasBands := len(decl.Bands) > 0
	if !hasValues && !hasBands {
		return nil, "must declare either 'values:' (enum) or 'bands:' (numeric)"
	}
	if hasValues && hasBands {
		return nil, "cannot declare both 'values:' and 'bands:' — pick one form"
	}
	seenNames := make(map[string]bool, len(decl.Values))
	for _, v := range decl.Values {
		if v.Name == "" || v.Prompt == "" {
			return nil, "value entry has empty name or prompt"
		}
		if seenNames[v.Name] {
			return nil, fmt.Sprintf("duplicate value name %q", v.Name)
		}
		seenNames[v.Name] = true
		def.Values = append(def.Values, CursorValue{Name: v.Name, Prompt: v.Prompt})
	}
	for _, b := range decl.Bands {
		lo, hi, parseErr := parseBandRange(b.Range)
		if parseErr != "" {
			return nil, fmt.Sprintf("band %q: %s", b.Range, parseErr)
		}
		def.Bands = append(def.Bands, CursorBandSpec{Lo: lo, Hi: hi, Prompt: b.Prompt})
	}
	if hasBands {
		if err := checkBandsCoverage(def.Bands); err != "" {
			return nil, err
		}
	}
	return def, ""
}

// parseBandRange parses "lo..hi" into two floats in [0,1] with lo <= hi.
func parseBandRange(s string) (float64, float64, string) {
	parts := strings.SplitN(s, "..", 2)
	if len(parts) != 2 {
		return 0, 0, "range must be \"lo..hi\""
	}
	lo, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, "invalid lower bound"
	}
	hi, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, "invalid upper bound"
	}
	if lo < 0 || hi > 1 {
		return 0, 0, "range must lie in [0.0, 1.0]"
	}
	if lo > hi {
		return 0, 0, "lower bound greater than upper bound"
	}
	return lo, hi, ""
}

// checkBandsCoverage rejects overlapping bands (after the first match
// wins, an overlap silently hides the second prompt — likely an
// authoring mistake). Gaps are allowed: an invocation that falls in a
// gap is flagged at the call site as C084.
func checkBandsCoverage(bands []CursorBandSpec) string {
	for i := range bands {
		for j := i + 1; j < len(bands); j++ {
			if bands[i].Lo <= bands[j].Hi && bands[j].Lo <= bands[i].Hi {
				return fmt.Sprintf("bands overlap: [%g..%g] and [%g..%g]",
					bands[i].Lo, bands[i].Hi, bands[j].Lo, bands[j].Hi)
			}
		}
	}
	return ""
}

// compileCursorInvocation converts an AST cursors block into the IR form.
// Validation of cursor names + values is deferred to validateCursorInvocations
// (it needs the workflow-level CursorDef map, which the compiler doesn't have
// at the moment compileAgents/compileJudges runs).
func compileCursorInvocation(b *ast.CursorBlock) *CursorInvocation {
	if b == nil {
		return nil
	}
	inv := &CursorInvocation{Enabled: b.Enabled}
	for _, s := range b.Settings {
		inv.Settings = append(inv.Settings, CursorSetting{Key: s.Key, Value: s.Value})
	}
	return inv
}
