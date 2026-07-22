package secretguard

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// A realistic-length fake secret (40 chars) so encodings are long
	// and unique enough to taint safely.
	fakeKey = "sk-ant-FAKE-abcDEF0123456789ghiJKLmnoPQRස" // includes a non-ASCII rune on purpose
	awsKey  = "AKIAIOSFODNN7EXAMPLE"                      // canonical AWS example access key
)

func newTestGuard(t *testing.T, secrets ...Secret) *Guard {
	t.Helper()
	return New(secrets, DefaultConfig())
}

func TestEncodingsOf_CoversFormats(t *testing.T) {
	v := "MyS3cretValue-0123456789"
	encs := encodingsOf(v)
	want := map[string]string{
		"raw":        v,
		"base64 std": base64.StdEncoding.EncodeToString([]byte(v)),
		"base64 url": base64.URLEncoding.EncodeToString([]byte(v)),
		"hex lower":  hex.EncodeToString([]byte(v)),
		"hex upper":  strings.ToUpper(hex.EncodeToString([]byte(v))),
		"url query":  url.QueryEscape(v),
	}
	set := make(map[string]struct{}, len(encs))
	for _, e := range encs {
		set[e] = struct{}{}
	}
	for label, form := range want {
		if _, ok := set[form]; !ok {
			t.Errorf("encodingsOf missing %s form %q", label, form)
		}
	}
}

func TestRedact_KnownValueAllEncodings(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "api_key", Value: fakeKey})
	ph := defaultPlaceholder("api_key")

	cases := map[string]string{
		"raw":        fakeKey,
		"base64 std": base64.StdEncoding.EncodeToString([]byte(fakeKey)),
		"base64 raw": base64.RawStdEncoding.EncodeToString([]byte(fakeKey)),
		"base64 url": base64.URLEncoding.EncodeToString([]byte(fakeKey)),
		"hex":        hex.EncodeToString([]byte(fakeKey)),
		"hex upper":  strings.ToUpper(hex.EncodeToString([]byte(fakeKey))),
		"url query":  url.QueryEscape(fakeKey),
	}
	for label, encoded := range cases {
		in := "prefix " + encoded + " suffix"
		got := g.Redact(in)
		if strings.Contains(got, encoded) && encoded != ph {
			t.Errorf("%s: secret still present after Redact: %q", label, got)
		}
		if !strings.Contains(got, ph) {
			t.Errorf("%s: expected placeholder %q in %q", label, ph, got)
		}
	}
}

func TestRedact_JSONEscapedValue(t *testing.T) {
	// A value with characters that JSON escapes, embedded inside a JSON
	// document the way it would appear in events.jsonl.
	v := `line1
"quoted"\back`
	g := newTestGuard(t, Secret{Name: "tok", Value: v})
	doc := `{"field":"` + jsonEscape(v) + `"}`
	got := g.Redact(doc)
	if strings.Contains(got, jsonEscape(v)) {
		t.Errorf("json-escaped secret survived: %q", got)
	}
	if !strings.Contains(got, defaultPlaceholder("tok")) {
		t.Errorf("expected placeholder in %q", got)
	}
}

