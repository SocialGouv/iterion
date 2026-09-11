package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/gen"
	"github.com/SocialGouv/iterion/pkg/connector/overlay"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// ConnectorsGenOptions drives `iterion connectors gen`.
type ConnectorsGenOptions struct {
	// Spec is a path or an https URL to the vendor's description.
	Spec string
	// ID is the connector slug; Out is the package directory to write.
	ID  string
	Out string
	// Version is the package semver to stamp.
	Version string
	// License is the licence the DESCRIPTION carries, and Redistributable
	// asserts that the generated operations may ship in iterion's own catalog.
	// The licence is an operator INPUT rather than something read out of the
	// document, because `info.license` is frequently absent and a missing
	// field must never read as permissive.
	License         string
	Redistributable bool
	// OperatorSuppliedBaseURL marks a self-hostable product, so a connection
	// asks for an instance URL instead of assuming the vendor's SaaS origin.
	OperatorSuppliedBaseURL bool
	// KeepOverlay re-applies the package's existing overlay.yaml after
	// generating, which is what turns a regeneration into a diff of the
	// vendor's changes rather than a loss of every correction.
	KeepOverlay bool
}

// ConnectorsGen generates a connector package from a vendor API description.
//
// It is the command that makes a committed package REPRODUCIBLE: a generated
// package that nobody can regenerate is a blob, and the whole
// generated/authored split rests on the generated half being disposable.
//
// It is also the install-time lane. A description iterion may not redistribute
// can still be generated from locally by whoever holds the right to do so —
// which is why the licence and the redistribution assertion are explicit
// inputs, recorded in the package's provenance, rather than inferred.
func ConnectorsGen(opts ConnectorsGenOptions, out io.Writer) error {
	if strings.TrimSpace(opts.ID) == "" {
		return fmt.Errorf("connectors: --id is required (it is the package slug and the first segment of every operation id)")
	}
	if strings.TrimSpace(opts.Out) == "" {
		opts.Out = filepath.Join("connectors", opts.ID)
	}

	data, source, err := readSpec(opts.Spec)
	if err != nil {
		return err
	}

	pkg, report, err := gen.Generate(data, gen.Options{
		ConnectorID:             opts.ID,
		Version:                 opts.Version,
		SpecURL:                 source,
		SpecLicense:             opts.License,
		Redistributable:         opts.Redistributable,
		OperatorSuppliedBaseURL: opts.OperatorSuppliedBaseURL,
		GeneratedBy:             "iterion " + appinfo.Version,
	})
	if err != nil {
		return err
	}

	// The overlay is checked against a SEPARATE copy of the fresh package,
	// never against the one about to be written.
	//
	// Applying it to `pkg` and then writing that is what made regeneration
	// non-idempotent: the merged operations landed in ops/ with their overlay
	// ids already applied, and the next `validate` — which loads ops/ and
	// applies the overlay again — looked up the ORIGINAL ids, found none of
	// them, and failed on thirteen "unmatched" entries. It also broke the
	// contract the package's two halves rest on: ops/ is a pure derivation of
	// the vendor's description, regenerable to the same bytes, and an overlay
	// baked into it is neither.
	//
	// The check itself is worth keeping, which is why it happens at all: a
	// regeneration that moved a derived id must fail here, naming the id,
	// rather than write a package whose corrections silently stopped applying.
	var ov *overlay.Overlay
	// merged is the package AS A LAUNCH WOULD SEE IT — generated plus overlay.
	// Kept apart from `pkg` (what gets written) so the completeness report at
	// the end judges what will actually run, rather than announcing a package
	// incomplete because its authored half has not been merged into the half
	// that must never carry it.
	merged := pkg
	if opts.KeepOverlay {
		ov, err = overlay.Load(opts.Out)
		if err != nil {
			return err
		}
		if ov != nil {
			probe, _, gerr := gen.Generate(data, gen.Options{
				ConnectorID:             opts.ID,
				Version:                 opts.Version,
				SpecURL:                 source,
				SpecLicense:             opts.License,
				Redistributable:         opts.Redistributable,
				OperatorSuppliedBaseURL: opts.OperatorSuppliedBaseURL,
				GeneratedBy:             "iterion " + appinfo.Version,
			})
			if gerr != nil {
				return gerr
			}
			if err := overlay.Apply(probe, ov); err != nil {
				return fmt.Errorf("the existing overlay no longer applies to the regenerated package: %w", err)
			}
			merged = probe
		}
	}

	// The PURE package: what ops/ must hold for the two halves to stay
	// separable.
	if err := writePackage(opts.Out, pkg); err != nil {
		return err
	}
	size, err := spec.Measure(opts.Out)
	if err != nil {
		return err
	}

	ops := pkg.Operations()
	fmt.Fprintf(out, "wrote %s — %d operations in %d domains, %d schemas, %s in %d files\n",
		opts.Out, len(ops), len(pkg.Ops), len(pkg.Schemas), humanBytes(size.Total()), size.Files)
	fmt.Fprintf(out, "  format %s · spec version %s · licence %s · redistributable %v\n",
		pkg.Connector.Provenance.SpecFormat, orNone(pkg.Connector.Provenance.SpecVersion),
		orNone(pkg.Connector.Provenance.SpecLicense), pkg.Connector.Provenance.Redistributable)
	if ov != nil {
		fmt.Fprintf(out, "  overlay re-applied from %s\n", filepath.Join(opts.Out, overlay.File))
	}

	// The gaps are printed, always. A catalog whose holes are invisible is
	// the failure the skip mechanism exists to avoid, so a quiet run would
	// defeat it.
	if len(report.Skipped) > 0 {
		fmt.Fprintf(out, "\n%d operations were NOT published — the description does not describe them well enough to call:\n", len(report.Skipped))
		for _, s := range report.Skipped {
			fmt.Fprintf(out, "  %-6s %s (%s)\n      %s\n", s.Method, s.Path, orNone(s.SourceOperationID), s.Reason)
		}
	}
	// Judged on the MERGED package: what a launch will load is the generated
	// half plus the overlay, so validating the written half alone would report
	// every complete package as incomplete.
	if err := merged.Validate(); err != nil {
		fmt.Fprintf(out, "\nthe package is not complete yet — %v\n", err)
		fmt.Fprintf(out, "write %s to supply it; the generated half is what a vendor's description could state.\n", filepath.Join(opts.Out, overlay.File))
	}
	return nil
}

