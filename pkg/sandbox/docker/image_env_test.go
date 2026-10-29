package docker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
)

// discardLogger is a logger whose output goes nowhere: these tests assert on
// returned values, not on the run's stream.
func discardLogger() *iterlog.Logger { return iterlog.New(iterlog.LevelInfo, io.Discard) }

// fakeInspector writes a container-runtime stand-in whose `image inspect
// --format {{json .Config.Env}}` answers with env, and returns it as a
// [Runtime] (an absolute path, so no PATH is touched). exitCode != 0 makes
// it fail the way an absent image does.
func fakeInspector(t *testing.T, env string, exitCode int) Runtime {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in runtime is a shell script")
	}
	bin := filepath.Join(t.TempDir(), "fake-runtime")
	script := "#!/bin/sh\n"
	if exitCode != 0 {
		script += "echo 'Error response from daemon: No such image' >&2\nexit " +
			string(rune('0'+exitCode)) + "\n"
	} else {
		script += "cat <<'JSON'\n" + env + "\nJSON\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("writing the stand-in runtime: %v", err)
	}
	return Runtime(bin)
}

// TestImageConfigEnvReadsWhatTheImageBakesIn is the read half of the screen:
// the driver reports the image's own ENV entries, in the image's order.
func TestImageConfigEnvReadsWhatTheImageBakesIn(t *testing.T) {
	rt := fakeInspector(t, `["PATH=/usr/bin","CLAUDE_CODE_USE_BEDROCK=1","AWS_SECRET_ACCESS_KEY=planted"]`, 0)
	d := &Driver{rt: rt, logger: discardLogger()}
	prepared := &Prepared{spec: sandbox.Spec{Image: "ghcr.io/planter/dev:1"}, runtime: rt}

	env, err := d.ImageConfigEnv(context.Background(), prepared)
	if err != nil {
		t.Fatalf("ImageConfigEnv: %v", err)
	}
	want := []string{"PATH=/usr/bin", "CLAUDE_CODE_USE_BEDROCK=1", "AWS_SECRET_ACCESS_KEY=planted"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("ImageConfigEnv = %q, want %q", env, want)
	}
}

// TestImageConfigEnvReportsAnUnreadableImage: an image the local store does
// not have is an ERROR, never an empty list — an empty list reads as a
// clean image and would hand the run a guard it never had.
func TestImageConfigEnvReportsAnUnreadableImage(t *testing.T) {
	rt := fakeInspector(t, "", 1)
	d := &Driver{rt: rt, logger: discardLogger()}
	prepared := &Prepared{spec: sandbox.Spec{Image: "ghcr.io/planter/absent:1"}, runtime: rt}

	env, err := d.ImageConfigEnv(context.Background(), prepared)
	if err == nil {
		t.Fatalf("ImageConfigEnv on an absent image returned %q and no error", env)
	}
	if env != nil {
		t.Errorf("ImageConfigEnv returned %q alongside its error, want nil", env)
	}
	if !strings.Contains(err.Error(), "ghcr.io/planter/absent:1") {
		t.Errorf("the error does not name the image it could not read: %v", err)
	}
}

