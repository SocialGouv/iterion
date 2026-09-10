// Package overlay is the AUTHORED half of a connector package.
//
// Everything under ops/ is derived from a vendor's description and is
// therefore disposable: delete it, regenerate, get the same bytes. An overlay
// is the opposite — it is iterion's own work, it is small, and it is what a
// human maintains. Keeping the two apart is what makes a vendor's next
// release a regeneration instead of a merge conflict.
//
// # What an overlay has to carry
//
// The list below is not a guess at what might be useful. It is what four real
// descriptions (Forgejo, Slack, GitHub, GitLab) turned out to be unable to
// state, measured while generating them:
//
//   - AUTH, sometimes entirely. GitHub's MIT-licensed OpenAPI declares no
//     security scheme anywhere; authentication lives in its prose. Slack
//     declares OAuth but passes the credential as an ordinary `token`
//     parameter, which must be marked secret or it reaches a model's context.
//     Forgejo's "prepend the word token" is a sentence in a description field.
//   - PAGINATION. A description shows that a `page` parameter exists. It
//     never shows that the response is a page OF something, which is the only
//     part an executor needs.
//   - THE CURATED MCP SET. 1225 tools would bury an agent's context; which
//     eight matter is a product judgement no derivation reaches.
//   - ID PINS. A derived id is stable until the vendor adds an endpoint that
//     derives the same name and sorts earlier, at which point it would take
//     the existing id. Pinning is what makes a `.bot` safe across releases.
//   - OUTCOME. That Slack signals failure as `{"ok": false}` inside a 200 is
//     documented in prose and nowhere machine-readable.
//   - CORRECTIONS a derivation gets wrong in a way only a human sees: a POST
//     that searches, an endpoint that runs a model and is therefore not
//     deterministic, an operation to drop outright.
//
// # Why merging is not just assignment
//
// An overlay names operations by their DERIVED id, so a regeneration that
// moves an id would silently orphan the overlay entry that corrects it — the
// operation would come back uncorrected, looking fine. Apply therefore fails
// on an entry that matches nothing, rather than ignoring it.
package overlay

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// File is the overlay file name inside a connector package directory.
const File = "overlay.yaml"

// Overlay is a parsed overlay.yaml.
type Overlay struct {
	SchemaVersion int `yaml:"schema_version"`
	// Connector must match the package it applies to. Checked rather than
	// assumed: an overlay applied to the wrong package would rename
	// operations that do not exist and silently correct nothing.
	Connector string `yaml:"connector"`

	// DisplayName / Description replace the vendor's own wording when it is
	// unhelpful ("The Jira Cloud platform REST API" is a title, not a label).
	DisplayName string `yaml:"display_name,omitempty"`
	Description string `yaml:"description,omitempty"`

	// Auth REPLACES the derived schemes when non-empty. Replacement rather
	// than merge, because the case that matters is a description declaring
	// none at all, and a merge would make "state the truth" impossible when
	// the derivation was wrong rather than absent.
	Auth []spec.AuthScheme `yaml:"auth,omitempty"`
	// BaseURL overrides the derived origin and prefix. A nil field is left
	// alone; OperatorSupplied is a pointer for the same reason.
	BaseURL *BaseURLOverride `yaml:"base_url,omitempty"`
	// Outcome is the connector-wide success/failure policy.
	Outcome *spec.OutcomePolicy `yaml:"outcome,omitempty"`
	// DefaultSecurity replaces the derived root-level requirements.
	DefaultSecurity []spec.SecurityRequirement `yaml:"default_security,omitempty"`
	// Maturity raises or lowers the package floor.
	Maturity spec.Maturity `yaml:"maturity,omitempty"`

	// Drop removes operations by derived id — a deprecated endpoint, one that
	// is dangerous to expose, one the catalog should not carry.
	Drop []string `yaml:"drop,omitempty"`
	// Operations corrects individual operations, keyed by DERIVED id.
	Operations map[string]OperationOverlay `yaml:"operations,omitempty"`
}

// BaseURLOverride corrects where calls go. Every field is a pointer so an
// overlay can set one without clearing the others.
type BaseURLOverride struct {
	Default          *string `yaml:"default,omitempty"`
	PathPrefix       *string `yaml:"path_prefix,omitempty"`
	OperatorSupplied *bool   `yaml:"operator_supplied,omitempty"`
}

