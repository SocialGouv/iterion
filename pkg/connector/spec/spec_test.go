package spec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// validPackage is the smallest package that passes the COMPLETE check. Each
// test below breaks exactly one thing about it, so a failure names its cause.
func validPackage() *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion,
			ID:            "probe",
			Version:       "0.1.0",
			BaseURL:       spec.BaseURL{Default: "https://probe.example", PathPrefix: "/api/v1"},
			Auth: []spec.AuthScheme{{
				ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization",
			}},
			Maturity: spec.MaturityExperimental,
		},
		Schemas: map[string]spec.Schema{
			"Issue": {Type: "object", Properties: map[string]spec.Schema{"id": {Type: "integer"}}},
		},
		Ops: []spec.OpsFile{{
			SchemaVersion: spec.SchemaVersion,
			Connector:     "probe",
			Domain:        "issue",
			Operations: []spec.Operation{{
				ID: "probe.issue.get", Resource: "issue", Verb: "get",
				HTTP:          spec.HTTPBinding{Method: "GET", Path: "/issues/{id}"},
				Params:        []spec.Param{{Key: "id", Name: "id", In: spec.InPath, Type: "integer", Required: true}},
				Results:       []spec.ResultCase{{Status: 200, SchemaRef: "Issue"}},
				Effect:        spec.EffectRead,
				Deterministic: true,
			}},
		}},
	}
}

func TestValidPackagePasses(t *testing.T) {
	if err := validPackage().Validate(); err != nil {
		t.Fatalf("the reference package must validate: %v", err)
	}
}

// TestAWellFormedPaginationPasses is the other half of the refusals below: a
// guard that only ever says no would be satisfied by refusing everything.
func TestAWellFormedPaginationPasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*spec.Package)
	}{
		{"a bare array needs no items_field", func(p *spec.Package) {
			p.Ops[0].Operations[0].Results[0].Array = true
			p.Ops[0].Operations[0].Pagination = &spec.Pagination{
				Style: spec.PageNumber, PageParam: "page", SizeParam: "limit", DefaultSize: 50,
			}
		}},
		{"an envelope named by items_field", func(p *spec.Package) {
			p.Ops[0].Operations[0].Pagination = &spec.Pagination{
				Style: spec.PageNumber, PageParam: "page", SizeParam: "limit",
				ItemsField: "data", DefaultSize: 50,
			}
		}},
		{"a cursor walk that names its cursor", func(p *spec.Package) {
			p.Ops[0].Operations[0].Results[0].Array = true
			p.Ops[0].Operations[0].Pagination = &spec.Pagination{
				Style: spec.PageCursor, CursorParam: "after", CursorField: "next",
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := validPackage()
			tc.set(p)
			if err := p.Validate(); err != nil {
				t.Fatalf("must validate: %v", err)
			}
		})
	}
}