// TestImageConfigEnvAcceptsAnImageWithNoEnv: `.Config.Env` of a config that
// sets none inspects as the JSON literal null, which is an image setting
// nothing — not a decoding failure.
func TestImageConfigEnvAcceptsAnImageWithNoEnv(t *testing.T) {
	rt := fakeInspector(t, "null", 0)
	d := &Driver{rt: rt, logger: discardLogger()}
	prepared := &Prepared{spec: sandbox.Spec{Image: "scratch:bare"}, runtime: rt}

	env, err := d.ImageConfigEnv(context.Background(), prepared)
	if err != nil {
		t.Fatalf("ImageConfigEnv: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("ImageConfigEnv = %q, want nothing", env)
	}
}

// TestNeutralizedImageEnvReachesTheRunArgv is the witness of the whole
// deterministic half: a planted name neutralised on the prepared spec comes
// out of the argv builder `docker run` is given as `--env NAME=`, which is
// what overrides the image's own ENV.
//
// It exercises [envArgs], the producer Start appends — not a re-reading of
// the spec — so the claim is about the argv and not about the map.
func TestNeutralizedImageEnvReachesTheRunArgv(t *testing.T) {
	d := &Driver{rt: RuntimeDocker, logger: discardLogger()}
	prepared := &Prepared{
		spec:    sandbox.Spec{Image: "ghcr.io/planter/dev:1", Env: map[string]string{"KEPT": "value"}},
		runtime: RuntimeDocker,
	}

	screened, emptied, err := d.NeutralizeImageEnv(prepared, sandbox.RunInfo{}, []string{"CLAUDE_CODE_USE_BEDROCK", "AWS_SECRET_ACCESS_KEY"})
	if err != nil {
		t.Fatalf("NeutralizeImageEnv: %v", err)
	}
	// What it says it emptied is what it emptied: the caller publishes
	// that list as the run's audit record of a container started with
	// `--env NAME=`.
	if want := []string{"CLAUDE_CODE_USE_BEDROCK", "AWS_SECRET_ACCESS_KEY"}; !slices.Equal(emptied, want) {
		t.Errorf("NeutralizeImageEnv reports it emptied %q, want %q", emptied, want)
	}
	sp, ok := screened.(*Prepared)
	if !ok {
		t.Fatalf("NeutralizeImageEnv returned %T, want *Prepared", screened)
	}
	args, err := envArgs(sp.spec.Env)
	if err != nil {
		t.Fatalf("envArgs: %v", err)
	}
	for _, want := range []string{"--env", "CLAUDE_CODE_USE_BEDROCK=", "AWS_SECRET_ACCESS_KEY=", "KEPT=value"} {
		if !slices.Contains(args, want) {
			t.Errorf("the run argv lacks %q: %q", want, args)
		}
	}
	// The planted VALUE must appear nowhere in the argv.
	for _, arg := range args {
		if strings.Contains(arg, "CLAUDE_CODE_USE_BEDROCK=1") {
			t.Errorf("the argv carries the planted value: %q", arg)
		}
	}
	// The prepared spec handed in is not mutated: the caller may still
	// start the original, and a shared map would leak the override.
	if _, mutated := prepared.spec.Env["CLAUDE_CODE_USE_BEDROCK"]; mutated {
		t.Errorf("NeutralizeImageEnv mutated the prepared spec it was handed: %v", prepared.spec.Env)
	}
}

// TestNeutralizeImageEnvLeavesTheOperatorsOwnValueAlone: the workflow's
// `sandbox.env:` and the launch overlay are the operator's reviewed channel.
// The screen reports the collision; it never replaces that value with an
// empty string.
//
// And it says so: the name it skipped is absent from the list it reports
// as emptied. That list is the one the run's event and log publish as
// "set and EMPTY", a container state `docker inspect` can contradict —
// this is the measurement the runtime's doubles copy their skip rule
// from, so an over-generous answer here is a false audit record there.
func TestNeutralizeImageEnvLeavesTheOperatorsOwnValueAlone(t *testing.T) {
	d := &Driver{rt: RuntimeDocker, logger: discardLogger()}
	prepared := &Prepared{
		spec:    sandbox.Spec{Image: "i:1", Env: map[string]string{"AWS_REGION": "eu-west-3"}},
		runtime: RuntimeDocker,
	}

	screened, emptied, err := d.NeutralizeImageEnv(prepared, sandbox.RunInfo{}, []string{"AWS_REGION", "AWS_SECRET_ACCESS_KEY"})
	if err != nil {
		t.Fatalf("NeutralizeImageEnv: %v", err)
	}
	got := screened.(*Prepared).spec.Env
	if got["AWS_REGION"] != "eu-west-3" {
		t.Errorf("AWS_REGION = %q, want the operator's own eu-west-3", got["AWS_REGION"])
	}
	if v, ok := got["AWS_SECRET_ACCESS_KEY"]; !ok || v != "" {
		t.Errorf("AWS_SECRET_ACCESS_KEY = %q (present %v), want neutralised", v, ok)
	}
	if !slices.Equal(emptied, []string{"AWS_SECRET_ACCESS_KEY"}) {
		t.Errorf("NeutralizeImageEnv reports it emptied %q, want [AWS_SECRET_ACCESS_KEY] alone: the container starts with the operator's AWS_REGION, so a caller told it was emptied would publish a state docker inspect contradicts", emptied)
	}
	// The argv is the producer, and it carries the operator's value and
	// the empty override — exactly what the two lists say.
	args, err := envArgs(got)
	if err != nil {
		t.Fatalf("envArgs: %v", err)
	}
	for _, want := range []string{"AWS_REGION=eu-west-3", "AWS_SECRET_ACCESS_KEY="} {
		if !slices.Contains(args, want) {
			t.Errorf("the run argv lacks %q: %q", want, args)
		}
	}
}

// TestNeutralizeImageEnvRefusesAMalformedName: an image's ENV is the
// planter's text. A name carrying `=`, a newline or a NUL would inject a
// second docker flag, so it is refused at the screen rather than three
// steps later in Start.
func TestNeutralizeImageEnvRefusesAMalformedName(t *testing.T) {
	d := &Driver{rt: RuntimeDocker, logger: discardLogger()}
	prepared := &Prepared{spec: sandbox.Spec{Image: "i:1"}, runtime: RuntimeDocker}

	if _, _, err := d.NeutralizeImageEnv(prepared, sandbox.RunInfo{}, []string{"OK_TOKEN\nDOCKER_HOST=tcp://planter"}); err == nil {
		t.Fatalf("NeutralizeImageEnv accepted a name carrying a newline")
	}
}

// inspectedRunInfo is the [sandbox.RunInfo] a run under an ENFORCED network
// policy produces: an egress proxy endpoint, the host-gateway alias its MCP
// listeners need, and a per-run CA because that proxy inspects TLS. It is
// the shape under which Start appends pairs of its OWN, which is the state
// every witness below is about.
func inspectedRunInfo() sandbox.RunInfo {
	return sandbox.RunInfo{
		RunID:            "run-image-env-driver-set",
		WorkspacePath:    "/tmp/iterion-witness-workspace",
		ProxyEndpoint:    "http://host.docker.internal:45321",
		HostGatewayAlias: true,
		ProxyCACert:      testCACert(),
	}
}

// TestNeutralizeImageEnvLeavesTheNamesTheDriversOwnStartSetsAlone is the
// witness of the audit record `docker inspect` judges.
//
// `HTTP_PROXY` and its five sibling spellings are in the neutralise
// sub-class — `_PROXY` is a suffix of it, and `NO_PROXY` matches the same
// suffix — so the screen asks for them. But Start appends its own
// `--env HTTP_PROXY=<endpoint>` AFTER the spec's pairs, and the last pair
// for a name is the one the container gets: a driver that answered
// "emptied" would have the run publish `neutralized: [HTTP_PROXY]` of a
// container whose `.Config.Env` reads the proxy URL. Measured on the real
// driver: with the pair in place `docker inspect` returned
// `http://host.docker.internal:45321`, not "".
func TestNeutralizeImageEnvLeavesTheNamesTheDriversOwnStartSetsAlone(t *testing.T) {
	d := &Driver{rt: RuntimeDocker, logger: discardLogger()}
	prepared := &Prepared{spec: sandbox.Spec{Image: "i:1"}, runtime: RuntimeDocker}
	info := inspectedRunInfo()

	// The whole class the screen would decide on for this image: the four
	// proxy spellings, both NO_PROXY spellings, two CA bundles the driver
	// points at its own CA, and one name nothing but the image sets.
	asked := []string{
		"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy",
		"NODE_EXTRA_CA_CERTS", "GIT_SSL_CAINFO", "JAVA_TOOL_OPTIONS",
		"ANTHROPIC_BASE_URL",
	}
	screened, emptied, err := d.NeutralizeImageEnv(prepared, info, asked)
	if err != nil {
		t.Fatalf("NeutralizeImageEnv: %v", err)
	}
	if !slices.Equal(emptied, []string{"ANTHROPIC_BASE_URL"}) {
		t.Errorf("NeutralizeImageEnv reports it emptied %q, want [ANTHROPIC_BASE_URL] alone: every other name is one this driver's own Start sets, so a container created under this RunInfo carries ITERION's value for it and an audit record saying `set and EMPTY` is one `docker inspect` contradicts",
			emptied)
	}
	// And the spec carries no empty override for them either: the pair
	// would be harmless (the driver's own wins) and the map is what a
	// reader of the prepared spec sees.
	got := screened.(*Prepared).spec.Env
	for _, name := range asked {
		if name == "ANTHROPIC_BASE_URL" {
			continue
		}
		if v, present := got[name]; present {
			t.Errorf("the screened spec carries %s=%q: this driver sets that name itself at Start, and the screen must leave it to it", name, v)
		}
	}
	if v, present := got["ANTHROPIC_BASE_URL"]; !present || v != "" {
		t.Errorf("ANTHROPIC_BASE_URL = %q (present %v), want the planted route emptied", v, present)
	}
}

// TestTheDriverDeclaresEveryEnvNameItsOwnStartSets is the chokepoint: the
// declaration the image env screen reads and the argv `docker run` is given
// come from the SAME name lists, under the same conditions.
//
// Driven over the four RunInfo shapes that switch those appends on and off,
// because the declaration is conditional too: a name declared for a run
// whose Start would not set it is filed as "the driver's value" while the
// container carries the image's — the mirror of the defect above.
func TestTheDriverDeclaresEveryEnvNameItsOwnStartSets(t *testing.T) {
	d := &Driver{rt: RuntimeDocker, logger: discardLogger()}
	for _, tc := range []struct {
		name string
		info sandbox.RunInfo
	}{
		{"no proxy, no alias, no CA", sandbox.RunInfo{}},
		{"the host-gateway alias alone", sandbox.RunInfo{HostGatewayAlias: true}},
		{"a proxy endpoint", sandbox.RunInfo{ProxyEndpoint: "http://host.docker.internal:45321"}},
		{"a CA alone", sandbox.RunInfo{ProxyCACert: testCACert()}},
		{"an enforced policy with TLS inspection", inspectedRunInfo()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fromArgv := envNamesOfArgs(hostNetworkArgs(tc.info, nil))
			if len(tc.info.ProxyCACert) > 0 {
				caArgs, dirs, err := caInjectionArgs(tc.info)
				t.Cleanup(func() { cleanupTempDirs(dirs) })
				if err != nil {
					t.Fatalf("caInjectionArgs: %v", err)
				}
				fromArgv = append(fromArgv, envNamesOfArgs(caArgs)...)
			}
			slices.Sort(fromArgv)
			declared := d.StartEnvNames(tc.info)
			slices.Sort(declared)
			if !slices.Equal(fromArgv, declared) {
				t.Errorf("Start's own argv sets %q and StartEnvNames declares %q — the image env screen reads the declaration, so a pair missing from it is published as emptied or as the image's own value",
					fromArgv, declared)
			}
		})
	}
}

