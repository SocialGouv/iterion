package secretguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// TestRegisterCarriesTheReplacedEgressScope is the rotation-scope witness
// of #2756: a seam that swaps a value must not widen what the value it
// replaced carried. A `hosts:`-scoped credential rotates mid-run; the
// fresh value registers under the SAME scope — allow-any here would leave
// the one live credential of the run invisible to the Layer-2 egress gate
// while its dead predecessor stayed blocked. Red when carryReplaced stops
// transporting Hosts.
func TestRegisterCarriesTheReplacedEgressScope(t *testing.T) {
	launch, rotated := randomToken(t), randomToken(t)
	g := New([]Secret{{
		Name:       "forge_publish_token",
		Value:      launch,
		RedactOnly: true,
		EnvOnly:    true,
		Hosts:      []string{"forge.example.com"},
	}}, DefaultConfig())
	if !g.ExfiltratesTo("body="+launch, "evil.example") {
		t.Fatal("scenario broken: the launch value is not gated toward a non-approved host")
	}
	if g.ExfiltratesTo("body="+launch, "forge.example.com") {
		t.Fatal("scenario broken: the launch value is gated toward its OWN approved host")
	}

	g.Register(Secret{Name: "forge_publish_token", Value: rotated, RedactOnly: true})
	if !g.ContainsSecret(rotated) {
		t.Fatal("scenario broken: the rotated value is not registered")
	}
	if !g.ExfiltratesTo("body="+rotated, "evil.example") {
		t.Fatal("the rotated credential is allow-any at the egress gate — the live value of the run lost the scope of the value it replaced")
	}
	if g.ExfiltratesTo("body="+rotated, "forge.example.com") {
		t.Fatal("the rotated credential is gated toward its own approved host — the scope was narrowed, not carried")
	}
	// The env-only class carries too: the rotated grant resolves in the
	// authored-shell channel alone, like the launch grant it replaces.
	var envOnly bool
	for _, s := range g.Secrets() {
		if s.Value == rotated {
			envOnly = s.EnvOnly
		}
	}
	if !envOnly {
		t.Fatal("the rotated value lost the env-only class of the grant it replaces")
	}
}

// TestRotatedDeclaredSecretMaterialisesTheLiveValue is the declared-secret
// arm: a rotation of a MATERIALISABLE secret takes its slot over — the
// declared placeholder keeps resolving, to the live value from now on (it
// used to spend the dead launch value forever), the fresh value redacts to
// a placeholder that the agent channel resolves (it used to read back as a
// _redacted__ form that resolves nowhere, so a bot using what it read sent
// a literal placeholder), and the value that was replaced stays scrubbed
// from every sink with a placeholder resolving nowhere. Red when
// carryReplaced stops taking the materialisable slot over, and when the
// remnant stops keeping the replaced value registered.
func TestRotatedDeclaredSecretMaterialisesTheLiveValue(t *testing.T) {
	launch, rotated := randomToken(t), randomToken(t)
	g := New([]Secret{{
		Name:     "forge_token",
		Value:    launch,
		FilePath: "/run/iterion/secrets/forge_token",
		Hosts:    []string{"forge.example.com"},
	}}, DefaultConfig())

	// Before the rotation: what the model reads back resolves to the value.
	seen := g.RedactForModel("Authorization: Bearer " + launch)
	if strings.Contains(seen, launch) {
		t.Fatal("scenario broken: the launch value is not placeholdered for the model")
	}
	if got := g.MaterializeFor(secretsink.Agent, seen); !strings.Contains(got, launch) {
		t.Fatal("scenario broken: the declared placeholder does not resolve before the rotation")
	}

	g.Register(Secret{Name: "forge_token", Value: rotated, RedactOnly: true})

	// The fresh value reads back to the DECLARED placeholder — the one that
	// resolves — and materialises to the live token at the agent sink.
	seen2 := g.RedactForModel("Authorization: Bearer " + rotated)
	if strings.Contains(seen2, rotated) {
		t.Fatalf("scenario broken: the rotated value crossed the model boundary verbatim: %q", seen2)
	}
	if seen2 != seen {
		t.Fatalf("the rotated value redacts to %q, want the declared placeholder %q", seen2, seen)
	}
	if got := g.MaterializeFor(secretsink.Agent, seen2); got != "Authorization: Bearer "+rotated {
		t.Fatalf("the declared placeholder materialises %q, want the live token", got)
	}
	// The egress scope of the declaration gates the fresh value.
	if !g.ExfiltratesTo("body="+rotated, "evil.example") {
		t.Fatal("the rotated declared credential is allow-any at the egress gate")
	}
	// The replaced value is still known — scrubbed everywhere, its
	// placeholder resolving nowhere.
	if !g.ContainsSecret(launch) {
		t.Fatal("the replaced launch value left the guard — a value that was live at launch must stay scrubbed for the rest of the run")
	}
	redactedLaunch := g.RedactForModel("Bearer " + launch)
	if !strings.Contains(redactedLaunch, redactOnlyPlaceholder(PlaceholderForName("forge_token"))) {
		t.Fatalf("the replaced value redacts to %q, want the _redacted__ placeholder", redactedLaunch)
	}
	if got := g.MaterializeFor(secretsink.Agent, redactedLaunch); strings.Contains(got, launch) {
		t.Fatalf("the replaced value's placeholder resolves at the agent sink: %q", got)
	}
	// The mount survives the takeover: the fresh value is the one the file
	// secret serves.
	if got := g.SecretFilePath("forge_token"); got != "/run/iterion/secrets/forge_token" {
		t.Fatalf("the rotated declared secret lost its file mount: %q", got)
	}
}

