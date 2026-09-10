// Package gen turns a vendor API description into a connector package.
//
// It accepts BOTH OpenAPI 3.x and Swagger 2.0, natively. That is not a
// convenience: of the four permissively-licensed specs the pilot surveyed,
// two (Slack, Forgejo) are Swagger 2.0, so a generator that took only 3.x
// would need an out-of-band Node converter — in a project that has just
// refused Node at execution time, and for a lane (generation on the
// operator's own instance, for a spec iterion may not redistribute) where an
// extra toolchain is not available at all.
//
// What it does NOT do is decide. The generator derives everything mechanical
// — paths, parameters, result shapes, error statuses, effects, schemas — and
// leaves every judgement to the overlay: which operations an agent should
// see, how a collection paginates, which id must stay stable across a vendor
// rename. A machine can read that a `page` query parameter exists; it cannot
// read that the response is a page OF something.
package gen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// Options drives one generation.
type Options struct {
	// ConnectorID is the package slug and the first segment of every derived
	// operation id. Required — it is a naming decision, not something to
	// derive from a vendor's title.
	ConnectorID string
	// Version is the PACKAGE version to stamp (semver). Defaults to "0.1.0".
	Version string
	// SpecURL and SpecLicense are recorded verbatim in the provenance. The
	// licence is passed IN rather than read out of the spec because
	// `info.license` is frequently absent, and a missing field must not read
	// as permissive.
	SpecURL     string
	SpecLicense string
	// Redistributable asserts that the generated ops may ship inside
	// iterion's own catalog. It defaults to FALSE: a package generated from
	// an unexamined spec must not enter a redistributable catalog just
	// because nobody said otherwise.
	Redistributable bool
	// GeneratedBy names the iterion build doing the generation.
	GeneratedBy string
	// Maturity is the package floor to stamp. Defaults to experimental — a
	// freshly generated package is runnable by explicit choice, never
	// qualified, because nothing has been tested yet.
	Maturity spec.Maturity
	// OperatorSuppliedBaseURL marks a self-hostable product, so a connection
	// asks for an instance URL instead of assuming the vendor's SaaS origin.
	OperatorSuppliedBaseURL bool
	// Now is injectable so a generated package is byte-reproducible in tests.
	Now func() time.Time
}

func (o *Options) defaults() {
	if o.Version == "" {
		o.Version = "0.1.0"
	}
	if o.Maturity == "" {
		o.Maturity = spec.MaturityExperimental
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Skip records one operation the generator could not derive, and why.
type Skip struct {
	// Path and Method locate it in the description; SourceOperationID is the
	// vendor's own id when it declared one. Together they are what a reader
	// needs to find the offending block in a multi-megabyte document.
	Path              string
	Method            string
	SourceOperationID string
	Reason            string
}

// Report is what a generation produced BESIDES the package.
//
// It exists because a real vendor description of any size contains a few
// malformed operations, and the two obvious reactions are both wrong: failing
// the whole generation loses 1200 good operations to one bad one — the same
// shape as the broken manifest that failed every launch of a team for 2h22
// (ADR-080's amendment) — while skipping in silence produces a catalog with
// holes nobody can see. So the generation proceeds and hands back the list.
//
// Measured: GitLab's auto-generated OpenAPI declares a path parameter
// `issue_id` on a path templated `{epic_issue_id}`.
type Report struct {
	Skipped []Skip
}

// Format is a recognised description format.
type Format string

const (
	FormatOpenAPI3 Format = "openapi3"
	FormatSwagger2 Format = "swagger2"
)

// Generate parses a vendor description and produces a connector package.
//
// The result is checked with spec.Package.ValidateGenerated, not the complete
// Validate: a generated package is one HALF of a connector, and the pieces
// only an overlay can supply — an auth scheme a vendor documents in prose, a
// pagination style, a curated MCP set — are legitimately missing here. So a
// generation that produced an incoherent operation fails right away, while
// one that merely awaits its overlay succeeds and stays unusable until the
// overlay lands.
func Generate(data []byte, opts Options) (*spec.Package, *Report, error) {
	opts.defaults()
	if strings.TrimSpace(opts.ConnectorID) == "" {
		return nil, nil, fmt.Errorf("gen: connector id is required")
	}
	doc, err := decode(data)
	if err != nil {
		return nil, nil, err
	}
	format, err := detectFormat(doc)
	if err != nil {
		return nil, nil, err
	}

	w := &walker{doc: doc, format: format, opts: opts, schemas: map[string]spec.Schema{}}
	if err := w.run(); err != nil {
		return nil, nil, err
	}

	pkg := &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion,
			ID:            opts.ConnectorID,
			DisplayName:   str(mapAt(doc, "info"), "title"),
			Description:   firstLine(str(mapAt(doc, "info"), "description")),
			Version:       opts.Version,
			Provenance: spec.Provenance{
				SpecURL:         opts.SpecURL,
				SpecFormat:      string(format),
				SpecVersion:     str(mapAt(doc, "info"), "version"),
				SpecLicense:     opts.SpecLicense,
				Redistributable: opts.Redistributable,
				GeneratedAt:     opts.Now().UTC().Format(time.RFC3339),
				GeneratedBy:     opts.GeneratedBy,
			},
			BaseURL:         w.baseURL,
			Auth:            w.auth,
			Maturity:        opts.Maturity,
			DefaultSecurity: w.defaultSecurity,
		},
		Ops:     w.opsFiles(),
		Schemas: w.schemas,
	}
	if err := pkg.ValidateGenerated(); err != nil {
		// A failure HERE is a generator bug, not bad vendor data: every
		// operation was already validated on its own during the walk, so
		// what is left is a package-level contradiction the walk built.
		return nil, nil, fmt.Errorf("gen: generated package is invalid: %w", err)
	}
	return pkg, &Report{Skipped: w.skipped}, nil
}

