package execenv

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

func TestReadinessStripsOnlyPreambleAndForwardsSameWrite(t *testing.T) {
	t.Parallel()
	for split := 0; split <= len(protocol.Readiness); split++ {
		t.Run(string(rune('A'+split)), func(t *testing.T) {
			var dst bytes.Buffer
			p := newPrefixWriter(&dst, nil, protocol.Invocation{})
			if n, err := p.Write([]byte(protocol.Readiness[:split])); n != split || err != nil {
				t.Fatalf("prefix: %d %v", n, err)
			}
			if split < len(protocol.Readiness) {
				select {
				case <-p.ready:
					t.Fatal("ready before full preamble")
				default:
				}
			}
			tail := []byte(protocol.Readiness[split:] + "business\x00\xff")
			if n, err := p.Write(tail); n != len(tail) || err != nil {
				t.Fatalf("coalesced: %d %v", n, err)
			}
			if dst.String() != "business\x00\xff" {
				t.Fatalf("suffix not forwarded in same Write: %q", dst.String())
			}
			if err := p.result(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-p.ready:
			default:
				t.Fatal("ready not signaled")
			}
			if _, err := p.Write([]byte(protocol.Readiness)); err != nil {
				t.Fatal(err)
			}
			if dst.String() != "business\x00\xff"+protocol.Readiness {
				t.Fatal("second literal marker removed from workload output")
			}
		})
	}
}
func TestReadinessRejectsHelpZeroExitMissingShortAndFuture(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", "help\n", protocol.Readiness[:15], "IENV-READY-V2\x00\x00\x00", "usage: old helper accepts --help\n"} {
		t.Run(v, func(t *testing.T) {
			var dst bytes.Buffer
			calls := 0
			p := newPrefixWriter(&dst, func() { calls++ }, protocol.Invocation{})
			_, _ = p.Write([]byte(v))
			if !errors.Is(p.result(), protocol.ErrTransport) || dst.Len() != 0 {
				t.Fatalf("accepted/leaked unknown stdout: %v %q", p.result(), dst.String())
			}
			select {
			case <-p.ready:
				t.Fatal("invalid ready")
			default:
			}
			if len(v) >= 16 && calls != 1 {
				t.Fatalf("reject callback=%d", calls)
			}
		})
	}
}

type shortWriter struct{}

func (shortWriter) Write(b []byte) (int, error) { return len(b) / 2, nil }
func TestReadinessPreservesOutputFailure(t *testing.T) {
	t.Parallel()
	p := newPrefixWriter(shortWriter{}, nil, protocol.Invocation{})
	b := []byte(protocol.Readiness + "data")
	n, err := p.Write(b)
	if n != 18 || !errors.Is(err, io.ErrShortWrite) || !errors.Is(p.result(), io.ErrShortWrite) {
		t.Fatalf("short output hidden: %d %v %v", n, err, p.result())
	}
}