// envNamesOfArgs reads the names out of the `--env NAME=value` pairs of an
// argv — the argv is the producer, so this is what the container actually
// gets.
func envNamesOfArgs(args []string) []string {
	var names []string
	for i, arg := range args {
		if arg != "--env" || i+1 >= len(args) {
			continue
		}
		name, _, _ := strings.Cut(args[i+1], "=")
		names = append(names, name)
	}
	return names
}

// TestNoEnvPairOfAStartedContainerIsBuiltOutsideTheDeclaredHelpers holds
// the chokepoint at the CLASS: the previous test compares two lists, and
// this one proves there is no third place to add a pair from.
//
// It reads this package's own non-test sources and finds every function
// that builds a `--env` pair. Each must be accounted for below: the two
// helpers [Driver.StartEnvNames] is read from, the spec's own builder, or a
// site that is not the container's creation env at all (`docker exec`, the
// probe's one-shot container). A new pair appended straight into Start —
// the shape that produced this defect — has no row and reddens here.
func TestNoEnvPairOfAStartedContainerIsBuiltOutsideTheDeclaredHelpers(t *testing.T) {
	declared := map[string]string{
		"hostNetworkArgs": "the proxy and NO_PROXY spellings, from proxyEnvNames/noProxyEnvNames, which StartEnvNames declares",
		"caInjectionArgs": "the CA bundles and the truststore, from caBundleEnvNames/javaTrustEnvNames, which StartEnvNames declares",
		"envArgs":         "the prepared spec's OWN env — the reviewed channel, which the screen reads from the spec itself",
		"Command":         "`docker exec --env`: the per-command layer, not the env the container is CREATED with, and the screen's own docs name it as the second layer",
	}
	found := envPairBuilders(t)
	if len(found) < 3 {
		t.Fatalf("read only %d function(s) building a `--env` pair (%v): the source reader is broken, and an empty set would make this guard pass vacuously", len(found), found)
	}
	for _, fn := range found {
		if _, ok := declared[fn]; !ok {
			t.Errorf("%s builds a `--env` pair and is not accounted for: if the container is CREATED with it, read its names from a list StartEnvNames declares (the image env screen publishes what the container has, and `docker inspect` is the judge); if it is another layer, say so in the table of this test", fn)
		}
	}
	for fn := range declared {
		if !slices.Contains(found, fn) {
			t.Errorf("the table names %q, which builds no `--env` pair any more: drop the row rather than leave a reviewed site pointing at nothing", fn)
		}
	}
}

