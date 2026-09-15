package spec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// contractPackage is validPackage with one status opted into a v2 contract.
func contractPackage() *spec.Package {
	p := validPackage()
	p.Connector.SchemaVersion = spec.ResponseContractsVersion
	p.Ops[0].SchemaVersion = spec.ResponseContractsVersion
	p.Ops[0].Operations[0].Results[0].ResponseSchemaRef = "Issue"
	p.ResponseSchemas = map[string]spec.ResponseSchema{
		"Issue": {Type: "object", Required: []string{"id"}, Properties: map[string]spec.ResponseSchema{"id": {Type: "integer"}}},
	}
	return p
}

func writeContractPackage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := spec.Write(dir, contractPackage()); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir
}

// rewriteResponses replaces responses.json with a hand-built document, which is
// the only way to reach the states a WRITER never produces and a hostile or
// hand-edited package can still present.
func rewriteResponses(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, spec.ResponsesFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadErr(t *testing.T, dir string) error {
	t.Helper()
	_, err := spec.LoadGenerated(dir)
	return err
}

func TestAContractPackageRoundTrips(t *testing.T) {
	dir := writeContractPackage(t)
	back, err := spec.LoadGenerated(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(back.ResponseSchemas) != 1 {
		t.Fatalf("contracts = %v, want one", back.ResponseSchemas)
	}
	if back.Ops[0].Operations[0].Results[0].ResponseSchemaRef != "Issue" {
		t.Error("the result case lost its contract reference on disk")
	}
}

// TestAV1PackageWritesNoResponsesFile is the compatibility floor at the IO
// layer, and the obsolete-file rule with it: regenerating a package without
// contracts must not leave the previous run's responses.json behind, still
// loading, still enforced.
func TestAV1PackageWritesNoResponsesFile(t *testing.T) {
	dir := writeContractPackage(t)
	if _, err := os.Stat(filepath.Join(dir, spec.ResponsesFile)); err != nil {
		t.Fatalf("the v2 package must write %s: %v", spec.ResponsesFile, err)
	}
	if err := spec.Write(dir, validPackage()); err != nil {
		t.Fatalf("rewrite as v1: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, spec.ResponsesFile)); !os.IsNotExist(err) {
		t.Fatalf("%s survived a v1 write (err=%v) — the package would still enforce contracts nobody asked for", spec.ResponsesFile, err)
	}
}

// TestTheDocumentAndThePackageMustAgreeOnTheFormat covers both directions of
// the version pre-pass: a version this reader does not know is diagnosed as
// such, and a version that merely disagrees with the connector is refused.
func TestTheDocumentAndThePackageMustAgreeOnTheFormat(t *testing.T) {
	schemas := `"schemas":{"Issue":{"type":"object"}}`

	t.Run("a document from a newer iterion", func(t *testing.T) {
		dir := writeContractPackage(t)
		rewriteResponses(t, dir, `{"schema_version":99,"connector":"probe",`+schemas+`}`)
		err := loadErr(t, dir)
		if err == nil || !strings.Contains(err.Error(), "upgrade iterion") {
			t.Fatalf("err = %v, want the version diagnosis rather than an unknown-field error", err)
		}
	})

	t.Run("a document one version behind its package", func(t *testing.T) {
		dir := writeContractPackage(t)
		rewriteResponses(t, dir, `{"schema_version":1,"connector":"probe",`+schemas+`}`)
		err := loadErr(t, dir)
		if err == nil || !strings.Contains(err.Error(), "schema_version") {
			t.Fatalf("err = %v, want the two documents' versions compared", err)
		}
	})

	t.Run("contracts beside a v1 connector", func(t *testing.T) {
		dir := writeContractPackage(t)
		body, err := os.ReadFile(filepath.Join(dir, spec.ConnectorFile))
		if err != nil {
			t.Fatal(err)
		}
		downgraded := strings.Replace(string(body), "schema_version: 2", "schema_version: 1", 1)
		if downgraded == string(body) {
			t.Fatal("the fixture does not declare schema_version 2 — this case did not run")
		}
		if err := os.WriteFile(filepath.Join(dir, spec.ConnectorFile), []byte(downgraded), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := loadErr(t, dir); err == nil {
			t.Fatal("a v1 connector carrying response contracts must be refused")
		}
	})

	t.Run("another connector's document", func(t *testing.T) {
		dir := writeContractPackage(t)
		rewriteResponses(t, dir, `{"schema_version":2,"connector":"somethingelse",`+schemas+`}`)
		err := loadErr(t, dir)
		if err == nil || !strings.Contains(err.Error(), "somethingelse") {
			t.Fatalf("err = %v, want the mismatch named", err)
		}
	})
}

// TestARepeatedKeyIsRefusedRatherThanSilentlyResolved.
//
// encoding/json keeps the LAST value for a repeated key and reports nothing, so
// a reviewer reading the diff sees both contracts and cannot tell which one the
// engine enforces. For the document that states what a vendor may answer, the
// text reviewed and the rule applied have to be the same thing.
func TestARepeatedKeyIsRefusedRatherThanSilentlyResolved(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		key  string
	}{
		{
			"two contracts under one name",
			`{"schema_version":2,"connector":"probe","schemas":{"Issue":{"type":"object"},"Issue":{"type":"string"}}}`,
			"Issue",
		},
		{
			"two schemas blocks",
			`{"schema_version":2,"connector":"probe","schemas":{"Issue":{"type":"object"}},"schemas":{"Issue":{"type":"string"}}}`,
			"schemas",
		},
		{
			// The SAME value twice on purpose. Two different values are caught
			// by the version comparison below, so that spelling would pass with
			// the duplicate check removed and prove nothing about it.
			"the version key twice, where only the repetition is wrong",
			`{"schema_version":2,"schema_version":2,"connector":"probe","schemas":{"Issue":{"type":"object"}}}`,
			"schema_version",
		},
		{
			"repeated deep inside a contract",
			`{"schema_version":2,"connector":"probe","schemas":{"Issue":{"type":"object","properties":{"id":{"type":"integer"},"id":{"type":"string"}}}}}`,
			"id",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeContractPackage(t)
			rewriteResponses(t, dir, tc.body)
			err := loadErr(t, dir)
			if err == nil {
				t.Fatal("a repeated key was resolved silently")
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("err = %v, want it to name %q", err, tc.key)
			}
		})
	}

	// The other direction: a key repeated in SIBLING objects is ordinary JSON
	// and must keep loading. A check that refused it would reject every real
	// document, since every contract has a "type".
	dir := writeContractPackage(t)
	rewriteResponses(t, dir, `{"schema_version":2,"connector":"probe","schemas":{"Issue":{"type":"object"},"Other":{"type":"object"}}}`)
	if err := loadErr(t, dir); err != nil {
		t.Fatalf("the same key in two sibling objects is not a duplicate: %v", err)
	}
}

func TestAMalformedContractDocumentIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"an unknown field", `{"schema_version":2,"connector":"probe","schemas":{},"extra":1}`},
		{"two JSON documents", `{"schema_version":2,"connector":"probe","schemas":{}}{"schema_version":2}`},
		{"not JSON at all", `schema_version: 2`},
		{"a contract naming a missing reference", `{"schema_version":2,"connector":"probe","schemas":{"Issue":{"ref":"Absent"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeContractPackage(t)
			rewriteResponses(t, dir, tc.body)
			if err := loadErr(t, dir); err == nil {
				t.Fatal("the document was accepted")
			}
		})
	}
}

// TestAnOversizedContractDocumentIsRefusedBeforeItIsParsed keeps a package from
// deciding this process's memory.
func TestAnOversizedContractDocumentIsRefusedBeforeItIsParsed(t *testing.T) {
	dir := writeContractPackage(t)
	padding := strings.Repeat("p", 5<<20)
	rewriteResponses(t, dir, `{"schema_version":2,"connector":"probe","schemas":{"Issue":{"type":"object","ref":"`+padding+`"}}}`)
	err := loadErr(t, dir)
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("err = %v, want the size limit named", err)
	}
}

// NOT TESTED, deliberately, and said out loud: that Write stamps responses.json
// from the PACKAGE's schema_version rather than from ResponseContractsVersion.
//
// Only one contract format exists, so the two expressions evaluate to the same
// 2 and no fixture can tell them apart — a test asserting it would pass with
// either code and certify nothing. It is a construction choice (one source of
// truth per package), not a proven property, until a second version exists. The
// read side's version agreement above IS exercised, and would catch the drift
// at the next bump.
