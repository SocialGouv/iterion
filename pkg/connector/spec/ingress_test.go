package spec_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// ingressDoc is a Forgejo-shaped triggers.yaml with an ingress, one trigger,
// its outputs, a filter, a loop guard and a sample. Each refusal case below
// replaces exactly one line of it, so each guard is exercised alone.
const ingressDoc = `schema_version: 1
connector: probe
ingress:
  body: json
  credential:
    scheme: hmac-sha256
    in: header
    name: X-Probe-Signature
    encoding: hex
    signed: "{body}"
    secret: minted
  event: { header: X-Probe-Event }
  delivery_id: { header: X-Probe-Delivery }
  headers: [X-Probe-Event, X-Probe-Delivery]
  ack: { status: 200 }
triggers:
  - id: probe.issue.commented
    event: issue_comment
    display_name: Issue commented
    outputs:
      - { name: repo, from: 'payload.repository.full_name' }
      - { name: repo_url, from: 'payload.repository.clone_url' }
      - { name: actor, from: 'payload.sender.login' }
      - { name: issue_number, from: 'payload.issue.number', type: int }
      - { name: labels, from: 'map(payload.issue.labels, l => l.name)', type: list }
      - { name: event_name, from: 'headers["X-Probe-Event"]' }
    filters:
      - { id: action, from: 'payload.action', type: enum, options: [created, edited, deleted], default: [created] }
    ignore: 'payload.sender.login == "iterion-bot"'
    sample: samples/issue-commented.json
`

const ingressSample = `{
  "headers": {"X-Probe-Event": "issue_comment", "X-Probe-Delivery": "d-1"},
  "payload": {
    "action": "created",
    "issue": {"number": 7, "labels": [{"name": "bug"}, {"name": "triage"}]},
    "comment": {"id": 42},
    "repository": {"full_name": "acme/widgets", "clone_url": "https://forge.example/acme/widgets.git"},
    "sender": {"login": "alice"}
  }
}`

