package gen_test

// P0 measurement harness. It is a TEST rather than a throwaway main so the
// numbers behind the packaging decision (embed in the binary vs COPY into the
// image) can be reproduced by anyone, on any spec, without re-deriving the
// invocation:
//
//	ITERION_CONNECTOR_SPEC=/tmp/forgejo_swagger.json \
//	ITERION_CONNECTOR_ID=forgejo go test ./pkg/connector/gen -run Measure -v
//
// It SKIPS without that env var, so it costs a normal `go test ./...` nothing
// and never depends on a network fetch.

import (
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/gen"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

func TestMeasureRealSpec(t *testing.T) {
	path := os.Getenv("ITERION_CONNECTOR_SPEC")
	if path == "" {
		t.Skip("set ITERION_CONNECTOR_SPEC to measure a real vendor description")
	}
	id := os.Getenv("ITERION_CONNECTOR_ID")
	if id == "" {
		id = "probe"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	pkg, err := gen.Generate(data, gen.Options{
		ConnectorID:             id,
		SpecURL:                 os.Getenv("ITERION_CONNECTOR_SPEC_URL"),
		SpecLicense:             os.Getenv("ITERION_CONNECTOR_SPEC_LICENSE"),
		Redistributable:         os.Getenv("ITERION_CONNECTOR_REDISTRIBUTABLE") == "1",
		OperatorSuppliedBaseURL: true,
		GeneratedBy:             "p0-measurement",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	dir := t.TempDir()
	if err := spec.Write(dir, pkg); err != nil {
		t.Fatalf("write: %v", err)
	}
	size, err := spec.Measure(dir)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}

	ops := pkg.Operations()
	byEffect := map[spec.Effect]int{}
	withBody, withEnum, noSummary := 0, 0, 0
	for _, op := range ops {
		byEffect[op.Effect]++
		for _, p := range op.Params {
			if p.In == spec.InBody {
				withBody++
				break
			}
		}
		for _, p := range op.Params {
			if len(p.Enum) > 0 {
				withEnum++
				break
			}
		}
		if op.Summary == "" {
			noSummary++
		}
	}

	t.Logf("connector      %s (%s, spec %s)", pkg.Connector.ID, pkg.Connector.Provenance.SpecFormat, pkg.Connector.Provenance.SpecVersion)
	t.Logf("operations     %d in %d domains", len(ops), len(pkg.Ops))
	t.Logf("schemas        %d", len(pkg.Schemas))
	t.Logf("auth schemes   %s", authSummary(pkg.Connector.Auth))
	t.Logf("base url       default=%q prefix=%q operator_supplied=%v",
		pkg.Connector.BaseURL.Default, pkg.Connector.BaseURL.PathPrefix, pkg.Connector.BaseURL.OperatorSupplied)
	t.Logf("effects        read=%d create=%d update=%d delete=%d",
		byEffect[spec.EffectRead], byEffect[spec.EffectCreate], byEffect[spec.EffectUpdate], byEffect[spec.EffectDelete])
	t.Logf("params         %d ops carry a body, %d carry an enum, %d have no summary", withBody, withEnum, noSummary)
	t.Logf("SIZE  total    %s in %d files", human(size.Total()), size.Files)
	t.Logf("SIZE  ops/     %s", human(size.Ops))
	t.Logf("SIZE  schemas  %s", human(size.Schemas))
	t.Logf("SIZE  manifest %s", human(size.Connector))

	// A round trip through disk is the real check: a package that cannot be
	// re-read is not a package.
	reloaded, err := spec.LoadGenerated(dir)
	if err != nil {
		t.Fatalf("reload the written package: %v", err)
	}
	if got, want := len(reloaded.Operations()), len(ops); got != want {
		t.Fatalf("round trip lost operations: wrote %d, read %d", want, got)
	}
	// What the overlay still owes, said out loud rather than discovered at
	// the first launch. A vendor description that carries no auth is normal.
	if err := reloaded.Validate(); err != nil {
		t.Logf("COMPLETE       no — the overlay still owes: %v", err)
	} else {
		t.Log("COMPLETE       yes — usable without an overlay")
	}

	t.Log("sample operations:")
	for _, op := range sampleOps(ops) {
		t.Logf("  %-46s %-6s %s", op.ID, op.HTTP.Method, op.HTTP.Path)
	}
}

func authSummary(schemes []spec.AuthScheme) string {
	out := make([]string, 0, len(schemes))
	for _, s := range schemes {
		entry := fmt.Sprintf("%s(%s", s.ID, s.Kind)
		if s.In != "" {
			entry += fmt.Sprintf(",%s:%s", s.In, s.Name)
		}
		if s.ValuePrefix != "" {
			entry += fmt.Sprintf(",prefix=%q", s.ValuePrefix)
		}
		out = append(out, entry+")")
	}
	return fmt.Sprint(out)
}

// sampleOps spreads the sample across the whole sorted list instead of taking
// the head, so the log shows several domains rather than whichever one sorts
// first.
func sampleOps(ops []spec.Operation) []spec.Operation {
	const want = 12
	if len(ops) <= want {
		return ops
	}
	step := len(ops) / want
	out := make([]spec.Operation, 0, want)
	for i := 0; i < len(ops) && len(out) < want; i += step {
		out = append(out, ops[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