func jsonEscape(v string) string {
	// mirror encodingsOf's json form
	for _, e := range encodingsOf(v) {
		if e != v && strings.Contains(e, `\`) {
			return e
		}
	}
	return v
}

func TestMaterialize_RoundTrip(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "deploy_key", Value: fakeKey})
	ph := defaultPlaceholder("deploy_key")
	cmd := `curl -H "Authorization: Bearer ` + ph + `" https://api.example.com`
	got := g.Materialize(cmd)
	if !strings.Contains(got, fakeKey) {
		t.Errorf("Materialize did not substitute real value: %q", got)
	}
	if strings.Contains(got, ph) {
		t.Errorf("placeholder survived Materialize: %q", got)
	}
	// Redact is the inverse on the materialised text.
	if back := g.Redact(got); !strings.Contains(back, ph) || strings.Contains(back, fakeKey) {
		t.Errorf("Redact did not invert Materialize: %q", back)
	}
}

// TestMaterializeShell_SingleQuoteInjection guards the fix for the deepsec
// finding (run 019f02f4): the tool-node template wraps a secret placeholder in
// single quotes, and plain Materialize substituted the RAW value, so a secret
// value containing a single quote broke out of the quoting -> shell injection.
// MaterializeShell escapes the value for inside-single-quote use so the
// surrounding quotes stay balanced and the value is inert shell text.
func TestMaterializeShell_SingleQuoteInjection(t *testing.T) {
	// A hostile secret value that, raw, would close the quote and run `id`.
	const evil = `x'; id #`
	g := newTestGuard(t, Secret{Name: "tok", Value: evil})
	ph := defaultPlaceholder("tok")
	// The template layer emits the placeholder inside single quotes.
	cmd := `deploy --token '` + ph + `'`

	raw := g.Materialize(cmd)
	if !strings.Contains(raw, `'; id #`) {
		t.Fatalf("precondition: raw Materialize should embed the unescaped value: %q", raw)
	}

	got := g.MaterializeShell(cmd)
	// Every single quote in the VALUE must be the escaped form ('\''), so no
	// bare `'` from the value can terminate the surrounding quote. `sh -c got`
	// then passes the literal value `x'; id #` as one arg — `id` never runs.
	want := `deploy --token 'x'\''; id #'`
	if got != want {
		t.Errorf("MaterializeShell = %q; want %q", got, want)
	}
	if strings.Contains(got, ph) {
		t.Errorf("placeholder survived MaterializeShell: %q", got)
	}
}

// TestMaterializeShellEnv_KeepsSecretOutOfCommandText guards the fix for
// the "secret value in subprocess argv" finding: inlining a materialised
// secret INTO the exec'd command string makes it visible via ps /
// /proc/<pid>/cmdline to any co-resident local process for the
// subprocess's lifetime. MaterializeShellEnv instead swaps the quoted
// placeholder for an env-var reference and returns the real value
// out-of-band, so the command text itself never carries the plaintext
// secret — regardless of what characters the value contains, since
// double-quoted parameter expansion does not re-parse the substituted
// value for shell metacharacters.
func TestMaterializeShellEnv_KeepsSecretOutOfCommandText(t *testing.T) {
	const evil = `x'; id # $(whoami) "quoted"`
	g := newTestGuard(t, Secret{Name: "tok", Value: evil})
	ph := defaultPlaceholder("tok")
	cmd := `deploy --token '` + ph + `'`

	gotCmd, env := g.MaterializeShellEnv(cmd)

	if strings.Contains(gotCmd, evil) {
		t.Fatalf("secret value leaked into command text: %q", gotCmd)
	}
	// The placeholder NAME is expected to survive — it doubles as the
	// env-var name in the "$NAME" reference. What must NOT survive is
	// its single-quoted (inline-value) form.
	if strings.Contains(gotCmd, "'"+ph+"'") {
		t.Errorf("quoted placeholder was not converted to an env reference: %q", gotCmd)
	}
	want := `deploy --token "$` + ph + `"`
	if gotCmd != want {
		t.Errorf("gotCmd = %q, want %q", gotCmd, want)
	}
	if env[ph] != evil {
		t.Errorf("env[%q] = %q, want raw value %q", ph, env[ph], evil)
	}
}

// TestMaterializeShellEnv_MultipleSecrets verifies every quoted
// placeholder present in the command is swapped, each into its own env
// entry.
func TestMaterializeShellEnv_MultipleSecrets(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "user", Value: "alice"}, Secret{Name: "pass", Value: "s3cr3t!"})
	userPh, passPh := defaultPlaceholder("user"), defaultPlaceholder("pass")
	cmd := `login --user '` + userPh + `' --pass '` + passPh + `'`

	gotCmd, env := g.MaterializeShellEnv(cmd)

	if strings.Contains(gotCmd, "alice") || strings.Contains(gotCmd, "s3cr3t!") {
		t.Fatalf("secret values leaked into command text: %q", gotCmd)
	}
	want := `login --user "$` + userPh + `" --pass "$` + passPh + `"`
	if gotCmd != want {
		t.Errorf("gotCmd = %q, want %q", gotCmd, want)
	}
	if env[userPh] != "alice" || env[passPh] != "s3cr3t!" {
		t.Errorf("env map incomplete: %#v", env)
	}
}

