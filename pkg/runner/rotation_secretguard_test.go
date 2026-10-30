package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// rotatedTokenFixture builds a rotation-shaped token at random — never a
// repetitive fixture, whose periodicity would exercise different matcher
// paths than a real credential.
func rotatedTokenFixture(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return "rva10c" + hex.EncodeToString(buf)
}

// TestExecutorSpecWiresTheSecretGuardSink is the wiring witness of #2756
// B2, in the spirit of this file's other binder tests: a sink missing here
// is a capability that silently dies on every cloud run — the rotation
// loops would hold a holder nothing ever fills, and a rotated token would
// land verbatim in the run's log.
func TestExecutorSpecWiresTheSecretGuardSink(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: "run-guard-sink", WorkflowName: "wf"}

	holder := &runSecretGuard{}
	spec, _, err := r.executorSpec(context.Background(), msg, &ir.Workflow{}, "", iterlog.Nop(), nil, holder)
	if err != nil {
		t.Fatalf("executorSpec: %v", err)
	}
	if spec.SecretGuardSink == nil {
		t.Fatal("ExecutorSpec.SecretGuardSink not bound — the rotation loops' guard holder stays empty on every cloud run")
	}
	probe := secretguard.New([]secretguard.Secret{{Name: "probe", Value: rotatedTokenFixture(t), RedactOnly: true}}, secretguard.DefaultConfig())
	spec.SecretGuardSink(probe)
	if holder.Load() != probe {
		t.Fatal("the sink did not deliver the guard to the holder")
	}
	// A nil guard (redaction built to nothing) is not stored: there is no
	// guard to register into, and the holder must not read as landed.
	empty := &runSecretGuard{}
	secretGuardSinkOf(empty)(nil)
	if empty.Load() != nil {
		t.Fatal("a nil guard must not fill the holder")
	}
}

// TestBuildExecutorDeliversTheLiveGuardOnTheRunnerPath proves the late
// binding end to end on the runner's own construction path: buildExecutor
// → runview.BuildExecutor → the sink — and that the delivered guard is
// non-nil on the default configuration (heuristic pass on), so a rotation
// loop ticking after the build always finds it.
func TestBuildExecutorDeliversTheLiveGuardOnTheRunnerPath(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: "run-guard-live", WorkflowName: "wf"}

	holder := &runSecretGuard{}
	exec, _, err := r.buildExecutor(context.Background(), msg, &ir.Workflow{}, t.TempDir(), iterlog.Nop(), nil, holder)
	if err != nil {
		t.Fatalf("buildExecutor: %v", err)
	}
	if exec == nil {
		t.Fatal("buildExecutor returned no executor")
	}
	if holder.Load() == nil {
		t.Fatal("the runner path built the executor without delivering the run's guard — a rotation after this point is unredacted")
	}
}