// decode reads a description that may be JSON or YAML into a generic tree.
//
// The generic tree is deliberate. The two formats differ in a handful of
// specific places (request bodies, responses, security, servers) and agree
// everywhere else; two full typed parsers would duplicate the agreement and
// hide the differences, where one walk over a tree puts every difference at
// the exact line where it matters.
func decode(data []byte) (map[string]any, error) {
	trimmed := strings.TrimLeft(string(data), " \t\r\n")
	if strings.HasPrefix(trimmed, "{") {
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("gen: parse JSON description: %w", err)
		}
		return out, nil
	}
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("gen: parse YAML description: %w", err)
	}
	norm, ok := normalizeYAML(raw).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("gen: description is not a mapping")
	}
	return norm, nil
}

// normalizeYAML converts yaml.v2's map[interface{}]interface{} into
// map[string]any so one walk serves both input syntaxes. A non-string key is
// dropped: no API description has one, and carrying it would force every
// reader downstream to handle a case that cannot occur.
func normalizeYAML(v any) any {
	switch t := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			ks, ok := k.(string)
			if !ok {
				continue
			}
			out[ks] = normalizeYAML(val)
		}
		return out
	case []any:
		for i := range t {
			t[i] = normalizeYAML(t[i])
		}
		return t
	default:
		return v
	}
}

// detectFormat reads the version marker each format carries at the root.
func detectFormat(doc map[string]any) (Format, error) {
	if v, ok := doc["openapi"].(string); ok {
		if strings.HasPrefix(v, "3.") {
			return FormatOpenAPI3, nil
		}
		return "", fmt.Errorf("gen: unsupported openapi version %q (want 3.x)", v)
	}
	if v, ok := doc["swagger"].(string); ok {
		if strings.HasPrefix(v, "2.") {
			return FormatSwagger2, nil
		}
		return "", fmt.Errorf("gen: unsupported swagger version %q (want 2.0)", v)
	}
	return "", fmt.Errorf("gen: not an API description (no `openapi` or `swagger` version at the root)")
}

// --- small generic-tree helpers -------------------------------------------

func mapAt(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	out, _ := m[key].(map[string]any)
	return out
}

func sliceAt(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	out, _ := m[key].([]any)
	return out
}

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	out, _ := m[key].(string)
	return out
}

func boolAt(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	out, _ := m[key].(bool)
	return out
}

// strSlice reads a list of strings, tolerating the non-string entries a
// hand-edited description occasionally carries.
func strSlice(m map[string]any, key string) []string {
	var out []string
	for _, v := range sliceAt(m, key) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// enumStrings renders an enum whose members may be numbers or booleans, since
// the values become a validation list and a form's options.
func enumStrings(m map[string]any) []string {
	var out []string
	for _, v := range sliceAt(m, "enum") {
		switch t := v.(type) {
		case string:
			out = append(out, t)
		case float64:
			out = append(out, trimFloat(t))
		case bool:
			out = append(out, fmt.Sprint(t))
		}
	}
	return out
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

// firstLine keeps a description's first line, so a vendor's multi-page prose
// does not become the package's one-line summary.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// sortedKeys makes every walk deterministic, so regenerating an unchanged
// spec produces a byte-identical package and a diff shows only real change.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