// readSpec loads a description from a path or an https URL, returning the
// bytes and the source to record in the provenance.
//
// A remote fetch goes through the shared SSRF guard: the URL is operator
// input, and a generator that dialled whatever it was handed would be one
// more way to reach a metadata endpoint from inside the deployment.
func readSpec(source string) ([]byte, string, error) {
	if strings.TrimSpace(source) == "" {
		return nil, "", fmt.Errorf("connectors: --spec is required (a path or an https URL to the vendor's description)")
	}
	if !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "https://") {
		// #nosec G304 — an operator-supplied path is the point of the flag.
		data, err := os.ReadFile(source)
		if err != nil {
			return nil, "", fmt.Errorf("connectors: read %s: %w", source, err)
		}
		return data, "", nil
	}

	// The shared guard, not a hand-rolled client: it pins the resolved
	// address, refuses anything that is not public unicast, and declines to
	// auto-follow a redirect (each hop would re-target an unvalidated host).
	// `strict` is on — a vendor description lives on the public internet, so
	// nothing here has a reason to reach a private address.
	resp, err := httpdial.SafeClient(true, 2*time.Minute).Get(source)
	if err != nil {
		return nil, "", fmt.Errorf("connectors: fetch %s: %w", source, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("connectors: fetch %s: HTTP %d", source, resp.StatusCode)
	}
	// A description is large (GitHub's is 12 MB) but not unbounded; a cap
	// keeps a wrong URL from streaming forever into memory.
	const maxSpecBytes = 64 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes))
	if err != nil {
		return nil, "", fmt.Errorf("connectors: read %s: %w", source, err)
	}
	return data, source, nil
}

// writePackage replaces the generated half of a package directory, leaving
// the authored half alone. ops/ is REMOVED first: an operation the vendor
// dropped must disappear rather than linger as a stale file the next load
// would happily read.
func writePackage(dir string, pkg *spec.Package) error {
	if err := os.RemoveAll(filepath.Join(dir, spec.OpsDir)); err != nil {
		return fmt.Errorf("connectors: clear %s: %w", spec.OpsDir, err)
	}
	return spec.Write(dir, pkg)
}

// ConnectorsValidate loads a package directory, applies its overlay and runs
// the complete check — `iterion validate` for a connector.
func ConnectorsValidate(dir string, out io.Writer) error {
	// The SAME loader a run uses. Assembling the two halves here by hand was
	// how validation and execution came to disagree: this function merged and
	// checked, while the catalog called `spec.Load` and got the generated half
	// alone. An operator's green `validate` described a package no run ever
	// saw.
	pkg, err := overlay.LoadPackage(dir)
	if err != nil {
		return err
	}

	ops := pkg.Operations()
	curated, deterministic := 0, 0
	byMaturity := map[spec.Maturity]int{}
	for _, op := range ops {
		if op.MCP {
			curated++
		}
		if op.Deterministic {
			deterministic++
		}
		byMaturity[pkg.EffectiveMaturity(op)]++
	}
	fmt.Fprintf(out, "%s v%s — %d operations, %d curated for MCP, %d deterministic\n",
		pkg.Connector.ID, pkg.Connector.Version, len(ops), curated, deterministic)
	fmt.Fprintf(out, "  auth: %s\n", strings.Join(schemeIDs(pkg.Connector.Auth), ", "))
	for _, m := range sortedMaturities(byMaturity) {
		fmt.Fprintf(out, "  %-12s %d operations%s\n", m, byMaturity[m], attachableNote(m))
	}
	return nil
}

func schemeIDs(schemes []spec.AuthScheme) []string {
	out := make([]string, 0, len(schemes))
	for _, s := range schemes {
		out = append(out, fmt.Sprintf("%s (%s)", s.ID, s.Kind))
	}
	return out
}

func sortedMaturities(m map[spec.Maturity]int) []spec.Maturity {
	out := make([]spec.Maturity, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func attachableNote(m spec.Maturity) string {
	if m.Attachable() {
		return ""
	}
	return "  (inert — visible in the index, refused at attachment)"
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}