// writeIngressPackage writes the probe package with doc as its triggers.yaml
// and sample as its recorded delivery.
func writeIngressPackage(t *testing.T, doc, sample string) string {
	t.Helper()
	dir := writePackageDir(t)
	writeTriggers(t, dir, doc)
	if err := os.MkdirAll(filepath.Join(dir, "samples"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "samples", "issue-commented.json"), []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestAnIngressAndItsSampleLoad is the positive witness: the declarations
// load, the ingress arrives on the Package, and the outputs read the sample
// through the one delivery context the lane and the preview share.
func TestAnIngressAndItsSampleLoad(t *testing.T) {
	p, err := spec.Load(writeIngressPackage(t, ingressDoc, ingressSample))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.Ingress == nil || p.Ingress.Credential.Scheme != spec.SchemeHMACSHA256 {
		t.Fatalf("ingress = %+v, want the declared block", p.Ingress)
	}
	tr := p.Triggers[0]
	if len(tr.Outputs) != 6 || len(tr.Filters) != 1 || tr.SampleBody == nil {
		t.Fatalf("trigger = %+v, want its outputs, filter and loaded sample", tr)
	}
	ctx := spec.NewDeliveryContext(
		map[string]any{"repository": map[string]any{"full_name": "acme/widgets"}},
		http.Header{"X-Probe-Event": {"issue_comment"}, "X-Probe-Signature": {"sig"}},
		p.Ingress.Headers,
	)
	got, err := expr.MustParse(`payload.repository.full_name`).Eval(ctx)
	if err != nil || got != "acme/widgets" {
		t.Fatalf("payload read = %v, %v", got, err)
	}
	if got, _ := expr.MustParse(`headers["X-Probe-Signature"]`).Eval(ctx); got != nil {
		t.Fatalf("an unlisted header must be invisible to expressions, read %v", got)
	}
}

// TestABodyTimestampNeedsNoTimestampPlaceholder pins Linear's contract: the
// timestamp travels inside the signed body, so the signature over {body}
// already authenticates it and the template must not demand {timestamp}.
func TestABodyTimestampNeedsNoTimestampPlaceholder(t *testing.T) {
	doc := strings.Replace(ingressDoc, `  event: { header: X-Probe-Event }`,
		"  timestamp: { in: body, name: webhookTimestamp, unit: ms, tolerance: 1m }\n  event: { header: X-Probe-Event }", 1)
	if _, err := spec.Load(writeIngressPackage(t, doc, ingressSample)); err != nil {
		t.Fatalf("a body timestamp under a {body} signature must load: %v", err)
	}
}

// TestAnIngressDeclarationNoLaneCouldHonourIsRefused: one case per guard,
// each changing one line of a valid document, each asserting its own
// refusal so no guard hides behind another.
func TestAnIngressDeclarationNoLaneCouldHonourIsRefused(t *testing.T) {
	cases := []struct {
		name, old, new, sample, want string
	}{
		{"a signature that does not cover the body", `signed: "{body}"`, `signed: "v0:"`, "", "does not cover {body}"},
		{"an unknown placeholder", `signed: "{body}"`, `signed: "{body}{nonce}"`, "", "only {body} and {timestamp} exist"},
		{"a header timestamp left unsigned", `  event: { header: X-Probe-Event }`,
			"  timestamp: { in: header, name: X-Probe-Timestamp, unit: s, tolerance: 5m }\n  event: { header: X-Probe-Event }", "", "does not cover {timestamp}"},
		{"a timestamp placeholder with no header timestamp", `signed: "{body}"`, `signed: "{timestamp}:{body}"`, "", "no timestamp travels in a header"},
		{"an unknown scheme", `scheme: hmac-sha256`, `scheme: md5`, "", "is never believed"},
		{"no secret origin", `    secret: minted`, ``, "", "secret's origin"},
		{"a listed Authorization header", `headers: [X-Probe-Event, X-Probe-Delivery]`, `headers: [X-Probe-Event, Authorization]`, "", "carries a credential"},
		{"the credential header listed", `headers: [X-Probe-Event, X-Probe-Delivery]`, `headers: [X-Probe-Event, x-probe-signature]`, "", "carries a credential"},
		{"an event located twice", `event: { header: X-Probe-Event }`, `event: { header: X-Probe-Event, from: 'payload.action' }`, "", "one value has one source"},
		{"a non-2xx ack", `ack: { status: 200 }`, `ack: { status: 302 }`, "", "is not a 2xx"},
		{"an output reading the implicit input", `from: 'payload.sender.login' }`, `from: 'sender.login' }`, "", `reads namespace "sender"`},
		{"a reserved output with the wrong type", `from: 'map(payload.issue.labels, l => l.name)', type: list }`, `from: 'payload.action' }`, "", `"labels" is reserved as a list`},
		{"a duplicate output", `- { name: event_name,`, `- { name: actor,`, "", "duplicate output"},
		{"an enum default outside its options", `default: [created]`, `default: [opened]`, "", "is not an option"},
		{"a selector naming no operation", `default: [created] }`, `default: [created], selector: probe.nothing.list }`, "", "is not an operation of this package"},
		{"a sample of another event", "", "", strings.Replace(ingressSample, `"X-Probe-Event": "issue_comment"`, `"X-Probe-Event": "push"`, 1), `the sample's event is "push"`},
		{"a sample the trigger's own guard drops", "", "", strings.Replace(ingressSample, `"login": "alice"`, `"login": "iterion-bot"`, 1), "drops its sample"},
		{"a sample whose output has the wrong type", "", "", strings.Replace(ingressSample, `"number": 7`, `"number": "7"`, 1), `output "issue_number" reads string`},
		{"a sample path leaving the package", `sample: samples/issue-commented.json`, `sample: samples/../connector.yaml`, "", "is not a samples/<name>.json path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := ingressDoc
			if c.old != "" {
				if !strings.Contains(doc, c.old) {
					t.Fatalf("fixture drift: %q is not in the document", c.old)
				}
				doc = strings.Replace(doc, c.old, c.new, 1)
			}
			sample := ingressSample
			if c.sample != "" {
				sample = c.sample
			}
			_, err := spec.Load(writeIngressPackage(t, doc, sample))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal containing %q, got %v", c.want, err)
			}
		})
	}
}

// TestTriggerDeclarationsWithoutAnIngressAreRefused: outputs, filters, a
// loop guard or a sample are read by the ingress lane only, so declaring them
// with no ingress block is a declaration nothing reads.
func TestTriggerDeclarationsWithoutAnIngressAreRefused(t *testing.T) {
	doc := `schema_version: 1
connector: probe
triggers:
  - id: probe.issue.commented
    event: issue_comment
    outputs:
      - { name: repo, from: 'payload.repository.full_name' }
`
	dir := writePackageDir(t)
	writeTriggers(t, dir, doc)
	_, err := spec.Load(dir)
	if err == nil || !strings.Contains(err.Error(), "declares no ingress: block") {
		t.Fatalf("want the no-ingress refusal, got %v", err)
	}
}
