package connectors_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/ingress"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// A source package's witness: the package a published binary carries loads,
// and a delivery signed the way its VENDOR signs one — by an implementation
// that stands apart from the package's own `signed:` declaration, so a wrong
// template reds this test rather than agreeing with itself — is authenticated,
// located as its trigger, and read into the outputs the package promises.
// The vendor is a double: no test here opens a socket.
//
// The table is the closure: every shipped package that declares a trigger has
// an entry, refused otherwise (TestEveryShippedSourceIsWitnessed), so a source
// cannot ship with its contract unwitnessed.

// vendorDouble reproduces one vendor's signing recipe from the vendor's own
// documentation, independently of the package: given the secret, the instant
// the delivery claims and the raw body, it answers the headers the vendor
// would send.
type vendorDouble struct {
	// signingSecret maps the secret iterion holds onto the key the vendor
	// signs with. Most vendors sign with it directly; a few derive it.
	sign func(secret string, now time.Time, body []byte) map[string]string
	// request adds what the signature does not carry and the vendor always
	// sends (Slack's timestamp header, Linear's `webhookTimestamp`).
	request func(now time.Time, payload map[string]any) map[string]string
	// installation is the value the delivery names for the ingress to match.
	installation string
	// handshake is the body a vendor sends to verify its endpoint, and the
	// value the package must answer with. Nil for a vendor that has none.
	handshake func() (payload map[string]any, want string)
}

// vendorDoubles holds one double per shipped source, registered from the
// file that owns it (connectors/source_<id>_test.go). A map filled by
// registration rather than one literal keeps each vendor's recipe beside its
// witness: adding a source touches its own file and nothing else.
var vendorDoubles = map[string]vendorDouble{}

func registerVendor(id string, d vendorDouble) {
	if _, dup := vendorDoubles[id]; dup {
		panic("two vendor doubles for " + id)
	}
	vendorDoubles[id] = d
}

// TestAShippedSourceReadsASignedDelivery is the positive witness per package.
func TestAShippedSourceReadsASignedDelivery(t *testing.T) {
	for id := range vendorDoubles {
		t.Run(id, func(t *testing.T) {
			p, err := spec.Load(id)
			if err != nil {
				t.Fatalf("load the shipped package %q: %v", id, err)
			}
			if p.Ingress == nil {
				t.Fatalf("%q declares no ingress: a package in the witness table is a SOURCE", id)
			}
			if len(p.Triggers) == 0 {
				t.Fatalf("%q declares no trigger", id)
			}
			witnessed := 0
			for _, trig := range p.Triggers {
				if trig.Sample == "" {
					continue
				}
				witnessed++
				t.Run(trig.ID, func(t *testing.T) {
					body, header, now := signSample(t, id, p, trig)
					d, err := ingress.Read(ingress.Request{
						Ingress: p.Ingress, Triggers: p.Triggers, Secret: []byte(witnessSecret),
						Installation: vendorDoubles[id].installation,
						Body:         body, Header: header, Now: now,
					})
					if err != nil {
						t.Fatalf("read a vendor-signed delivery: %v", err)
					}
					if d.TriggerID != trig.ID {
						t.Fatalf("the delivery read as %q, want %q (event %q)", d.TriggerID, trig.ID, d.EventName)
					}
					if d.Filtered != "" {
						t.Fatalf("the delivery was dropped: %s", d.Filtered)
					}
					var got struct {
						Outputs map[string]any `json:"outputs"`
					}
					if err := json.Unmarshal(d.Event, &got); err != nil {
						t.Fatalf("the event is not JSON: %v", err)
					}
					// Every declared output the sample can fill is present in the
					// event a run would receive.
					for _, o := range trig.Outputs {
						if _, ok := got.Outputs[o.Name]; !ok {
							t.Errorf("output %q is absent from the event: %s", o.Name, d.Event)
						}
					}
					// A body the signature does not cover is refused before any
					// trigger is read.
					header.Set(vendorSignatureHeader(t, p), "00")
					if _, err := ingress.Read(ingress.Request{
						Ingress: p.Ingress, Triggers: p.Triggers, Secret: []byte(witnessSecret),
						Installation: vendorDoubles[id].installation,
						Body:         body, Header: header, Now: now,
					}); !errors.Is(err, ingress.ErrUnauthenticated) {
						t.Fatalf("a forged signature answered %v, want ErrUnauthenticated", err)
					}
				})
			}
			if witnessed == 0 {
				t.Fatalf("%q ships no trigger with a sample: its contract is unwitnessed", id)
			}
		})
	}
}