// TestMaterializeShellEnv_MixedQuotedAndBareFallsBackToInline covers the
// unusual case where the SAME secret is referenced both as
// {{secrets.X}} (quoted) and {{!secrets.X}} (bare) within one command:
// converting only the quoted occurrence to an env-reference while
// leaving the bare one for a later blind substitution would corrupt the
// just-inserted "$PLACEHOLDER" token (the placeholder name is a
// substring of it), so this case conservatively falls back to inline
// materialisation for every occurrence of that placeholder — matching
// MaterializeShell's existing, still-safe (just not argv-hidden) behaviour.
func TestMaterializeShellEnv_MixedQuotedAndBareFallsBackToInline(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "tok", Value: "plain-value"})
	ph := defaultPlaceholder("tok")
	cmd := `echo '` + ph + `' ` + ph

	gotCmd, env := g.MaterializeShellEnv(cmd)

	want := `echo 'plain-value' plain-value`
	if gotCmd != want {
		t.Errorf("gotCmd = %q, want %q", gotCmd, want)
	}
	if len(env) != 0 {
		t.Errorf("expected no env entries for the mixed-usage fallback, got %#v", env)
	}
}

// TestMaterializeShellEnv_UnquotedFallsBackToInline covers the
// {{!secrets.X}} raw/bang form: the template layer never wraps that
// placeholder in single quotes, so env-indirection has no quoted token to
// swap and must fall back to plain MaterializeShell's inline (escaped)
// substitution rather than silently leaving the placeholder unresolved.
func TestMaterializeShellEnv_UnquotedFallsBackToInline(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "tok", Value: "plain-value"})
	ph := defaultPlaceholder("tok")
	cmd := "echo " + ph // no surrounding quotes — the bang/raw form's shape

	gotCmd, env := g.MaterializeShellEnv(cmd)

	if gotCmd != "echo plain-value" {
		t.Errorf("expected inline fallback, got %q", gotCmd)
	}
	if len(env) != 0 {
		t.Errorf("expected no env entries for the inline fallback path, got %#v", env)
	}
}

func TestContainsSecret_DeterministicGate(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "k", Value: fakeKey})
	if !g.ContainsSecret("payload=" + fakeKey) {
		t.Error("ContainsSecret should match raw value")
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(fakeKey))
	if !g.ContainsSecret("blob:" + b64) {
		t.Error("ContainsSecret should match base64 value")
	}
	if g.ContainsSecret("nothing sensitive here, just words and 12345") {
		t.Error("ContainsSecret false positive on benign text")
	}
	// The gate must NOT fire on a heuristic-only token (unknown AWS key).
	if g.ContainsSecret("env AWS_KEY=" + awsKey) {
		t.Error("ContainsSecret must not fire on heuristic-only (unregistered) tokens")
	}
}

func TestFileSecretReferenceRendersPathAndRegistersValue(t *testing.T) {
	g := newTestGuard(t, Secret{
		Name:     "kubeconfig",
		Value:    fakeKey,
		FilePath: "/run/iterion/secrets/kubeconfig",
		Env:      "KUBECONFIG",
	})
	if got := g.ResolveSecretRef("kubeconfig"); got != "/run/iterion/secrets/kubeconfig" {
		t.Fatalf("ResolveSecretRef(file) = %q", got)
	}
	if !g.ContainsSecret("payload=" + fakeKey) {
		t.Fatal("file secret value should remain registered for DLP/redaction")
	}
	hints := g.SecretFileHints()
	if len(hints) != 1 || hints[0].Path != "/run/iterion/secrets/kubeconfig" || hints[0].Env != "KUBECONFIG" {
		t.Fatalf("file hints not preserved: %+v", hints)
	}
}

// TestMaterializeHostFiles_RewritesPathAndWritesValue guards the host
// materialisation seam: on a non-sandbox run, MaterializeHostFiles writes
// each file secret's plaintext to dir/<sanitized-name> (0600) and
// rewrites ResolveSecretRef + SecretFileHints to the HOST path so
// {{secrets.X.path}} resolves to a real file.
func TestMaterializeHostFiles_RewritesPathAndWritesValue(t *testing.T) {
	const payload = "webhook-content-abcdef"
	g := newTestGuard(t, Secret{
		Name:     "webhooks.json",
		Value:    payload,
		FilePath: "/run/iterion/secrets/webhooks.json",
		Env:      "WEBHOOKS_FILE",
	})
	dir := t.TempDir()
	cleanup, err := g.MaterializeHostFiles(dir)
	if err != nil {
		t.Fatalf("MaterializeHostFiles: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup should not be nil")
	}

	wantPath := filepath.Join(dir, "webhooks.json")
	if got := g.ResolveSecretRef("webhooks.json"); got != wantPath {
		t.Errorf("ResolveSecretRef after materialise = %q, want %q", got, wantPath)
	}
	hints := g.SecretFileHints()
	if len(hints) != 1 || hints[0].Path != wantPath {
		t.Fatalf("hints not rewritten: %+v", hints)
	}

	info, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("stat host secret file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("host secret file perms = %v, want 0600", perm)
	}
	b, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read host secret file: %v", err)
	}
	if string(b) != payload {
		t.Errorf("host secret content = %q, want %q", string(b), payload)
	}

	cleanup()
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove host secret file: %v", err)
	}
}

