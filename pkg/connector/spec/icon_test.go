package spec_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// iconPackage builds a loadable package whose icon is body.
func iconPackage(body string, extra map[string]string) fstest.MapFS {
	conn := "" +
		"schema_version: 1\n" +
		"id: acme\n" +
		"version: 0.1.0\n" +
		"icon: icon.svg\n" +
		"base_url:\n" +
		"  default: https://acme.example\n" +
		"auth:\n" +
		"- id: token\n" +
		"  kind: api_key\n" +
		"  in: header\n" +
		"  name: Authorization\n"
	files := fstest.MapFS{
		"connector.yaml": &fstest.MapFile{Data: []byte(conn)},
		"icon.svg":       &fstest.MapFile{Data: []byte(body)},
	}
	for name, data := range extra {
		files[name] = &fstest.MapFile{Data: []byte(data)}
	}
	return files
}

// loadIconPackage loads iconPackage and fails the test on any load error.
func loadIconPackage(t *testing.T, files fstest.MapFS) *spec.Package {
	t.Helper()
	p, err := spec.LoadFS(files, ".")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return p
}

// TestLoadIconSanitizedAtLoad drives the load: an icon carrying every class
// the sanitizer drops is served as the sanitized bytes, and the digest is
// of those bytes — never of the file's.
func TestLoadIconSanitizedAtLoad(t *testing.T) {
	body := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">
  <circle cx="9" cy="7" r="4"/>
  <script>alert(1)</script>
  <foreignObject><div>html</div></foreignObject>
  <style>circle { fill: url(http://evil.example) }</style>
  <a xlink:href="http://evil.example"><rect width="4" height="4"/></a>
  <rect onload="alert(2)" onbegin="x" width="4" height="4" style="fill:url(http://evil.example)"/>
  <rect href="#keep" width="4" height="4"/>
  <!-- comment -->
  <text>plain</text>
</svg>`
	p := loadIconPackage(t, iconPackage(body, nil))
	svg := string(p.IconSVG)
	for _, dropped := range []string{
		"<script", "onload", "onbegin", "foreignObject", "<style",
		"evil.example", "<!--", "javascript:", "url(",
	} {
		if strings.Contains(svg, dropped) {
			t.Fatalf("sanitized icon still carries %q:\n%s", dropped, svg)
		}
	}
	for _, kept := range []string{"<circle", `<rect href="#keep"`, "<text>plain</text>"} {
		if !strings.Contains(svg, kept) {
			t.Fatalf("sanitized icon lost %q:\n%s", kept, svg)
		}
	}
	if got := strings.Count(svg, `xmlns=`); got != 1 {
		t.Fatalf("sanitized icon declares %d xmlns, want exactly the root's one:\n%s", got, svg)
	}
	if p.IconDigest == "" || len(p.IconDigest) != 16 {
		t.Fatalf("IconDigest = %q, want 16 hex chars", p.IconDigest)
	}
	sum := sha256.Sum256(p.IconSVG)
	if got := hex.EncodeToString(sum[:])[:16]; got != p.IconDigest {
		t.Fatalf("IconDigest %q is not of the sanitized bytes (%q)", p.IconDigest, got)
	}
}

// TestLoadIconCapIsABoundary drives the cap from both sides: the sanitized
// bytes of an icon exactly at the cap load, one byte over refuse, and the
// refusal names the operator's var so a surprise reads as the bound it is.
func TestLoadIconCapIsABoundary(t *testing.T) {
	// 0 removes the cap: the override is the operator's to state.
	t.Setenv(spec.EnvIconMaxBytes, "0")
	big := `<svg xmlns="http://www.w3.org/2000/svg"><text>` + strings.Repeat("x", 80<<10) + `</text></svg>`
	if _, err := spec.LoadFS(iconPackage(big, nil), "."); err != nil {
		t.Fatalf("uncapped: %v", err)
	}
	t.Setenv(spec.EnvIconMaxBytes, "240")
	base := loadIconPackage(t, iconPackage(`<svg xmlns="http://www.w3.org/2000/svg"><text>hi</text></svg>`, nil))
	at := len(base.IconSVG) + 3 // headroom for the boundary pair below
	t.Setenv(spec.EnvIconMaxBytes, strconv.Itoa(at))
	scaffold := len(`<svg xmlns="http://www.w3.org/2000/svg"><text></text></svg>`)
	atIcon := `<svg xmlns="http://www.w3.org/2000/svg"><text>` + strings.Repeat("x", at-scaffold) + `</text></svg>`
	p := loadIconPackage(t, iconPackage(atIcon, nil))
	if got := len(p.IconSVG); got != at {
		t.Fatalf("sanitized icon is %d bytes, want exactly the cap %d", got, at)
	}
	overIcon := `<svg xmlns="http://www.w3.org/2000/svg"><text>` + strings.Repeat("x", at-scaffold+1) + `</text></svg>`
	_, err := spec.LoadFS(iconPackage(overIcon, nil), ".")
	if err == nil || !strings.Contains(err.Error(), spec.EnvIconMaxBytes) {
		t.Fatalf("over-cap load: err = %v, want one naming %s", err, spec.EnvIconMaxBytes)
	}
}

// TestLoadIconBadBoundRefusesTheLoad drives the bound itself: a value that
// is not a non-negative byte count refuses the load — a fallback to the
// default would serve the file under a bound the operator did not choose.
func TestLoadIconBadBoundRefusesTheLoad(t *testing.T) {
	for _, bad := range []string{"abc", "-1"} {
		t.Setenv(spec.EnvIconMaxBytes, bad)
		_, err := spec.LoadFS(iconPackage(`<svg xmlns="http://www.w3.org/2000/svg"/>`, nil), ".")
		if err == nil || !strings.Contains(err.Error(), spec.EnvIconMaxBytes) {
			t.Fatalf("bound %q: err = %v, want a refusal naming %s", bad, err, spec.EnvIconMaxBytes)
		}
	}
}

// TestLoadIconPathGuards refuses the icon paths that leave the package.
func TestLoadIconPathGuards(t *testing.T) {
	for _, bad := range []string{"../out.svg", "/abs.svg", "missing.svg", "."} {
		files := iconPackage(`<svg xmlns="http://www.w3.org/2000/svg"/>`, nil)
		conn := files["connector.yaml"]
		conn.Data = []byte(strings.ReplaceAll(string(conn.Data), "icon: icon.svg", "icon: "+bad))
		_, err := spec.LoadFS(files, ".")
		if bad == "missing.svg" {
			if err == nil {
				t.Fatalf("icon %q loaded, want the read's refusal", bad)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "not a slash path") {
			t.Fatalf("icon %q: err = %v, want the guard's refusal naming a slash path", bad, err)
		}
	}
}

// TestLoadIconDefaultCapHolds: with no operator bound the default cap
// refuses an oversized icon, and the refusal still names the var.
func TestLoadIconDefaultCapHolds(t *testing.T) {
	body := `<svg xmlns="http://www.w3.org/2000/svg"><text>` +
		strings.Repeat("x", 80<<10) + `</text></svg>`
	_, err := spec.LoadFS(iconPackage(body, nil), ".")
	if err == nil || !strings.Contains(err.Error(), spec.EnvIconMaxBytes) {
		t.Fatalf("default cap: err = %v, want a refusal naming %s", err, spec.EnvIconMaxBytes)
	}
}