// TestRotatedCloneTokenIsRedactedByTheLiveGuard is the rotation integration
// witness #2756 B3 asks for: the clone token rotates mid-run, the bot
// prints the credential file, and the run's log — through the guard the
// executor wired as its redactor — plus the event payload the hooks scrub
// show the placeholder, never the token. Mutant: drop the registerRotated
// call in refreshGitCredentialsOnce and the verbatim token reddens this
// test.
func TestRotatedCloneTokenIsRedactedByTheLiveGuard(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealerFromBase64("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	mem := &tenantAssertingGenericStore{MemoryGenericSecretStore: secrets.NewMemoryGenericSecretStore(), t: t}
	tctx := store.WithTenant(context.Background(), "team-1")

	tokenV1 := rotatedTokenFixture(t)
	id := secrets.NewGenericSecretID()
	sealed, err := secrets.SealGenericSecret(sealer, id, []byte(tokenV1))
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Create(tctx, secrets.GenericSecret{ID: id, TenantID: "team-1", ScopeTeamID: "team-1", Name: "forge_token", SealedSecret: sealed}); err != nil {
		t.Fatal(err)
	}

	// The run's live guard as the runner path delivers it: built with the
	// launch-time value, handed to the holder through the sink, and wired
	// into the run's logger as THE redaction chokepoint (ADR-149) — the
	// same guard.Redact the event hooks scrub with.
	holder := &runSecretGuard{}
	guard := secretguard.New([]secretguard.Secret{{Name: "forge_token", Value: tokenV1, RedactOnly: true}}, secretguard.DefaultConfig())
	holder.set(guard)
	var logBuf bytes.Buffer
	runLogger := iterlog.New(iterlog.LevelInfo, &logBuf).WithRedactor(guard.Redact)

	r := &Runner{cfg: Config{
		Logger:         iterlog.New(iterlog.LevelError, os.Stderr),
		Sealer:         sealer,
		GenericSecrets: mem,
	}}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".git", gitCredentialFile)
	const repoURL = "https://forgejo.example.test/org/repo.git"

	last := ""
	r.refreshGitCredentialsOnce(context.Background(), "team-1", id, "run-1", path, repoURL, &last, holder)
	if last != tokenV1 {
		t.Fatalf("first tick did not deliver the launch token: last=%q", last)
	}

	// Rotate: the server refresh worker re-mints the store record mid-run.
	tokenV2 := rotatedTokenFixture(t)
	sealed2, err := secrets.SealGenericSecret(sealer, id, []byte(tokenV2))
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Update(tctx, secrets.GenericSecret{ID: id, TenantID: "team-1", ScopeTeamID: "team-1", Name: "forge_token", SealedSecret: sealed2}); err != nil {
		t.Fatal(err)
	}
	r.refreshGitCredentialsOnce(context.Background(), "team-1", id, "run-1", path, repoURL, &last, holder)
	if last != tokenV2 {
		t.Fatalf("rotation did not land: last=%q", last)
	}

	// The bot prints the credential file after the rotation.
	printed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(printed), tokenV2) {
		t.Fatal("scenario broken: the printed file does not carry the rotated token")
	}
	runLogger.Info("bot printed the clone credential file: %s", string(printed))
	eventPayload := guard.Redact(string(printed)) // the event hooks' scrub

	for name, sink := range map[string]string{"run log": logBuf.String(), "event payload": eventPayload} {
		if strings.Contains(sink, tokenV2) {
			t.Errorf("%s carries the rotated token verbatim: %q", name, sink)
		}
		if strings.Contains(sink, tokenV1) {
			t.Errorf("%s carries the launch token verbatim: %q", name, sink)
		}
		if !strings.Contains(sink, "__ITERION_SECRET_") {
			t.Errorf("%s shows no placeholder: %q", name, sink)
		}
	}
}

