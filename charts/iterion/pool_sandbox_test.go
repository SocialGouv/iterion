package chart_test

import (
	"fmt"
	"slices"
	"testing"
)

// runnerPools[].sandbox.enabled — runner.sandbox.enabled for ONE pool
// (ADR-162 Q3): the fork review lane's pool resolves the kubernetes sandbox
// for every run without moving every trusted `sandbox:` bot of the release
// into sibling pods (Q3's option B, the release-wide flag, is rejected). The
// flag renders on the pool's own Deployment as literals over the shared
// ConfigMap's envFrom, so nothing else of the release moves — and until
// ADR-154's pool shape lands (#2965: the pool's own ServiceAccount and Role,
// the pool label on its siblings, its standing NetworkPolicy) a pool turning
// its sandbox ON where the release does not provision one is refused at
// render time on the two arms that would otherwise fail at run time.
//
// Mutations, each red on its own subtest:
//   - the pool template's `if $sandbox` back to `if $.Values.runner.sandbox.enabled`
//     → "a pool opts in" and "a pool opts out" lose / keep the downward-API env;
//   - the `$ownSandbox` literals dropped → the opted-in pool keeps the
//     ConfigMap's ITERION_SANDBOX_OVERRIDE=none ("a pool opts in"), the
//     opted-out pool loses its override ("a pool opts out");
//   - either fail-closed requirement removed → its refusal row renders;
//   - the named-account requirement removed → the `default` ServiceAccount
//     row renders;
//   - iterion.poolSandbox ignoring the pool's key → every opt-in/opt-out row;
//   - the ConfigMap's image guard reading the release-wide flag alone → the
//     digest row renders an unpinned sandbox: auto image for the opted-in pool;
//   - the opted-out pool's automountServiceAccountToken dropped → "a pool
//     opts out" reads a mounted token;
//   - the opted-in literal ITERION_SANDBOX_DEFAULT dropped → "a pool opts in"
//     loses it; ITERION_SANDBOX_DEFAULT dropped from the owned names → its
//     conflict row renders;
//   - iterion.poolSandbox reading the map through sprig default, or not
//     ranging its keys → the falsy and misspelled rows of "the flag is a
//     boolean" render.
func TestChartRenderPoolSandbox(t *testing.T) {
	const alpha, solo = "iterion-runner-pool-alpha", "iterion-runner-pool-solo"
	pools := []string{"-f", "testdata/values-pools.yaml"}
	// The shape the fail-closed requirements admit until #2965: the operator
	// brings a NAMED ServiceAccount (and its bindings) and the release-wide
	// NetworkPolicy is off (the chart default).
	operatorRBAC := []string{"--set", "serviceAccount.create=false", "--set", "serviceAccount.name=iterion-operator"}
	poolContainer := func(t *testing.T, docs []map[string]any, name string) map[string]map[string]any {
		t.Helper()
		return envMap(dig(podSpec(t, docs, "Deployment", name), "containers", "0"))
	}
	downward := []string{"ITERION_POD_IP", "ITERION_RUNNER_POD_NAME", "ITERION_RUNNER_POD_UID"}

	t.Run("spelling the release's own value renders byte-identical to not spelling it", func(t *testing.T) {
		for _, release := range []string{"false", "true"} {
			base := slices.Clip(append(append([]string{"-f", "values-prod.yaml"}, pools...), "--set", "runner.sandbox.enabled="+release))
			unspelled := mustRender(t, base...)
			spelled := mustRender(t, append(base,
				"--set", "runnerPools[0].sandbox.enabled="+release,
				"--set", "runnerPools[1].sandbox.enabled="+release)...)
			if spelled != unspelled {
				t.Fatalf("runner.sandbox.enabled=%s: a pool spelling the release's own value renders differently from one that does not — nothing may move unless a pool's flag differs", release)
			}
			docs := manifests(t, unspelled)
			for _, name := range []string{alpha, solo} {
				env := poolContainer(t, docs, name)
				if _, ok := env["ITERION_SANDBOX_OVERRIDE"]; ok {
					t.Fatalf("runner.sandbox.enabled=%s: %s renders a literal ITERION_SANDBOX_OVERRIDE though no pool opted anything", release, name)
				}
				if _, ok := env["ITERION_POD_IP"]; ok != (release == "true") {
					t.Fatalf("runner.sandbox.enabled=%s: %s ITERION_POD_IP present=%v — a pool that spells nothing follows the release", release, name, ok)
				}
			}
		}
	})

	t.Run("a pool opts in: the sandbox env on that pool only, the release's override kept on the others", func(t *testing.T) {
		docs := manifests(t, mustRender(t, slices.Concat(pools, operatorRBAC, []string{
			"--set", "runnerPools[0].sandbox.enabled=true",
			"--set", "runner.sandbox.scheduling.requests.cpu=2",
		})...))
		in := poolContainer(t, docs, alpha)
		for _, name := range append(slices.Clone(downward), "ITERION_SANDBOX_K8S_REQUESTS_CPU") {
			if _, ok := in[name]; !ok {
				t.Fatalf("the opted-in pool renders no %s — its sandbox driver cannot advertise its proxy or place a sibling", name)
			}
		}
		// Literal env beats envFrom: the empty override lifts the ConfigMap's
		// none on this pool alone, and host_state is pinned as the release-wide
		// flag pins it.
		if got, ok := in["ITERION_SANDBOX_OVERRIDE"]; !ok || fmt.Sprint(got["value"]) != "" {
			t.Fatalf("the opted-in pool's ITERION_SANDBOX_OVERRIDE = %v (present=%v), want the empty literal over the ConfigMap's none", got, ok)
		}
		if got := fmt.Sprint(in["ITERION_SANDBOX_HOST_STATE"]["value"]); got != "none" {
			t.Fatalf("the opted-in pool's ITERION_SANDBOX_HOST_STATE = %q, want none — the driver has no host filesystem to bind", got)
		}
		// The global-default tier, over a release that set it to none for
		// everyone else: a bot declaring no `sandbox:` block resolves through it.
		withDefaultNone := manifests(t, mustRender(t, slices.Concat(pools, operatorRBAC, []string{
			"--set", "runnerPools[0].sandbox.enabled=true",
			"--set", "config.extraEnv.ITERION_SANDBOX_DEFAULT=none",
		})...))
		if got, ok := poolContainer(t, withDefaultNone, alpha)["ITERION_SANDBOX_DEFAULT"]; !ok || fmt.Sprint(got["value"]) != "auto" {
			t.Fatalf("the opted-in pool's ITERION_SANDBOX_DEFAULT = %v (present=%v), want the literal auto over the ConfigMap's none", got, ok)
		}
		// The other pool and the shared runner keep the release's posture.
		out := poolContainer(t, docs, solo)
		for _, name := range append(slices.Clone(downward), "ITERION_SANDBOX_OVERRIDE", "ITERION_SANDBOX_HOST_STATE", "ITERION_SANDBOX_K8S_REQUESTS_CPU") {
			if _, ok := out[name]; ok {
				t.Fatalf("the pool that did not opt in renders %s — one pool's flag moved another", name)
			}
		}
		runner := envMap(dig(podSpec(t, docs, "Deployment", "iterion-runner"), "containers", "0"))
		if _, ok := runner["ITERION_POD_IP"]; ok {
			t.Fatal("the shared runner renders ITERION_POD_IP — a pool's flag moved the release's trusted sandbox: bots into sibling pods (Q3's rejected option B)")
		}
		cm := must(t, docs, "ConfigMap", "iterion-config")
		if got := fmt.Sprint(dig(cm, "data", "ITERION_SANDBOX_OVERRIDE")); got != "none" {
			t.Fatalf("the shared ConfigMap's ITERION_SANDBOX_OVERRIDE = %q, want none — every runner that did not opt in keeps it", got)
		}
		if got := dig(cm, "data", "ITERION_SANDBOX_HOST_STATE"); got != nil {
			t.Fatalf("the shared ConfigMap carries ITERION_SANDBOX_HOST_STATE=%v — the release-wide flag is off", got)
		}
		if find(docs, "Role", "default-sandbox") != nil || find(docs, "Role", "iterion-sandbox") != nil {
			t.Fatal("a pool's flag rendered the release's sandbox Role — the pool's RBAC is ADR-154 decision 2's (#2965), not the shared ServiceAccount's")
		}
	})

	t.Run("a pool opts out of a release-wide sandbox", func(t *testing.T) {
		docs := manifests(t, mustRender(t, slices.Concat(pools, []string{
			"--set", "runner.sandbox.enabled=true",
			"--set", "runnerPools[1].sandbox.enabled=false",
		})...))
		out := poolContainer(t, docs, solo)
		if got, ok := out["ITERION_SANDBOX_OVERRIDE"]; !ok || fmt.Sprint(got["value"]) != "none" {
			t.Fatalf("the opted-out pool's ITERION_SANDBOX_OVERRIDE = %v (present=%v), want the literal none", got, ok)
		}
		// Every job of the opted-out pool runs in its own pod, under the
		// ServiceAccount the release binds to the sandbox Role: no token.
		if find(docs, "RoleBinding", "iterion-sandbox") == nil {
			t.Fatal("premise lost: the release-wide sandbox renders no RoleBinding for the shared ServiceAccount")
		}
		if got := podSpec(t, docs, "Deployment", solo)["automountServiceAccountToken"]; got != false {
			t.Fatalf("the opted-out pool's automountServiceAccountToken = %v, want false — its in-pod runs would read a token bound to the sandbox Role", got)
		}
		if got, ok := podSpec(t, docs, "Deployment", alpha)["automountServiceAccountToken"]; ok {
			t.Fatalf("the pool that follows the release-wide sandbox renders automountServiceAccountToken=%v — its driver needs the token", got)
		}
		for _, name := range downward {
			if _, ok := out[name]; ok {
				t.Fatalf("the opted-out pool renders %s", name)
			}
		}
		in := poolContainer(t, docs, alpha)
		if _, ok := in["ITERION_SANDBOX_OVERRIDE"]; ok {
			t.Fatal("the pool that follows the release-wide sandbox renders an override literal")
		}
		if _, ok := in["ITERION_POD_IP"]; !ok {
			t.Fatal("the pool that follows the release-wide sandbox lost ITERION_POD_IP")
		}
	})

	t.Run("fail closed until #2965: the RBAC the chart does not render for a pool", func(t *testing.T) {
		// serviceAccount.create (the default): the sandbox Role renders only
		// under runner.sandbox.enabled, so the pool's token holds no grant.
		mustRefuse(t, "#2965", append(slices.Clone(pools), "--set", "runnerPools[0].sandbox.enabled=true")...)
		mustRefuse(t, "serviceAccount.create=false", append(slices.Clone(pools), "--set", "runnerPools[0].sandbox.enabled=true")...)
		// The interim with no account named mounts the namespace's `default`
		// ServiceAccount: the grants bound to it would reach every pod of the
		// namespace that names none, so the chart asks for the dedicated one.
		unnamed := slices.Concat(pools, []string{"--set", "serviceAccount.create=false", "--set", "runnerPools[0].sandbox.enabled=true"})
		mustRefuse(t, "the namespace's `default` ServiceAccount", unnamed...)
		docs := manifests(t, mustRender(t, slices.Concat(pools, operatorRBAC, []string{"--set", "runnerPools[0].sandbox.enabled=true"})...))
		if got := podSpec(t, docs, "Deployment", alpha)["serviceAccountName"]; got != "iterion-operator" {
			t.Fatalf("the opted-in pool runs under %v, want the named account iterion-operator", got)
		}
	})

	t.Run("fail closed until #2965: the release-wide NetworkPolicy closes the pool runner's proxy", func(t *testing.T) {
		args := slices.Concat([]string{"-f", "values-prod.yaml"}, pools, operatorRBAC, []string{"--set", "runnerPools[0].sandbox.enabled=true"})
		mustRefuse(t, "networkPolicy.enabled", args...)
		mustRefuse(t, "ADR-154 decision 3 (#2965)", args...)
		// The same values with the policy off render.
		mustRender(t, append(args, "--set", "networkPolicy.enabled=false")...)
	})

	t.Run("the flag is a boolean and owns the two names on its pool", func(t *testing.T) {
		mustRefuse(t, "must be a boolean", slices.Concat(pools, operatorRBAC, []string{"--set-string", "runnerPools[0].sandbox.enabled=false"})...)
		for _, key := range []string{"ITERION_SANDBOX_OVERRIDE", "ITERION_SANDBOX_HOST_STATE"} {
			mustRefuse(t, "runnerPools[alpha].env."+key+" conflicts", slices.Concat(pools, operatorRBAC, []string{
				"--set", "runnerPools[0].sandbox.enabled=true",
				"--set", "runnerPools[0].env." + key + "=auto",
			})...)
		}
		mustRefuse(t, "runnerPools[alpha].env.ITERION_SANDBOX_DEFAULT conflicts", slices.Concat(pools, operatorRBAC, []string{
			"--set", "runnerPools[0].sandbox.enabled=true",
			"--set", "runnerPools[0].env.ITERION_SANDBOX_DEFAULT=none",
		})...)
		mustRefuse(t, "runnerPools[solo].env.ITERION_SANDBOX_OVERRIDE conflicts", slices.Concat(pools, []string{
			"--set", "runner.sandbox.enabled=true",
			"--set", "runnerPools[1].sandbox.enabled=false",
			"--set", "runnerPools[1].env.ITERION_SANDBOX_OVERRIDE=none",
		})...)
		// A falsy non-map, and a misspelled key, are refused rather than read
		// as "unset" — the pool would follow the release with no refusal.
		mustRefuse(t, "runnerPools[alpha].sandbox must be a map", slices.Concat(pools, []string{"--set", "runnerPools[0].sandbox=false"})...)
		for _, key := range []string{"enable", "Enabled"} {
			mustRefuse(t, "runnerPools[alpha].sandbox."+key+" is not a key", slices.Concat(pools, []string{"--set", "runnerPools[0].sandbox." + key + "=true"})...)
		}
	})

	t.Run("requireDigest reaches the sandbox: auto image a sandboxed pool reads from the ConfigMap", func(t *testing.T) {
		const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		const runnerRef = "ghcr.io/socialgouv/iterion-runner-devbox@" + digest
		const sandboxRef = "ghcr.io/socialgouv/iterion-sandbox-full@" + digest
		pinned := slices.Concat(pools, operatorRBAC, []string{
			"--set", "image.requireDigest=true",
			"--set", "image.digest=" + digest,
			"--set", "runner.image=" + runnerRef,
			"--set", "runnerPools[0].image=" + runnerRef,
			"--set", "runnerPools[1].image=" + runnerRef,
		})
		// Nothing sandboxed: no per-run pod, nothing to pin.
		mustRender(t, pinned...)
		optIn := append(slices.Clone(pinned), "--set", "runnerPools[0].sandbox.enabled=true")
		mustRefuse(t, "config.extraEnv.ITERION_SANDBOX_DEFAULT_IMAGE", optIn...)
		mustRender(t, append(slices.Clone(optIn), "--set", "config.extraEnv.ITERION_SANDBOX_DEFAULT_IMAGE="+sandboxRef)...)
		mustRender(t, append(slices.Clone(optIn), "--set", "runnerPools[0].env.ITERION_SANDBOX_DEFAULT_IMAGE="+sandboxRef)...)
		// An opted-out pool reads no sandbox image: a release-wide sandbox
		// whose only readers opted out, or pin their own, asks for nothing.
		mustRender(t, append(slices.Clone(pinned),
			"--set", "runner.enabled=false",
			"--set", "runner.sandbox.enabled=true",
			"--set", "runnerPools[0].sandbox.enabled=false",
			"--set", "runnerPools[1].env.ITERION_SANDBOX_DEFAULT_IMAGE="+sandboxRef)...)
	})
}
