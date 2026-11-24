package secretguard

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// randomToken builds a rotation-shaped credential: random (never
// repetitive — a fixture like "aaaa…" would match the periodic-literal
// fast paths and prove nothing), long enough to pass MinLen, with the
// character-class mix ambientSecretGloballySafe asks of a real token.
func randomToken(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return "rva10c-" + hex.EncodeToString(buf)
}

// TestRegisterRedactsTheNewValueInEveryEncoding is the ordinary Register
// case of #2756 B1: a value registered AFTER the guard was built is
// scrubbed like a launch-time one — raw, base64, hex, URL-escaped — and
// the launch-time value keeps matching beside it.
func TestRegisterRedactsTheNewValueInEveryEncoding(t *testing.T) {
	launch := randomToken(t)
	rotated := randomToken(t)
	g := New([]Secret{{Name: "forge_token", Value: launch, RedactOnly: true}}, DefaultConfig())

	if g.ContainsSecret(rotated) {
		t.Fatal("scenario broken: the rotated value matched before registration")
	}
	g.Register(Secret{Name: "forge_token", Value: rotated, RedactOnly: true})

	forms := map[string]string{
		"raw":          rotated,
		"base64":       base64.StdEncoding.EncodeToString([]byte(rotated)),
		"base64-raw":   base64.RawStdEncoding.EncodeToString([]byte(rotated)),
		"hex":          hex.EncodeToString([]byte(rotated)),
		"hex-upper":    strings.ToUpper(hex.EncodeToString([]byte(rotated))),
		"launch value": launch,
	}
	for name, form := range forms {
		out := g.Redact("leaked: " + form + " tail")
		if strings.Contains(out, form) {
			t.Errorf("%s form survived Redact after Register: %q", name, out)
		}
		if !strings.Contains(out, "__ITERION_SECRET_") {
			t.Errorf("%s form redacted to no placeholder: %q", name, out)
		}
	}
}

// TestRegisterDeduplicatesAKnownValue pins the idempotence the rotation
// loops rely on: re-registering an already-known value (a refresher
// re-reading an unchanged record, a retry after partial delivery) builds
// no new generation.
func TestRegisterDeduplicatesAKnownValue(t *testing.T) {
	tok := randomToken(t)
	g := New([]Secret{{Name: "forge_token", Value: tok, RedactOnly: true}}, DefaultConfig())
	before := g.state.Load()
	g.Register(Secret{Name: "forge_token", Value: tok, RedactOnly: true})
	if after := g.state.Load(); after != before {
		t.Fatal("re-registering a known value swapped the state — a refresher tick must be free")
	}
	g.Register(Secret{Value: ""}) // no value: nothing to taint
	if after := g.state.Load(); after != before {
		t.Fatal("registering an empty value swapped the state")
	}
}

// TestRegisterWhileRedacting is the race witness of #2756 B1: readers on
// the redaction hot path cross Register's copy-on-write swap with no lock
// held, so `go test -race` is the judge — a torn read (a matcher of one
// generation indexing the placeholders of another) is a data race by
// construction, and a deadlock the RWMutex shape this replaced.
func TestRegisterWhileRedacting(t *testing.T) {
	g := New([]Secret{{Name: "seed", Value: randomToken(t), RedactOnly: true}}, DefaultConfig())
	const readers = 8
	const registrations = 64

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := range readers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = g.Redact(fmt.Sprintf("reader %d pass %d", i, n))
			}
		}(i)
	}
	registered := make([]string, 0, registrations)
	for range registrations {
		tok := randomToken(t)
		registered = append(registered, tok)
		g.Register(Secret{Name: "rotated", Value: tok, RedactOnly: true})
	}
	close(stop)
	wg.Wait()

	// Every registered value matches in the final generation — the swap
	// carried the whole registration history forward, never a partial one.
	for _, tok := range registered {
		if !g.ContainsSecret(tok) {
			t.Errorf("final state lost a registered value: %q", tok)
		}
	}
	if got := len(g.state.Load().inputs); got != registrations+1 {
		t.Errorf("inputs = %d, want %d (seed + every registration)", got, registrations+1)
	}
}

// TestRegisterOnANilGuardIsANoOp: a nil Guard is the redaction-disabled
// shape, and Register must not bring it to life.
func TestRegisterOnANilGuardIsANoOp(t *testing.T) {
	var g *Guard
	g.Register(Secret{Name: "x", Value: randomToken(t)})
	if g.Redact("anything") != "anything" {
		t.Fatal("nil guard mutated text")
	}
}
