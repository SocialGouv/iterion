package spec_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// identPackage builds a loadable package carrying the identity block given.
func identPackage(identity string) fstest.MapFS {
	conn := "schema_version: 1\n" +
		"id: acme\n" +
		"version: 0.1.0\n" +
		identity +
		"base_url:\n" +
		"  default: https://acme.example\n" +
		"auth:\n" +
		"- id: token\n" +
		"  kind: api_key\n" +
		"  in: header\n" +
		"  name: Authorization\n"
	return fstest.MapFS{
		"connector.yaml": &fstest.MapFile{Data: []byte(conn)},
	}
}

func identLoad(t *testing.T, identity string) error {
	t.Helper()
	_, err := spec.LoadFS(identPackage(identity), ".")
	return err
}

// TestCategoryIsAClosedTaxonomy: a known category loads, the empty one
// loads as uncategorized, and an unknown one is refused as the typo it is.
func TestCategoryIsAClosedTaxonomy(t *testing.T) {
	if err := identLoad(t, "category: source_control\n"); err != nil {
		t.Fatalf("known category: %v", err)
	}
	if err := identLoad(t, ""); err != nil {
		t.Fatalf("uncategorized: %v", err)
	}
	err := identLoad(t, "category: sourc_control\n")
	if err == nil || !strings.Contains(err.Error(), "sourc_control") {
		t.Fatalf("typo'd category: err = %v, want a refusal naming it", err)
	}
}

// TestBrandColorShape: an accent is six hex digits behind a "#", or absent.
func TestBrandColorShape(t *testing.T) {
	for _, ok := range []string{"#ff9e1b", "#2B303B"} {
		if err := identLoad(t, "brand_color: \""+ok+"\"\n"); err != nil {
			t.Fatalf("brand_color %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"ff9e1b", "#ff9e1", "#ff9e1bb", "orange"} {
		err := identLoad(t, "brand_color: \""+bad+"\"\n")
		if err == nil || !strings.Contains(err.Error(), "brand_color") {
			t.Fatalf("brand_color %q: err = %v, want a refusal naming brand_color", bad, err)
		}
	}
}

// TestValidateRefusesAHandBuiltIcon: a Package whose connector declares an
// icon but that no load read one for cannot pass Validate — that Package
// was built by hand, and Validate is what keeps an unsanitized byte from
// travelling under a route's nose.
func TestValidateRefusesAHandBuiltIcon(t *testing.T) {
	p := spec.Package{Connector: spec.Connector{
		ID:      "acme",
		Version: "0.1.0",
		Icon:    "icon.svg",
		BaseURL: spec.BaseURL{Default: "https://acme.example"},
		Auth: []spec.AuthScheme{
			{ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization"},
		},
	}}
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("hand-built icon: err = %v, want the load invariant's refusal", err)
	}
	p.IconSVG = []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)
	p.IconDigest = "0123456789abcdef"
	if err := p.Validate(); err != nil {
		t.Fatalf("loaded icon: %v", err)
	}
}