// TestAShippedSourceAnswersItsHandshake: a vendor that verifies its endpoint
// before sending anything real gets the answer its package promises. The
// challenge is signed like any delivery — an unsigned handshake would be an
// endpoint anyone could make this deployment echo.
func TestAShippedSourceAnswersItsHandshake(t *testing.T) {
	for id, d := range vendorDoubles {
		if d.handshake == nil {
			continue
		}
		t.Run(id, func(t *testing.T) {
			p, err := spec.Load(id)
			if err != nil {
				t.Fatalf("load %q: %v", id, err)
			}
			if p.Ingress == nil || p.Ingress.Handshake == nil {
				t.Fatalf("%q has a handshake double but declares no handshake", id)
			}
			payload, want := d.handshake()
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("encode the handshake body: %v", err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			header := http.Header{}
			for k, v := range d.sign(witnessSecret, now, body) {
				header.Set(k, v)
			}
			got, err := ingress.Read(ingress.Request{
				Ingress: p.Ingress, Triggers: p.Triggers, Secret: []byte(witnessSecret),
				Installation: d.installation, Body: body, Header: header, Now: now,
			})
			if err != nil {
				t.Fatalf("read the handshake: %v", err)
			}
			if got.Handshake == nil || *got.Handshake != want {
				t.Fatalf("the handshake answered %v, want %q", got.Handshake, want)
			}
			if got.TriggerID != "" {
				t.Fatalf("the handshake was read as trigger %q: a verification routes no event", got.TriggerID)
			}
		})
	}
}

// TestEveryShippedSourceIsWitnessed closes the table: a shipped package that
// declares triggers is a source, and one without a vendor double would ship a
// verification contract nothing ever signs for.
func TestEveryShippedSourceIsWitnessed(t *testing.T) {
	for _, id := range shippedPackages(t) {
		p, err := spec.Load(id)
		if err != nil {
			t.Fatalf("load %q: %v", id, err)
		}
		if len(p.Triggers) == 0 {
			continue
		}
		if _, ok := vendorDoubles[id]; !ok {
			t.Errorf("the shipped source %q has no vendor double: add one to vendorDoubles in connectors/sources_test.go", id)
		}
	}
}

// witnessSecret is the plaintext a minted secret would hold: the lane only
// ever compares against it.
const witnessSecret = "s3cr3t-witness"

// signSample rebuilds the delivery body from the trigger's recorded sample and
// signs it with the vendor's recipe.
func signSample(t *testing.T, id string, p *spec.Package, trig spec.Trigger) (body []byte, header http.Header, now time.Time) {
	t.Helper()
	var s spec.SampleDelivery
	if err := json.Unmarshal(trig.SampleBody, &s); err != nil {
		t.Fatalf("%s sample %q: %v", id, trig.Sample, err)
	}
	if s.Payload == nil {
		t.Fatalf("%s sample %q has no payload", id, trig.Sample)
	}
	body, err := json.Marshal(s.Payload)
	if err != nil {
		t.Fatalf("%s: encode the delivery body: %v", id, err)
	}
	header = http.Header{}
	for k, v := range s.Headers {
		header.Set(k, v)
	}
	now = time.Now().UTC().Truncate(time.Second)
	d := vendorDoubles[id]
	for k, v := range d.sign(witnessSecret, now, body) {
		header.Set(k, v)
	}
	if d.request != nil {
		for k, v := range d.request(now, s.Payload) {
			header.Set(k, v)
		}
	}
	return body, header, now
}

// vendorSignatureHeader names the header the package's ingress reads its
// signature from, so the forgery case tampers with the value the lane looks at.
func vendorSignatureHeader(t *testing.T, p *spec.Package) string {
	t.Helper()
	if p.Ingress.Credential.In != "header" || p.Ingress.Credential.Name == "" {
		t.Fatalf("the package's signature does not travel in a header: %+v", p.Ingress.Credential)
	}
	return p.Ingress.Credential.Name
}

// TestTheSampleFilesAreTheOnesThePackageLoads keeps a shipped sample and the
// trigger's declaration in one place: an edit to one that forgets the other is
// the drift this reads out of the tree rather than out of a copy.
func TestTheSampleFilesAreTheOnesThePackageLoads(t *testing.T) {
	for _, id := range shippedPackages(t) {
		p, err := spec.Load(id)
		if err != nil {
			t.Fatalf("load %q: %v", id, err)
		}
		for _, trig := range p.Triggers {
			if trig.Sample == "" {
				continue
			}
			onDisk, err := os.ReadFile(filepath.Join(id, trig.Sample))
			if err != nil {
				t.Errorf("%s: read %q: %v", id, trig.Sample, err)
				continue
			}
			var a, b any
			if err := json.Unmarshal(onDisk, &a); err != nil {
				t.Errorf("%s: %q on disk is not JSON: %v", id, trig.Sample, err)
				continue
			}
			if err := json.Unmarshal(trig.SampleBody, &b); err != nil {
				t.Errorf("%s: the loaded sample %q is not JSON: %v", id, trig.Sample, err)
				continue
			}
			if aj, _ := json.Marshal(a); string(aj) != string(mustJSON(t, b)) {
				t.Errorf("%s: the sample the package carries differs from %q on disk", id, trig.Sample)
			}
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}