// TestRotatedFileSecretIsRegisteredOnTheLiveGuard is the `as: file` arm of
// the same witness: refreshFileSecretsOnce registers the rotated value on
// the live guard before rewriting the file.
func TestRotatedFileSecretIsRegisteredOnTheLiveGuard(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealerFromBase64("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	mem := &tenantAssertingGenericStore{MemoryGenericSecretStore: secrets.NewMemoryGenericSecretStore(), t: t}
	tctx := store.WithTenant(context.Background(), "team-1")

	tokenV1 := rotatedTokenFixture(t)
	id := secrets.NewGenericSecretID()
	sealed, err := secrets.SealGenericSecret(sealer, id, []byte(tokenV1))
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Create(tctx, secrets.GenericSecret{ID: id, TenantID: "team-1", ScopeTeamID: "team-1", Name: "forge_token", SealedSecret: sealed}); err != nil {
		t.Fatal(err)
	}

	holder := &runSecretGuard{}
	guard := secretguard.New([]secretguard.Secret{{Name: "forge_token", Value: tokenV1, RedactOnly: true}}, secretguard.DefaultConfig())
	holder.set(guard)

	r := &Runner{cfg: Config{
		Logger:         iterlog.New(iterlog.LevelError, os.Stderr),
		Sealer:         sealer,
		GenericSecrets: mem,
	}}
	path := filepath.Join(t.TempDir(), "forge_token")
	if err := os.WriteFile(path, []byte(tokenV1), 0o600); err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{"forge_token": id}
	files := map[string]string{"forge_token": path}
	last := map[string]string{"forge_token": tokenV1}

	tokenV2 := rotatedTokenFixture(t)
	sealed2, err := secrets.SealGenericSecret(sealer, id, []byte(tokenV2))
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Update(tctx, secrets.GenericSecret{ID: id, TenantID: "team-1", ScopeTeamID: "team-1", Name: "forge_token", SealedSecret: sealed2}); err != nil {
		t.Fatal(err)
	}
	r.refreshFileSecretsOnce(context.Background(), "team-1", refs, files, last, holder)

	redacted := guard.Redact("the file read back: " + tokenV2)
	if strings.Contains(redacted, tokenV2) {
		t.Fatalf("the rotated file secret survived Redact verbatim: %q", redacted)
	}
	if !strings.Contains(redacted, "__ITERION_SECRET_forge_token") {
		t.Fatalf("the rotated file secret redacted to no forge_token placeholder: %q", redacted)
	}
}

// TestRegisterRotatedKeepsTheReplacedEgressScope is the SEAM witness of the
// rotation scope: the holder→Register path a refresh loop actually takes
// must deliver the replaced registration's egress scope to the fresh value
// — a mid-run rotation of a `hosts:`-scoped credential is the live
// credential of the run, and an allow-any registration on it blinds the
// proxy's Layer-2 gate to exactly the token an exfiltration would carry.
// Red when registerRotated stops reaching the inheriting Register.
func TestRegisterRotatedKeepsTheReplacedEgressScope(t *testing.T) {
	launch, rotated := rotatedTokenFixture(t), rotatedTokenFixture(t)
	guard := secretguard.New([]secretguard.Secret{{
		Name:  "forge_token",
		Value: launch,
		Hosts: []string{"forge.example.com"},
	}}, secretguard.DefaultConfig())
	holder := &runSecretGuard{}
	holder.set(guard)

	if !guard.ExfiltratesTo("body="+launch, "evil.example") {
		t.Fatal("scenario broken: the launch value is not gated")
	}
	holder.registerRotated("forge_token", rotated)
	if !guard.ContainsSecret(rotated) {
		t.Fatal("scenario broken: the rotation did not register")
	}
	if !guard.ExfiltratesTo("body="+rotated, "evil.example") {
		t.Fatal("the rotated credential is allow-any at the egress gate — the seam lost the replaced scope")
	}
	if guard.ExfiltratesTo("body="+rotated, "forge.example.com") {
		t.Fatal("the rotated credential is gated toward its own approved host")
	}
	// And the declared placeholder resolves to the live token: a bot that
	// read the credential uses the value it read, not a dead one.
	ph := secretguard.PlaceholderForName("forge_token")
	if got := guard.MaterializeFor(secretsink.Agent, ph); got != rotated {
		t.Fatalf("the declared placeholder materialises %q, want the live token", got)
	}

	// The SECOND rotation of the same name — the normal shape of a long
	// run (a five-minute ticker under a one-hour token) — must find the
	// scope on the live generation, not stop at the redaction-only remnant
	// the first takeover left: otherwise the live value comes back
	// allow-any at the egress gate and the declared placeholder spends the
	// dead intermediate token.
	rotated2 := rotatedTokenFixture(t)
	holder.registerRotated("forge_token", rotated2)
	if !guard.ContainsSecret(rotated2) {
		t.Fatal("scenario broken: the second rotation did not register")
	}
	if !guard.ExfiltratesTo("body="+rotated2, "evil.example") {
		t.Fatal("after the second rotation the live value is allow-any at the egress gate — the seam's chain stopped at the first same-name registration")
	}
	if guard.ExfiltratesTo("body="+rotated2, "forge.example.com") {
		t.Fatal("after the second rotation the live value is gated toward its own approved host")
	}
	if got := guard.MaterializeFor(secretsink.Agent, ph); got != rotated2 {
		t.Fatalf("after the second rotation the declared placeholder materialises %q, want the live token", got)
	}
	// Both dead generations stay scrubbed.
	for _, dead := range []string{launch, rotated} {
		if !guard.ContainsSecret("x " + dead) {
			t.Fatal("a replaced generation left the guard — a value that was live must stay scrubbed for the rest of the run")
		}
	}
}
