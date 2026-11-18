package connectors_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/ingress"
	"github.com/SocialGouv/iterion/pkg/connector/overlay"
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
	// stamp frames the body the way the vendor does before signing it: the
	// instant a vendor carries IN the body (Linear) and the credential a
	// vendor carries there (Mattermost). Applied before signing and before
	// reading, so the signature covers the bytes the lane verifies.
	stamp func(secret string, now time.Time, body []byte) []byte
	// eventHeader is the header the VENDOR names its event in, for a vendor
	// that locates the event in a header (Forgejo's X-Forgejo-Event, Sentry's
	// Sentry-Hook-Resource). The double writes it from the trigger's own
	// event name, so a package that reads a different header — or a sample
	// that spells the vendor's header wrong — cannot agree with itself.
	eventHeader string
	// eventPath is the body path the VENDOR names its event at, for a vendor
	// that locates it in the body (Forgejo's payload.action is not one: its
	// event is a header). Same rule as eventHeader, one site over.
	eventPath string
	// installation is the value the delivery names for the ingress to match.
	installation string
	// timestamp says the VENDOR sends an instant a replay window can judge —
	// in a header or in the body. A double that knows it proves the package
	// declares the window, since a package that drops it would accept a
	// delivery from any past.
	timestamp bool
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
			p, err := overlay.LoadPackage(id)
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
					// An installation the package binds is what keeps one
					// tenant's deliveries out of another's: a package that
					// names none would accept them all, so a double that
					// knows the vendor's install id proves the binding.
					if d := vendorDoubles[id]; d.installation != "" {
						if p.Ingress.Installation == "" {
							t.Fatalf("%q reads a delivery the vendor names an installation for, but the package binds none — another tenant's delivery would launch here", id)
						}
						if _, err := ingress.Read(ingress.Request{
							Ingress: p.Ingress, Triggers: p.Triggers, Secret: []byte(witnessSecret),
							Installation: "another-" + d.installation,
							Body:         body, Header: header, Now: now,
						}); !errors.Is(err, ingress.ErrUnauthenticated) {
							t.Fatalf("%q accepted a delivery for another installation: %v", id, err)
						}
					}
					// A replay window the package declares must refuse a
					// delivery outside it. The signature stays valid here: the
					// instant is the vendor's own, only the reading clock moved.
					if w := p.Ingress.Timestamp; w != nil {
						tolerance, err := time.ParseDuration(w.Tolerance)
						if err != nil {
							t.Fatalf("%q declares tolerance %q, which is not a duration: %v", id, w.Tolerance, err)
						}
						if _, err := ingress.Read(ingress.Request{
							Ingress: p.Ingress, Triggers: p.Triggers, Secret: []byte(witnessSecret),
							Installation: vendorDoubles[id].installation,
							Body:         body, Header: header, Now: now.Add(2 * tolerance),
						}); !errors.Is(err, ingress.ErrUnauthenticated) {
							t.Fatalf("%q accepted a delivery %s old, outside its %s window: %v", id, 2*tolerance, w.Tolerance, err)
						}
					}
					// The loop guard must DECIDE. The lane drops only on a
					// true, so an expression that renders a value (an id, a
					// name) never drops anything and the package ships a
					// guard that guards nothing.
					if trig.Ignore != "" {
						var payload map[string]any
						if err := json.Unmarshal(body, &payload); err != nil {
							t.Fatalf("%q: the delivery body is not an object: %v", id, err)
						}
						v, err := spec.EvalDelivery(trig.Ignore, spec.NewDeliveryContext(payload, header, p.Ingress.Headers))
						if err != nil {
							t.Fatalf("%q: trigger %s's loop guard fails on its own sample: %v", id, trig.ID, err)
						}
						if _, ok := v.(bool); !ok {
							t.Errorf("%q: trigger %s's loop guard renders %T, not a verdict — the lane drops only on a true", id, trig.ID, v)
						}
					}
					// The credential never reaches the event a run sees.
					if bytes.Contains(d.Event, []byte(witnessSecret)) {
						t.Errorf("the event carries the credential: %s", d.Event)
					}
					// A credential that no longer proves anything is refused
					// before any trigger is read — forged at the site it
					// travels on, which is a header for most vendors and a
					// body field for Mattermost.
					forgedBody, forgedHeader := forgeCredential(t, p, body, header)
					if _, err := ingress.Read(ingress.Request{
						Ingress: p.Ingress, Triggers: p.Triggers, Secret: []byte(witnessSecret),
						Installation: vendorDoubles[id].installation,
						Body:         forgedBody, Header: forgedHeader, Now: now,
					}); !errors.Is(err, ingress.ErrUnauthenticated) {
						t.Fatalf("a forged credential answered %v, want ErrUnauthenticated", err)
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
			p, err := overlay.LoadPackage(id)
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
		p, err := overlay.LoadPackage(id)
		if err != nil {
			t.Fatalf("load %q: %v", id, err)
		}
		if len(p.Triggers) == 0 {
			continue
		}
		d, ok := vendorDoubles[id]
		if !ok {
			t.Errorf("the shipped source %q has no vendor double: register one from connectors/source_%s_test.go", id, id)
			continue
		}
		// A trigger with no recorded delivery is a routing rule, its outputs
		// and its rule against the vendor's shape that nothing ever proves.
		for _, trig := range p.Triggers {
			if trig.Sample == "" {
				t.Errorf("%q: trigger %q ships no sample, so nothing proves what it reads", id, trig.ID)
			}
		}
		// The reverse of the handshake test's skip: a package that answers a
		// vendor's endpoint verification must have a double that asks it.
		// The event a package locates must be the one the vendor actually
		// names: a rename in the package AND its sample would otherwise agree
		// with itself, since the sample is the package's own fixture.
		if d.eventHeader != "" && p.Ingress != nil && p.Ingress.Event.Header != d.eventHeader {
			t.Errorf("%q reads its event from header %q, but the vendor names it in %q", id, p.Ingress.Event.Header, d.eventHeader)
		}
		if d.eventPath != "" && p.Ingress != nil && p.Ingress.Event.From != d.eventPath {
			t.Errorf("%q reads its event from %q, but the vendor names it at %q", id, p.Ingress.Event.From, d.eventPath)
		}
		// A vendor that sends a replay-protectable instant, in a package that
		// declares no window, keeps nothing from being replayed.
		if d.timestamp && p.Ingress != nil && p.Ingress.Timestamp == nil {
			t.Errorf("%q: its vendor sends a timestamp and the package declares no replay window", id)
		}
		if p.Ingress != nil && p.Ingress.Handshake != nil && d.handshake == nil {
			t.Errorf("%q declares a handshake its vendor double never exercises", id)
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
	if d.stamp != nil {
		body = d.stamp(witnessSecret, now, body)
	}
	for k, v := range d.sign(witnessSecret, now, body) {
		header.Set(k, v)
	}
	if d.eventHeader != "" {
		header.Set(d.eventHeader, trig.Event)
	}
	return body, header, now
}

// forgeCredential returns the same delivery with its credential replaced by a
// value that proves nothing, tampered with WHERE THE CREDENTIAL TRAVELS: a
// header for an hmac or a bearer, a body field for a token a vendor posts in
// the body. A forger that only knew headers would leave the body-carried case
// unwitnessed — the very case where the whole proof is a plain body member.
func forgeCredential(t *testing.T, p *spec.Package, body []byte, header http.Header) ([]byte, http.Header) {
	t.Helper()
	c := p.Ingress.Credential
	if c.Name == "" {
		t.Fatalf("the credential names no field: %+v", c)
	}
	switch c.In {
	case "header", "":
		forged := header.Clone()
		forged.Set(c.Name, "00")
		return body, forged
	case "body":
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("the delivery body is not an object: %v", err)
		}
		m[c.Name] = "00"
		forged, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("encode the forged body: %v", err)
		}
		return forged, header.Clone()
	}
	t.Fatalf("no forgery site for credential.in = %q", c.In)
	return nil, nil
}