// TestRegisterKeepsTheHostFileMaterialisation is the rebuild witness: the
// host materialisation is a runtime resolution (MaterializeHostFiles
// rewrote the file hints to paths it wrote on THIS host), so a later
// registration — any name — must carry it forward instead of reverting the
// hints to the declared mount path, which on a local host does not exist.
// The system prompt announces the hinted path (delegate.SecretFileHints);
// a reverted one sends every file-secret read of the rest of the run to a
// missing file. Red when carryHostPaths stops applying the overlay.
func TestRegisterKeepsTheHostFileMaterialisation(t *testing.T) {
	hostDir := t.TempDir()
	declared := t.TempDir() + "/absent-on-this-host/forge_token"
	g := New([]Secret{{Name: "forge_token", Value: randomToken(t), FilePath: declared}}, DefaultConfig())

	cleanup, err := g.MaterializeHostFiles(hostDir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	host := g.SecretFilePath("forge_token")
	if !strings.HasPrefix(host, hostDir) {
		t.Fatalf("scenario broken: want a host path under %q, got %q", hostDir, host)
	}

	g.Register(Secret{Name: "clone_git_credential", Value: randomToken(t), RedactOnly: true})

	if after := g.SecretFilePath("forge_token"); after != host {
		t.Fatalf("a mid-run Register reverted the host materialisation: %q -> %q", host, after)
	}
	hints := g.SecretFileHints()
	if len(hints) != 1 || hints[0].Path != host {
		t.Fatalf("the file hint reverted with the rebuild: %+v", hints)
	}
}

// TestRegisterWhileMaterializingHostFiles is the convergence witness of the
// host materialisation's swap: it exchanges its rebuilt generation with a
// CompareAndSwap on the generation it read, and restarts on contention — a
// Register landing inside its I/O window must survive the swap (it used to
// be dropped wholesale: the rotated token stayed unknown to redaction),
// and the registration must keep the host paths (the rebuild alone would
// revert them). The green side is interleaving-free: whatever lands where,
// the final state holds both the registration and the host paths. The file
// hints sit below MinLen (a hint costs the write loop one WriteFile and
// the rebuild almost nothing — no matcher entry), so the thousands-strong
// window is wide, not slow, and the registration waits until the loop is
// demonstrably mid-flight: on the old unconditional swap, a registration
// caught inside the window was dropped.
func TestRegisterWhileMaterializingHostFiles(t *testing.T) {
	const rounds = 2
	const files = 20000
	for i := range rounds {
		base := t.TempDir()
		in := make([]Secret, 0, files)
		for j := range files {
			in = append(in, Secret{
				Name:     fmt.Sprintf("f%05d.%d", j, i),
				Value:    "x", // below MinLen: a file hint, no matcher entry
				FilePath: filepath.Join(base, "absent", fmt.Sprintf("f%05d", j)),
			})
		}
		g := New(in, DefaultConfig())
		hostDir := t.TempDir()
		rotated := randomToken(t)

		done := make(chan struct{})
		go func() {
			defer close(done)
			if _, err := g.MaterializeHostFiles(hostDir); err != nil {
				t.Errorf("round %d: MaterializeHostFiles: %v", i, err)
			}
		}()
		// Hold the registration until the write loop is demonstrably
		// mid-flight: its first files exist and its last do not.
		for {
			ents, err := os.ReadDir(hostDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(ents) > 0 && len(ents) < files {
				break
			}
			if len(ents) >= files {
				break // the loop outran us; the assertions still hold on the fix
			}
		}
		g.Register(Secret{Name: "clone_git_credential", Value: rotated, RedactOnly: true})
		<-done

		if !g.ContainsSecret(rotated) {
			t.Fatalf("round %d: the host materialisation's swap dropped a concurrent registration — Redact leaves the rotated token verbatim: %q", i, g.Redact("token="+rotated))
		}
		for _, s := range in {
			if got := g.SecretFilePath(s.Name); !strings.HasPrefix(got, hostDir) {
				t.Fatalf("round %d: secret %s lost its host path after the concurrent registration: %q", i, s.Name, got)
			}
		}
	}
}