// OperationOverlay corrects one operation.
type OperationOverlay struct {
	// ID pins the public id, so a vendor's next release cannot move it. This
	// is the identity lock in its per-operation form: the overlay states the
	// id, the derivation only proposes one.
	ID string `yaml:"id,omitempty"`
	// Summary / Description replace the vendor's wording — for a curated MCP
	// tool this is what an agent reads to choose it, so it is worth writing.
	Summary     string `yaml:"summary,omitempty"`
	Description string `yaml:"description,omitempty"`

	// MCP puts the operation in the facade's curated tool set.
	MCP *bool `yaml:"mcp,omitempty"`
	// Deterministic can only be turned OFF. An overlay may declare that an
	// endpoint is not certifiable on the node path (it runs a model, its
	// result is not reproducible); it may not declare that one the generator
	// refused is fine, because the generator's refusals are structural.
	Deterministic *bool `yaml:"deterministic,omitempty"`
	// Effect corrects a misread: a POST that searches, a PUT that creates.
	Effect spec.Effect `yaml:"effect,omitempty"`
	// Maturity is this operation's own readiness, clamped by the package's.
	Maturity spec.Maturity `yaml:"maturity,omitempty"`
	// Pagination declares how the collection walks.
	Pagination *spec.Pagination `yaml:"pagination,omitempty"`
	// Outcome overrides the connector-wide policy for this operation.
	Outcome *spec.OutcomePolicy `yaml:"outcome,omitempty"`
	// IdempotencyKeyParam names the parameter that makes a retry safe.
	IdempotencyKeyParam string `yaml:"idempotency_key_param,omitempty"`
	// Security replaces the derived requirements for this operation.
	Security []spec.SecurityRequirement `yaml:"security,omitempty"`
	// Params corrects individual parameters, keyed by their derived KEY.
	Params map[string]ParamOverlay `yaml:"params,omitempty"`
}

// ParamOverlay corrects one parameter.
type ParamOverlay struct {
	// Key renames the public key without touching the wire name — a vendor's
	// `q` becomes `query` for a `.bot` author, and the request is unchanged.
	Key         string `yaml:"key,omitempty"`
	Description string `yaml:"description,omitempty"`
	Required    *bool  `yaml:"required,omitempty"`
	// Secret marks a value that must never be logged or shown to a model.
	// Slack declares its credential as an ordinary parameter, so without this
	// a token would travel through an agent's context.
	Secret *bool `yaml:"secret,omitempty"`
	// Style and Explode correct a serialization the description got wrong or
	// left implicit.
	Style   spec.ParamStyle `yaml:"style,omitempty"`
	Explode *bool           `yaml:"explode,omitempty"`
}

// Load reads an overlay from a package directory. A missing file is not an
// error — a package whose description says everything needs no overlay — but
// an unreadable or malformed one is.
func Load(dir string) (*Overlay, error) {
	body, err := os.ReadFile(filepath.Join(dir, File))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("overlay: read %s: %w", File, err)
	}
	return Parse(body)
}

// Parse decodes an overlay document. The version is read by a tolerant
// pre-pass before the strict decode, for the reason spelled out in
// pkg/connector/spec: a document written for a newer iterion must be
// diagnosed as such, not as an unknown field.
func Parse(body []byte) (*Overlay, error) {
	var probe struct {
		SchemaVersion int `yaml:"schema_version"`
	}
	if err := yaml.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("overlay: %s is not valid YAML: %w", File, err)
	}
	if probe.SchemaVersion > spec.SchemaVersion {
		return nil, fmt.Errorf("overlay: %s declares schema_version %d, newer than supported %d (upgrade iterion)", File, probe.SchemaVersion, spec.SchemaVersion)
	}
	var ov Overlay
	if err := yaml.UnmarshalStrict(body, &ov); err != nil {
		return nil, fmt.Errorf("overlay: parse %s: %w", File, err)
	}
	return &ov, nil
}

// Apply merges an overlay onto a generated package IN PLACE and runs the
// COMPLETE validation on the result — because the merged package is what a
// launch would use, and the whole point of the split is that neither half is
// usable alone.
//
// Every entry must match something. An overlay naming an operation the
// package does not have is an ERROR, not a no-op: the usual cause is a
// regeneration that moved a derived id, and ignoring it would bring the
// operation back UNCORRECTED — a Slack call with an unmarked credential, a
// list that no longer paginates — while the overlay still looked applied.
func Apply(pkg *spec.Package, ov *Overlay) error {
	if pkg == nil {
		return fmt.Errorf("overlay: no package to apply to")
	}
	if ov == nil {
		return pkg.Validate()
	}
	if ov.Connector != "" && ov.Connector != pkg.Connector.ID {
		return fmt.Errorf("overlay: declares connector %q but the package is %q", ov.Connector, pkg.Connector.ID)
	}

	c := &pkg.Connector
	if ov.DisplayName != "" {
		c.DisplayName = ov.DisplayName
	}
	if ov.Description != "" {
		c.Description = ov.Description
	}
	if len(ov.Auth) > 0 {
		c.Auth = ov.Auth
	}
	if ov.Outcome != nil {
		c.Outcome = ov.Outcome
	}
	if len(ov.DefaultSecurity) > 0 {
		c.DefaultSecurity = ov.DefaultSecurity
	}
	if ov.Maturity != "" {
		c.Maturity = ov.Maturity
	}
	if b := ov.BaseURL; b != nil {
		if b.Default != nil {
			c.BaseURL.Default = *b.Default
		}
		if b.PathPrefix != nil {
			c.BaseURL.PathPrefix = *b.PathPrefix
		}
		if b.OperatorSupplied != nil {
			c.BaseURL.OperatorSupplied = *b.OperatorSupplied
		}
	}

	if err := applyDrops(pkg, ov.Drop); err != nil {
		return err
	}
	if err := applyOperations(pkg, ov.Operations); err != nil {
		return err
	}
	return pkg.Validate()
}

