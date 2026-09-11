package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/gen"
	"github.com/SocialGouv/iterion/pkg/connector/overlay"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
	"github.com/SocialGouv/iterion/pkg/store"
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
	// The CATALOG's own rule, not a second one. The id names a directory this
	// command writes — and whose `ops/` it REMOVES first — so `--id
	// ../../src` deleted `../src/ops` and wrote a package outside
	// `connectors/`; and it seeds the first segment of every operation id, so
	// `--id google.drive` generated cleanly and was then unaddressable,
	// ResolveAction cutting the action id at its first dot. Asking the rule
	// the resolver applies keeps "what can be written" equal to "what can be
	// resolved".
	if err := connection.CheckConnectorID(strings.TrimSpace(opts.ID)); err != nil {
		return fmt.Errorf("connectors: %w", err)
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
	noteUngrantedDestination(out, opts.Out)
	return nil
}

// noteUngrantedDestination says so when a package was written somewhere this
// process would not read it.
//
// `--out` defaults to `connectors/<id>` — the PROJECT tier, which is consulted
// only under ITERION_CONNECTOR_PROJECT_CATALOG because the workspace is the
// repository a run acts on. Without this line the documented first flow ends
// with "wrote connectors/forgejo — 503 operations" and then a `connections
// add` that cannot find the connector, which reads as a broken generation
// rather than as a grant nobody made. Said at the moment the directory is
// created, for the same reason `connections add` warns about a base URL the
// guard will refuse.
//
// A NOTE, never a refusal: writing a package the operator will grant later, or
// commit for a colleague, or copy into their home tier, is all legitimate.
func noteUngrantedDestination(out io.Writer, dest string) {
	if connection.ProjectCatalogGranted() {
		return
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return
	}
	wd, err := os.Getwd()
	if err != nil {
		return
	}
	// Only the project tier of THIS workspace: `--out ~/.iterion/connectors/x`
	// is the home tier and reads fine, and any other path is somewhere the
	// operator is deliberately staging a package.
	if filepath.Dir(abs) != filepath.Join(wd, "connectors") {
		return
	}
	fmt.Fprintf(out, "\nnote: %s is this workspace's PROJECT catalog, which iterion does not consult by default —\n", dest)
	fmt.Fprintf(out, "  a repository a run acts on must not be able to redefine what an operation does.\n")
	fmt.Fprintf(out, "  Set %s=1 to use it here, or move it to %s to install it for every project.\n",
		connection.ProjectCatalogEnv, filepath.Join(store.GlobalIterionDataDir(), "connectors"))
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
	// The URL that goes into the PROVENANCE is the redacted one. This lane
	// exists for a description an operator may not redistribute — i.e. exactly
	// the one that sits behind auth — and the only credential a fetch URL can
	// carry is in its userinfo or its query (`?private_token=…`). Recorded
	// verbatim, it was written into `connector.yaml` at 0644, in a directory
	// whose whole point is to be committed. Go itself redacts userinfo when it
	// prints a URL in an error; only what we persisted kept it in the clear.
	return data, redactedSpecURL(source), nil
}

// redactedSpecURL strips the credential material a fetch URL may carry, while
// keeping it recognisable as the source it was.
//
// The QUERY goes whole: a token there has no fixed parameter name
// (`private_token`, `access_token`, `key`, …) and guessing the list is how the
// next spelling leaks. What identifies the description is its host and path.
func redactedSpecURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// Unparsable, so nothing can be said about which part is a secret.
		// The provenance keeps the fetch's own note rather than the string.
		return ""
	}
	u.User = nil
	if u.RawQuery != "" {
		u.RawQuery = ""
		u.ForceQuery = false
		return u.String() + " (query omitted)"
	}
	u.Fragment = ""
	return u.String()
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
