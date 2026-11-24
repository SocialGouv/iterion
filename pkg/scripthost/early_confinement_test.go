package scripthost

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTheEarlyConfinementDocumentIsKeptAndPassedOver: a Proto-6 child writes
// its confinement before the script runs. The parent keeps it, and the
// verdict after it is still the verdict; any other non-verdict document is
// still not a response.
func TestTheEarlyConfinementDocumentIsKeptAndPassedOver(t *testing.T) {
	t.Parallel()
	early := `{"confinement":{"cpu_seconds":2,"all_threads":true}}` + "\n"
	verdict := `{"value":"42","confinement":{"cpu_seconds":2,"all_threads":true}}` + "\n"

	if got := earlyConfinement([]byte(early)); got == nil || got.CPUSeconds != 2 || !got.AllThreads {
		t.Errorf("a child killed after its early document: confinement %+v, want cpu_seconds 2", got)
	}
	if resp, ok := decodeResponse([]byte(early)); ok {
		t.Errorf("the early document alone was read as a verdict: %+v", resp)
	}
	if resp, ok := decodeResponse([]byte(early + verdict)); !ok || resp.Value != "42" {
		t.Errorf("the verdict after the early document = %+v, %v; want value 42", resp, ok)
	}
	if got := earlyConfinement([]byte(verdict)); got != nil {
		t.Errorf("an older child's single verdict was read as an early document: %+v", got)
	}
	if resp, ok := decodeResponse([]byte("{}\n" + verdict)); ok {
		t.Errorf("an empty document before the verdict was passed over: %+v", resp)
	}
}

// TestADeathNamesTheCPUBoundTheChildEnforced: a deployment's own CPU limit,
// stricter than the one asked, is the number a death is read against, and
// the refusal says which one it is — still the ambiguous verdict, since a
// kill ON an inherited hard limit is not attributable from the wait status.
func TestADeathNamesTheCPUBoundTheChildEnforced(t *testing.T) {
	t.Parallel()
	evidence := deathEvidence{
		limits:    Limits{Memory: 256 << 20, CPU: 10 * time.Second},
		signalled: true, signal: 9,
		burned:  1990 * time.Millisecond,
		probe:   probeResult{samples: 4},
		applied: &Applied{CPUSeconds: 2},
	}
	resp, err := deathToRefusal(evidence)
	if err != nil {
		t.Fatalf("want a refusal, got %v", err)
	}
	if resp.Refusal != RefusalHostDied || !strings.Contains(resp.Message, "against a 2s bound") || !strings.Contains(resp.Message, "deployment's own limit") {
		t.Errorf("refusal %s: %s — want the 2s bound the child enforced named", resp.Refusal, resp.Message)
	}
	evidence.applied = nil
	if resp, _ := deathToRefusal(evidence); !strings.Contains(resp.Message, "against a 10s bound") {
		t.Errorf("without the child's account the asked bound is named: %s", resp.Message)
	}
}

// writeStub writes an executable stub child without this process ever
// holding a write descriptor on it: a parallel test's fork would inherit one
// until its exec, and a file open for writing refuses exec with ETXTBSY.
func writeStub(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "child.sh")
	out, err := exec.Command("/bin/sh", "-c", `printf '%s' "$1" > "$2" && chmod 700 "$2"`, "sh", script, bin).CombinedOutput()
	if err != nil {
		t.Fatalf("writing the stub child %s: %v: %s", bin, err, out)
	}
	return bin
}

// TestAVerdictAtItsBoundIsReadBehindTheEarlyDocument: the stdout cap is the
// verdict's bound plus the early document's own, so the early document costs
// the verdict nothing. The verdict here is within 41 bytes of the verdict's
// share of the cap — a value under ResultBytes whose every byte JSON escapes
// to two — and the early document sits before it.
func TestAVerdictAtItsBoundIsReadBehindTheEarlyDocument(t *testing.T) {
	t.Parallel()
	limits := Limits{ResultBytes: 64 << 10}
	clamped, _ := limits.Clamp()
	verdictShare := int(clamped.ResultBytes) + stdoutSlack
	value := strings.Repeat(`"`, (verdictShare-53)/2)
	verdict, err := json.Marshal(Response{Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(verdict) + 1; n > verdictShare || n < verdictShare-64 || int64(len(value)) > clamped.ResultBytes {
		t.Fatalf("the fixture's verdict is %d bytes for a %d-byte share and a %d-byte value: not the case under test", n, verdictShare, len(value))
	}
	early, err := json.Marshal(Response{Confinement: &Applied{LandlockABI: 4, SocketsDenied: true, AllThreads: true, MemoryBytes: 1 << 30, CPUSeconds: 10, MemoryBaseBytes: 90 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	earlyFile, verdictFile := filepath.Join(dir, "early.json"), filepath.Join(dir, "verdict.json")
	if err := os.WriteFile(earlyFile, append(early, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(verdictFile, append(verdict, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	host := Host{Binary: writeStub(t, "#!/bin/sh\n/bin/cat >/dev/null\n/bin/cat '"+earlyFile+"' '"+verdictFile+"'\n")}
	resp, err := host.Run(context.Background(), "1", limits)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !resp.OK() || resp.Value != value {
		t.Fatalf("a verdict within its bound came back %s: %.200s", resp.Refusal, resp.Message)
	}
	if resp.Confinement == nil || resp.Confinement.CPUSeconds != 10 {
		t.Errorf("the early document's confinement = %+v, want it kept", resp.Confinement)
	}
}

// TestOnlyOneBoundedEarlyDocumentIsPassedOver: the early document is the
// first one, alone, and within its bound; a second confinement-only
// document, or an oversized first one, is a protocol error and fails closed.
func TestOnlyOneBoundedEarlyDocumentIsPassedOver(t *testing.T) {
	t.Parallel()
	early := `{"confinement":{"cpu_seconds":2}}` + "\n"
	verdict := `{"value":"42"}` + "\n"
	if resp, ok := decodeResponse([]byte(early + early + verdict)); ok {
		t.Errorf("a second early document was passed over: %+v", resp)
	}
	padded := `{"confinement":{"cpu_seconds":2}` + strings.Repeat(" ", maxEarlyDocument) + "}\n"
	if resp, ok := decodeResponse([]byte(padded + verdict)); ok {
		t.Errorf("an early document past its %d-byte bound was passed over: %+v", maxEarlyDocument, resp)
	}
	if got := earlyConfinement([]byte(padded)); got != nil {
		t.Errorf("an early document past its bound was kept: %+v", got)
	}
}