// applyDrops removes named operations, refusing a name that matches nothing.
func applyDrops(pkg *spec.Package, drop []string) error {
	if len(drop) == 0 {
		return nil
	}
	want := make(map[string]bool, len(drop))
	for _, id := range drop {
		want[id] = true
	}
	hit := map[string]bool{}
	for i := range pkg.Ops {
		kept := pkg.Ops[i].Operations[:0]
		for _, op := range pkg.Ops[i].Operations {
			if want[op.ID] {
				hit[op.ID] = true
				continue
			}
			kept = append(kept, op)
		}
		pkg.Ops[i].Operations = kept
	}
	return unmatched("drop", want, hit)
}

// applyOperations corrects each named operation.
func applyOperations(pkg *spec.Package, ops map[string]OperationOverlay) error {
	if len(ops) == 0 {
		return nil
	}
	want := make(map[string]bool, len(ops))
	for id := range ops {
		want[id] = true
	}
	hit := map[string]bool{}
	for i := range pkg.Ops {
		for j := range pkg.Ops[i].Operations {
			op := &pkg.Ops[i].Operations[j]
			o, ok := ops[op.ID]
			if !ok {
				continue
			}
			hit[op.ID] = true
			if err := applyOperation(op, o); err != nil {
				return err
			}
		}
	}
	return unmatched("operations", want, hit)
}

func applyOperation(op *spec.Operation, o OperationOverlay) error {
	if o.ID != "" {
		if !strings.HasPrefix(o.ID, strings.SplitN(op.ID, ".", 2)[0]+".") {
			return fmt.Errorf("overlay: operation %q pins the id %q, which is outside the connector's namespace", op.ID, o.ID)
		}
		op.ID = o.ID
	}
	if o.Summary != "" {
		op.Summary = o.Summary
	}
	if o.Description != "" {
		op.Description = o.Description
	}
	if o.MCP != nil {
		op.MCP = *o.MCP
	}
	if o.Deterministic != nil {
		// One direction only. A generator marks an HTTP call deterministic by
		// construction; an overlay knows things it cannot — that an endpoint
		// runs a model, that its result is not reproducible. But letting an
		// overlay claim determinism BACK would let a human overrule a
		// structural refusal with an assertion, which is exactly the kind of
		// promise this catalog must not accept on trust.
		if *o.Deterministic {
			return fmt.Errorf("overlay: operation %q sets deterministic: true — an overlay may only REMOVE the claim, never assert it", op.ID)
		}
		op.Deterministic = false
	}
	if o.Effect != "" {
		op.Effect = o.Effect
	}
	if o.Maturity != "" {
		op.Maturity = o.Maturity
	}
	if o.Pagination != nil {
		op.Pagination = o.Pagination
	}
	if o.Outcome != nil {
		op.Outcome = o.Outcome
	}
	if o.IdempotencyKeyParam != "" {
		op.IdempotencyKeyParam = o.IdempotencyKeyParam
	}
	if len(o.Security) > 0 {
		op.Security = o.Security
		op.Anonymous = false
	}
	return applyParams(op, o.Params)
}

func applyParams(op *spec.Operation, params map[string]ParamOverlay) error {
	if len(params) == 0 {
		return nil
	}
	want := make(map[string]bool, len(params))
	for key := range params {
		want[key] = true
	}
	hit := map[string]bool{}
	for i := range op.Params {
		p := &op.Params[i]
		o, ok := params[p.Key]
		if !ok {
			continue
		}
		hit[p.Key] = true
		if o.Key != "" {
			p.Key = o.Key
		}
		if o.Description != "" {
			p.Description = o.Description
		}
		if o.Required != nil {
			p.Required = *o.Required
		}
		if o.Secret != nil {
			p.Secret = *o.Secret
		}
		if o.Style != "" {
			p.Style = o.Style
		}
		if o.Explode != nil {
			p.Explode = o.Explode
		}
	}
	return unmatched("operation "+op.ID+" params", want, hit)
}

// unmatched turns "this overlay entry matched nothing" into an error naming
// every miss at once, so a regeneration that moved several ids is fixed in one
// pass rather than one error at a time.
func unmatched(what string, want, hit map[string]bool) error {
	var missing []string
	for id := range want {
		if !hit[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("overlay: %s names %s, which the package does not contain — a regeneration probably moved the id, and applying the rest would bring the operation back uncorrected",
		what, strings.Join(quoteAll(missing), ", "))
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}