// TestMaterializeHostFiles_PrefersRunnerMaterializedMountPath pins the
// fix for the live prod 401 (run 019f8861): on an unsandboxed cloud run
// the runner materialises each file secret at its DECLARED mount path
// and keeps that file LIVE via its mid-run refresh loop — but this seam
// used to snapshot the launch value into a per-run tempdir and re-point
// the agent-facing hint there, so the agent read a frozen token no
// refresher ever touched. When a file already exists at the declared
// path, the hint must keep pointing at it, and a subsequent rotation
// (the runner's atomic rewrite) must be visible through the hinted path.
func TestMaterializeHostFiles_PrefersRunnerMaterializedMountPath(t *testing.T) {
	mountDir := t.TempDir()
	mountPath := filepath.Join(mountDir, "forge_token")
	// The runner's launch-time materialisation.
	if err := os.WriteFile(mountPath, []byte("launch-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := newTestGuard(t, Secret{
		Name:     "forge_token",
		Value:    "launch-token-value-long-enough",
		FilePath: mountPath,
	})
	cleanup, err := g.MaterializeHostFiles(t.TempDir())
	if err != nil {
		t.Fatalf("MaterializeHostFiles: %v", err)
	}
	defer cleanup()

	if got := g.ResolveSecretRef("forge_token"); got != mountPath {
		t.Errorf("ResolveSecretRef = %q, want the runner-materialised mount path %q", got, mountPath)
	}
	hints := g.SecretFileHints()
	if len(hints) != 1 || hints[0].Path != mountPath {
		t.Fatalf("hint re-pointed away from the refreshed mount path: %+v", hints)
	}

	// The runner's mid-run refresh rewrites the mount-path file; the agent
	// reading the hinted path must see the ROTATED token.
	if err := os.WriteFile(mountPath+".tmp", []byte("rotated-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(mountPath+".tmp", mountPath); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(hints[0].Path)
	if err != nil {
		t.Fatalf("read hinted path: %v", err)
	}
	if string(b) != "rotated-token" {
		t.Errorf("hinted path content = %q, want the rotated token", string(b))
	}
}

// TestMaterializeHostFiles_SanitisesFilename verifies the host filename
// follows the shared SanitizeFileName rule (non-safe chars → `_`), so a
// secret named e.g. "cluster/kubeconfig" lands under a portable basename
// regardless of the DSL name shape.
func TestMaterializeHostFiles_SanitisesFilename(t *testing.T) {
	g := newTestGuard(t, Secret{
		Name:     "cluster/kubeconfig",
		Value:    "content",
		FilePath: "/run/iterion/secrets/cluster_kubeconfig",
	})
	dir := t.TempDir()
	cleanup, err := g.MaterializeHostFiles(dir)
	if err != nil {
		t.Fatalf("MaterializeHostFiles: %v", err)
	}
	defer cleanup()
	got := g.ResolveSecretRef("cluster/kubeconfig")
	want := filepath.Join(dir, "cluster_kubeconfig")
	if got != want {
		t.Errorf("ResolveSecretRef = %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("host secret file missing at %q: %v", want, err)
	}
}

// TestMaterializeHostFiles_SkipsEmptyValue mirrors the sandbox skip for
// an Optional file secret with no resolved value: no host file is
// written, and the mount path stays at the pre-materialise value so the
// tool sees the same "no such file" it would in a sandbox.
func TestMaterializeHostFiles_SkipsEmptyValue(t *testing.T) {
	g := newTestGuard(t, Secret{
		Name:     "opt",
		Value:    "",
		FilePath: "/run/iterion/secrets/opt",
	})
	dir := t.TempDir()
	cleanup, err := g.MaterializeHostFiles(dir)
	if err != nil {
		t.Fatalf("MaterializeHostFiles: %v", err)
	}
	defer cleanup()
	if got := g.ResolveSecretRef("opt"); got != "/run/iterion/secrets/opt" {
		t.Errorf("empty-value secret path should be unchanged, got %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("no file should be written for empty value, got %v", entries)
	}
}

// TestMaterializeHostFiles_NoFileHints is a no-op safety: a guard with
// only value secrets returns a nil-safe cleanup and no error.
func TestMaterializeHostFiles_NoFileHints(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "token", Value: fakeKey})
	cleanup, err := g.MaterializeHostFiles(t.TempDir())
	if err != nil {
		t.Fatalf("MaterializeHostFiles: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup should not be nil even on no-op")
	}
	cleanup() // must not panic
}

// TestMaterializeHostFiles_NilGuard is the nil-guard no-op path.
func TestMaterializeHostFiles_NilGuard(t *testing.T) {
	var g *Guard
	cleanup, err := g.MaterializeHostFiles("/tmp/does-not-matter")
	if err != nil {
		t.Fatalf("nil guard: %v", err)
	}
	if cleanup == nil {
		t.Fatal("nil guard should still return a callable cleanup")
	}
	cleanup()
}

// TestMaterializeHostFiles_EmptyDirError refuses an empty target dir so
// a caller that forgets to build the tempdir sees a loud failure instead
// of silently writing to CWD.
func TestMaterializeHostFiles_EmptyDirError(t *testing.T) {
	g := newTestGuard(t, Secret{
		Name:     "tok",
		Value:    "v",
		FilePath: "/run/iterion/secrets/tok",
	})
	if _, err := g.MaterializeHostFiles(""); err == nil {
		t.Fatal("expected error on empty dir")
	}
}

func TestRedact_HeuristicUnknownToken(t *testing.T) {
	g := newTestGuard(t) // no known secrets
	in := "leaked: " + awsKey + " end"
	got := g.Redact(in)
	if strings.Contains(got, awsKey) {
		t.Errorf("unknown AWS key not redacted heuristically: %q", got)
	}
	if !strings.Contains(got, DefaultConfig().Marker) {
		t.Errorf("expected marker in %q", got)
	}
}

func TestRedact_RecursiveBase64Decode(t *testing.T) {
	g := newTestGuard(t) // no known secrets; relies on recursive decode
	wrapped := base64.StdEncoding.EncodeToString([]byte(awsKey))
	in := "data " + wrapped + " more"
	got := g.Redact(in)
	if strings.Contains(got, wrapped) {
		t.Errorf("base64-wrapped AWS key not caught by recursive decode: %q", got)
	}
	if !strings.Contains(got, DefaultConfig().Marker) {
		t.Errorf("expected marker after recursive decode: %q", got)
	}
}

func TestRedact_DoesNotOverRedactBenign(t *testing.T) {
	g := newTestGuard(t)
	// A 40-char hex commit hash and a base64 of plain English — neither
	// is a token shape; the generic 0.6 rule is below the 0.7 MinScore,
	// so both must survive.
	commit := "9f1c2b3d4e5f60718293a4b5c6d7e8f901234567"
	benignB64 := base64.StdEncoding.EncodeToString([]byte("the quick brown fox jumps over a dog"))
	in := "commit " + commit + " note " + benignB64
	got := g.Redact(in)
	if !strings.Contains(got, commit) {
		t.Errorf("benign commit hash was over-redacted: %q", got)
	}
	if !strings.Contains(got, benignB64) {
		t.Errorf("benign base64 text was over-redacted: %q", got)
	}
}

func TestNilGuard_NoOp(t *testing.T) {
	var g *Guard
	if got := g.Redact("hello " + awsKey); got != "hello "+awsKey {
		t.Errorf("nil Redact mutated input: %q", got)
	}
	if got := g.Materialize("x"); got != "x" {
		t.Errorf("nil Materialize mutated input: %q", got)
	}
	if g.ContainsSecret("x") {
		t.Error("nil ContainsSecret should be false")
	}
	if g.HasKnownSecrets() {
		t.Error("nil HasKnownSecrets should be false")
	}
}

func TestNew_SkipsShortValues(t *testing.T) {
	g := New([]Secret{{Name: "tiny", Value: "ab"}}, DefaultConfig())
	if g.HasKnownSecrets() {
		t.Error("values shorter than MinLen must not be registered")
	}
}

func TestRedact_PreservesExistingPlaceholder(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "k", Value: fakeKey})
	ph := defaultPlaceholder("k")
	in := "already redacted: " + ph + " ok"
	if got := g.Redact(in); !strings.Contains(got, ph) {
		t.Errorf("existing placeholder was clobbered: %q", got)
	}
}

func TestRedact_HeuristicDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Heuristic = false
	g := New(nil, cfg)
	in := "leaked: " + awsKey
	if got := g.Redact(in); !strings.Contains(got, awsKey) {
		t.Errorf("heuristic disabled should leave unknown tokens: %q", got)
	}
}