// TestValidateRefusals covers every way a package can be internally
// incoherent. Each case states what the refusal PREVENTS, because a
// validation nobody can explain gets deleted the first time it is
// inconvenient.
func TestValidateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*spec.Package)
		wantMsg string
	}{
		{
			// Prevents: a request sent to a literally-templated URL.
			name: "path placeholder with no parameter",
			mutate: func(p *spec.Package) {
				p.Ops[0].Operations[0].Params = nil
			},
			wantMsg: "placeholder",
		},
		{
			// Prevents: a parameter that can never be placed on the wire.
			name: "path parameter absent from the path",
			mutate: func(p *spec.Package) {
				p.Ops[0].Operations[0].HTTP.Path = "/issues"
			},
			wantMsg: "does not appear in path",
		},
		{
			// Prevents: an optional value the URL cannot be built without.
			name: "optional path parameter",
			mutate: func(p *spec.Package) {
				p.Ops[0].Operations[0].Params[0].Required = false
			},
			wantMsg: "must be required",
		},
		{
			// Prevents: a result nothing can validate or type.
			name: "result references an unknown schema",
			mutate: func(p *spec.Package) {
				p.Ops[0].Operations[0].Results[0].SchemaRef = "Ghost"
			},
			wantMsg: "unknown schema",
		},
		{
			// Prevents: two operations sharing an id, one of them unreachable.
			name: "duplicate operation id",
			mutate: func(p *spec.Package) {
				dup := p.Ops[0].Operations[0]
				p.Ops[0].Operations = append(p.Ops[0].Operations, dup)
			},
			wantMsg: "duplicate operation id",
		},
		{
			// Prevents: an id that does not name its own connector, which
			// would collide across packages in a shared catalog.
			name: "operation id outside the connector namespace",
			mutate: func(p *spec.Package) {
				p.Ops[0].Operations[0].ID = "other.issue.get"
			},
			wantMsg: "must start with",
		},
		{
			// Prevents: an operation claiming a readiness the package never
			// earned. EffectiveMaturity clamps it at read time, but a package
			// that CONTRADICTS itself is an authoring mistake worth naming.
			name: "operation more mature than its package",
			mutate: func(p *spec.Package) {
				p.Ops[0].Operations[0].Maturity = spec.MaturityQualified
			},
			wantMsg: "above the package",
		},
		{
			// Prevents: licensing a retry the operation cannot make safe.
			name: "idempotency key naming no parameter",
			mutate: func(p *spec.Package) {
				p.Ops[0].Operations[0].IdempotencyKeyParam = "Idempotency-Key"
			},
			wantMsg: "names no parameter",
		},
		{
			// Prevents: a connector nobody can authenticate against.
			name: "no auth scheme",
			mutate: func(p *spec.Package) {
				p.Connector.Auth = nil
			},
			wantMsg: "auth scheme",
		},
		{
			// Prevents: a call with no address.
			name: "no base url and not operator supplied",
			mutate: func(p *spec.Package) {
				p.Connector.BaseURL = spec.BaseURL{}
			},
			wantMsg: "base url",
		},
		{
			// Prevents: an api_key nobody knows where to put.
			name: "api_key with no location",
			mutate: func(p *spec.Package) {
				p.Connector.Auth[0].In = ""
			},
			wantMsg: "must be header or query",
		},
		{
			// Prevents: an oauth scheme no flow can be driven with.
			name: "oauth without endpoints",
			mutate: func(p *spec.Package) {
				p.Connector.Auth[0] = spec.AuthScheme{ID: "oauth", Kind: spec.AuthOAuth2}
			},
			wantMsg: "auth_url and token_url",
		},
		{
			// Prevents: an operation whose retry policy cannot be decided.
			name: "missing effect",
			mutate: func(p *spec.Package) {
				p.Ops[0].Operations[0].Effect = ""
			},
			wantMsg: "missing effect",
		},
		{
			// Prevents THE dangerous pagination defect: the success body is an
			// object, no items_field names the array inside it, so the walk
			// extracts nothing from every page — and an empty page is how the
			// walk knows the collection ended. The caller receives zero items
			// and the word "complete". This is the shipped Forgejo
			// repository.search shape.
			name: "paginated over a non-array response with no items_field",
			mutate: func(p *spec.Package) {
				p.Ops[0].Operations[0].Pagination = &spec.Pagination{
					Style: spec.PageNumber, PageParam: "page", SizeParam: "limit", DefaultSize: 50,
				}
			},
			wantMsg: "is not an array and no items_field",
		},
		{
			// Prevents: a cursor walk that can never advance, silently
			// returning page one as the whole collection.
			name: "cursor pagination with no cursor_field",
			mutate: func(p *spec.Package) {
				p.Ops[0].Operations[0].Results[0].Array = true
				p.Ops[0].Operations[0].Pagination = &spec.Pagination{
					Style: spec.PageCursor, CursorParam: "after",
				}
			},
			wantMsg: "no cursor_field",
		},
		{
			// Prevents: a style the executor has no arm for reaching
			// production, where it fails at the first call instead of at
			// validation. link_header is declared in the model but not yet
			// walkable.
			name: "a pagination style the executor cannot walk",
			mutate: func(p *spec.Package) {
				p.Ops[0].Operations[0].Results[0].Array = true
				p.Ops[0].Operations[0].Pagination = &spec.Pagination{Style: spec.PageLink}
			},
			wantMsg: "not one iterion can walk",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := validPackage()
			tc.mutate(p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("want a refusal mentioning %q", tc.wantMsg)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("refusal = %v, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

// TestEffectiveMaturityClamps pins the read-side guarantee that pairs with
// the authoring-side refusal above: whatever a stored package says, a reader
// never sees an operation above its package's floor.
func TestEffectiveMaturityClamps(t *testing.T) {
	p := validPackage()
	p.Connector.Maturity = spec.MaturityExperimental
	op := p.Ops[0].Operations[0]

	op.Maturity = ""
	if got := p.EffectiveMaturity(op); got != spec.MaturityExperimental {
		t.Errorf("an operation declaring nothing = %q, want the package floor", got)
	}
	op.Maturity = spec.MaturityQualified
	if got := p.EffectiveMaturity(op); got != spec.MaturityExperimental {
		t.Errorf("an over-claiming operation = %q, want it clamped to the floor", got)
	}
	op.Maturity = spec.MaturitySpotted
	if got := p.EffectiveMaturity(op); got != spec.MaturitySpotted {
		t.Errorf("an operation below the floor = %q, want its own lower value kept", got)
	}
}

// TestSpottedIsInert is the property the whole large-index strategy rests on:
// a merely-spotted entry is visible and NOT attachable, so an index of
// hundreds promises nothing it has not qualified. The empty value must read
// as spotted too — an entry that declares nothing must not become attachable
// by omission.
func TestSpottedIsInert(t *testing.T) {
	for _, m := range []spec.Maturity{spec.MaturitySpotted, ""} {
		if m.Attachable() {
			t.Errorf("maturity %q must not be attachable", m)
		}
	}
	for _, m := range []spec.Maturity{spec.MaturityExperimental, spec.MaturityQualified, spec.MaturityDeprecated} {
		if !m.Attachable() {
			t.Errorf("maturity %q must be attachable", m)
		}
	}
}

// TestVersionIsReadBeforeTheStrictParse is the R07 lesson made a test.
// pkg/plugin decodes strictly BEFORE checking schema_version, so its own
// "upgrade iterion" diagnostic is unreachable: a future manifest fails on
// whichever unknown field the decoder meets first. A connector package is
// designed to grow, so the order is inverted here — and asserted, because
// nothing else would notice it silently flipping back.
func TestVersionIsReadBeforeTheStrictParse(t *testing.T) {
	dir := t.TempDir()
	body := "schema_version: 99\nid: probe\nversion: 0.1.0\nsome_field_this_build_never_heard_of: true\n"
	if err := os.WriteFile(filepath.Join(dir, spec.ConnectorFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := spec.Load(dir)
	if err == nil {
		t.Fatal("a package from a newer schema must be refused")
	}
	if !strings.Contains(err.Error(), "upgrade iterion") {
		t.Errorf("refusal = %v — it must name the version, not the unknown field it happened to meet first", err)
	}
}

// TestRoundTrip pins that what Write produces, Load reads back identically —
// the property a three-tier distribution depends on, since a package travels
// through disk on its way to every runner.
func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	orig := validPackage()
	if err := spec.Write(dir, orig); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := spec.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if back.Connector.ID != orig.Connector.ID || back.Connector.Version != orig.Connector.Version {
		t.Errorf("identity changed: %+v", back.Connector)
	}
	if got, want := len(back.Operations()), len(orig.Operations()); got != want {
		t.Fatalf("operations: read %d, wrote %d", got, want)
	}
	op, ok := back.Operation("probe.issue.get")
	if !ok {
		t.Fatal("probe.issue.get did not survive the round trip")
	}
	if op.HTTP.Path != "/issues/{id}" || op.Effect != spec.EffectRead {
		t.Errorf("operation changed: %+v", op)
	}
	if _, ok := back.Schemas["Issue"]; !ok {
		t.Error("the shared schema did not survive the round trip")
	}

	size, err := spec.Measure(dir)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if size.Total() == 0 || size.Files == 0 {
		t.Errorf("measure reported nothing: %+v", size)
	}
}

// TestLoadRefusesAnUnknownField guards the strict decode itself: once the
// version is known to be supported, a key this build does not know is a
// mistake, not something to ignore.
func TestLoadRefusesAnUnknownField(t *testing.T) {
	dir := t.TempDir()
	body := "schema_version: 1\nid: probe\nversion: 0.1.0\ntypo_here: true\n"
	if err := os.WriteFile(filepath.Join(dir, spec.ConnectorFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := spec.Load(dir)
	if err == nil {
		t.Fatal("an unknown field at a supported version must be refused")
	}
	if strings.Contains(err.Error(), "upgrade iterion") {
		t.Errorf("a typo must not be reported as a version problem: %v", err)
	}
}

// TestClassifyStatus pins the closed error vocabulary, including the two
// properties a retry decision reads: a status nobody declared is still typed,
// and only the three genuinely transient classes are retryable.
func TestClassifyStatus(t *testing.T) {
	for status, want := range map[int]spec.ErrorClass{
		400: spec.ErrBadRequest, 401: spec.ErrUnauthorized, 403: spec.ErrForbidden,
		404: spec.ErrNotFound, 409: spec.ErrConflict, 422: spec.ErrBadRequest,
		429: spec.ErrRateLimited, 500: spec.ErrUpstream, 503: spec.ErrUpstream,
		418: spec.ErrBadRequest, 200: "",
	} {
		if got := spec.ClassifyStatus(status); got != want {
			t.Errorf("status %d = %q, want %q", status, got, want)
		}
	}
	for _, c := range []spec.ErrorClass{spec.ErrRateLimited, spec.ErrUpstream, spec.ErrTransport} {
		if !c.Retryable() {
			t.Errorf("%q must be retryable", c)
		}
	}
	for _, c := range []spec.ErrorClass{
		spec.ErrBadRequest, spec.ErrUnauthorized, spec.ErrForbidden,
		spec.ErrNotFound, spec.ErrConflict, spec.ErrUnknownOutcome,
	} {
		if c.Retryable() {
			t.Errorf("%q must not be retryable", c)
		}
	}
}