// envPairBuilders returns the names of this package's non-test functions
// whose body carries a `"--env"` literal.
func envPairBuilders(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	var found []string
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING && lit.Value == `"--env"` && !slices.Contains(found, fn.Name.Name) {
					found = append(found, fn.Name.Name)
				}
				return true
			})
		}
	}
	slices.Sort(found)
	return found
}

// TestUsableEnvNameImpliesTheDockerEnvGuardAccepts is why both docs pages
// say no docker image can trigger the refusal branch today.
//
// The screen hands [devcontainer.UsableEnvName] names alone to
// NeutralizeImageEnv, which refuses on [validateEnvVar] with an EMPTY
// value. The two predicates read the same character set, so "usable"
// implies "accepted" — and the branch that stops the boot is right for a
// FUTURE screener with a narrower guard, not an outcome an operator can
// observe. Walked rather than argued: every byte alone and in the shapes an
// image's `ENV` can carry, plus the evasions a planter would try.
func TestUsableEnvNameImpliesTheDockerEnvGuardAccepts(t *testing.T) {
	var corpus []string
	for b := 0; b < 256; b++ {
		c := string(rune(b))
		corpus = append(corpus, c, "AWS_"+c, c+"_TOKEN", "AWS"+c+"_SECRET")
	}
	corpus = append(corpus,
		"", "=", "A=B", "AWS_SECRET\nACCESS_KEY", "AWS_SECRET\rACCESS_KEY", "AWS_SECRET\x00KEY",
		"anthropic_base_url", "ANTHROPIC_BASE_URL", "İMDS_ENDPOINT", "ſhellopts", "ＡＷＳ_TOKEN",
		"BASH_FUNC_git%%", "GIT_CONFIG_KEY_0", "LD_PRELOAD", "PS4",
	)
	usableButRefused := 0
	for _, name := range corpus {
		if !devcontainer.UsableEnvName(name) {
			continue
		}
		if err := validateEnvVar(name, ""); err != nil {
			usableButRefused++
			t.Errorf("UsableEnvName(%q) is true and validateEnvVar refuses it (%v): the refusal branch of the image env screen IS reachable for a docker image, and both docs pages say it is not",
				name, err)
		}
	}
	if len(corpus) < 256 {
		t.Fatalf("the corpus holds %d names: an empty walk would prove the implication vacuously", len(corpus))
	}
	t.Logf("%d names walked, %d usable-but-refused", len(corpus), usableButRefused)
}

// testCACert is a self-signed CA PEM, the shape
// [sandbox.RunInfo.ProxyCACert] carries when the egress proxy inspects
// TLS: the real thing, because netproxy.JavaTrustStorePKCS12 parses it and
// a placeholder would make the CA arm of these witnesses unreachable.
func testCACert() []byte {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("test CA key: " + err.Error())
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "iterion egress witness CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic("test CA cert: " + err.Error())
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
