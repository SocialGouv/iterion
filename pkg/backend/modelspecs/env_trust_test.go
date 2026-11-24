package modelspecs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/store"
)

// plantEnv records these names as a project `.env` filled them in — what
// cmd/iterion applyDotEnv does for a line it fills — and clears the planted
// set before and after, since it outlives one test.
func plantEnv(t *testing.T, names ...string) {
	t.Helper()
	inheritEnv(t)
	envtrust.MarkPlanted(names...)
}

// inheritEnv is the control state: nothing planted, every knob speaking for
// the operator as an exported variable does.
func inheritEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
}

// The five ITERION_MODEL_SPECS_* knobs are ONE class: each selects which
// table answers for EVERY model — the endpoint fetched, the file the table is
// read back from, how long it is believed, whether the tier runs at all. A
// `.env` sits in a repository, including the one under review, whose `.env`
// the in-container runner auto-loads over every name the sandbox crossing
// left absent; and on the host, a value taken for the operator's there is
// laundered into the record the crossing carries as the host's own answer.
// So none of the five speaks for the operator when it was planted. Each name
// is driven on its own: inherited (the control — it must still apply) and
// planted (it must not).
func TestOptionsFromEnv_APlantedKnobNeverSelectsTheTable(t *testing.T) {
	const (
		repoURL = "https://repo-under-review.invalid/api.json"
		repoTTL = 90 * time.Second
	)
	for _, tc := range []struct {
		name  string
		value string
		// applied asserts the knob reached Options; ignored asserts the
		// package default did instead.
		applied func(Options) bool
		ignored func(Options) bool
	}{
		{EnabledEnv, "off",
			func(o Options) bool { return o.Disabled },
			func(o Options) bool { return !o.Disabled }},
		{RefreshEnv, "1",
			func(o Options) bool { return o.ForceRefresh },
			func(o Options) bool { return !o.ForceRefresh }},
		{URLEnv, repoURL,
			func(o Options) bool { return o.URL == repoURL },
			func(o Options) bool { return o.URL == "" }},
		{CacheEnv, "/tmp/iterion-chosen-by-the-repo.json",
			func(o Options) bool { return o.CachePath == "/tmp/iterion-chosen-by-the-repo.json" },
			func(o Options) bool { return o.CachePath == "" }},
		{TTLEnv, "90s",
			func(o Options) bool { return o.TTL == repoTTL },
			func(o Options) bool { return o.TTL == 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)

			// Inherited, the knob applies — an operator's own deployment
			// must not lose its registry configuration to this.
			inheritEnv(t)
			if opts := OptionsFromEnv(); !tc.applied(opts) {
				t.Fatalf("an inherited %s=%q did not apply: %+v", tc.name, tc.value, opts)
			}

			// The same bytes, planted by a workspace `.env`.
			plantEnv(t, tc.name)
			opts := OptionsFromEnv()
			if !tc.ignored(opts) {
				t.Fatalf("a planted %s=%q selected the table the run resolves every model from: %+v", tc.name, tc.value, opts)
			}
		})
	}
}

// The EFFECT, not the label: ITERION_MODEL_SPECS_CACHE names a FILE, and the
// registry indexes whatever that file holds. A repository writing one — one
// `.env` line and one JSON file in the tree under review — would otherwise
// choose the window its reviewer's nodes compact at, the output budget they
// ask for and the price they are billed at, for every model, with the
// provenance reading `catalog-live` (#2844, the H-10 measurement).
func TestOptionsFromEnv_APlantedCacheFileDoesNotAnswerForAnyModel(t *testing.T) {
	forged := filepath.Join(t.TempDir(), "specs.json")
	raw, err := json.Marshal(cacheFile{
		FetchedAt: time.Now(),
		Source:    Source,
		Specs: map[string]Spec{
			"scaleway/gw-m": {ContextWindow: 4096, MaxOutputTokens: 1, InputCostPerM: 0.000001, OutputCostPerM: 0.000001},
		},
	})
	if err != nil {
		t.Fatalf("marshal the forged cache: %v", err)
	}
	if err := os.WriteFile(forged, raw, 0o600); err != nil {
		t.Fatalf("write the forged cache: %v", err)
	}
	t.Setenv(CacheEnv, forged)

	// NoAutoFetch is the test's own: it is not a knob, and nothing here may
	// reach the network. Everything the case is about comes from the env.
	registry := func() *Registry {
		opts := OptionsFromEnv()
		opts.NoAutoFetch = true
		return New(opts)
	}

	// Inherited, the operator's own cache answers — the disk tier is the
	// point of the knob.
	inheritEnv(t)
	if spec, ok := registry().LookupExact("scaleway", "gw-m"); !ok || spec.ContextWindow != 4096 {
		t.Fatalf("an inherited %s stopped answering: %+v ok=%v", CacheEnv, spec, ok)
	}

	plantEnv(t, CacheEnv)
	spec, ok := registry().LookupExact("scaleway", "gw-m")
	if ok || spec.ContextWindow != 0 || spec.MaxOutputTokens != 0 || spec.InputCostPerM != 0 {
		t.Fatalf("a planted %s answered %+v (ok=%v): the repository under review chose the window, the output budget and the price of the run reviewing it", CacheEnv, spec, ok)
	}
}

// The fetched table persists under the iterion home the OPERATOR chose. A
// home a project `.env` selected re-roots this file as surely as the cache
// knob names it outright — the same laundering, one variable further out.
func TestDefaultCachePath_APlantedHomeDoesNotRootTheCache(t *testing.T) {
	repoHome := t.TempDir()
	t.Setenv("ITERION_HOME", repoHome)
	t.Setenv("HOME", t.TempDir())

	inheritEnv(t)
	if got, want := DefaultCachePath(), filepath.Join(repoHome, "model-specs-cache.json"); got != want {
		t.Fatalf("an inherited ITERION_HOME stopped rooting the cache: %q, want %q", got, want)
	}

	plantEnv(t, "ITERION_HOME")
	if got := DefaultCachePath(); got == filepath.Join(repoHome, "model-specs-cache.json") {
		t.Fatalf("the cache the registry reads every model from lives where the repository's .env said: %q", got)
	}
}

// No home at all is no disk tier — not a relative file in whatever directory
// the process runs in, and not the <tmp>/iterion-data fallback a writer takes
// without a home, which any local user can create and fill before iterion
// does. The registry keeps the network and the embedded snapshot.
func TestDefaultCachePath_NoInheritedHomeMeansNoDiskTier(t *testing.T) {
	t.Setenv("ITERION_HOME", "")
	t.Setenv("HOME", "")
	store.ResolveIterionHomeAsInProductionForTests(t)
	inheritEnv(t)

	if got := DefaultCachePath(); got != "" {
		t.Fatalf("DefaultCachePath() = %q with no inherited home, want \"\"", got)
	}
	// Both disk sites ask diskCacheOff, so the empty path neither reads nor
	// writes anything: a WriteFileAtomic on "" would land a temp file in the
	// working directory, which under a run is the repository.
	r := New(Options{NoAutoFetch: true})
	if !r.diskCacheOff() {
		t.Fatalf("the registry kept a cache path %q with no inherited home", r.cachePath)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	before := dirEntries(t, wd)
	r.ensureFresh()
	r.writeCache(cacheFile{FetchedAt: time.Now(), Source: Source, Specs: map[string]Spec{"p/m": {ContextWindow: 1}}})
	if after := dirEntries(t, wd); after != before {
		t.Fatalf("the disk-tier-off registry wrote in the working directory: %d entries, was %d", after, before)
	}
}

func dirEntries(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return len(entries)
}

// The snapshot's whole review model is "the committed diff is the review",
// which needs the same upstream body to render the same file. Two upstream
// keys that normalise to one — differing only in case — made the winner
// depend on the order two map ranges happened to take: one body rendered two
// different snapshots, with a `tool_call` verdict among what flipped.
//
// The assertion is the STABLE winner, not merely "every rendering agrees": a
// test that can only fail in one order is flaky rather than a witness, and a
// mutant that drops the rule fails this on the value, whatever order it drew.
func TestParseModelsDev_ACaseCollidingKeyHasAStableWinner(t *testing.T) {
	body := []byte(`{"Prov":{"models":{"M1":{"limit":{"context":1},"cost":{"input":1,"output":2},"tool_call":true}}},` +
		`"prov":{"models":{"m1":{"limit":{"context":2},"cost":{"input":3,"output":4},"tool_call":false}}}}`)
	// "Prov/M1" < "prov/m1" (upper case sorts first), so the first is the
	// rule's winner: window 1, tool_call stated true.
	for i := 0; i < 200; i++ {
		table, err := ParseModelsDev(body)
		if err != nil {
			t.Fatalf("ParseModelsDev: %v", err)
		}
		if len(table) != 1 {
			t.Fatalf("two keys normalising to one rendered %d entries", len(table))
		}
		spec := table["prov/m1"]
		if spec.ContextWindow != 1 || spec.ToolCall == nil || !*spec.ToolCall {
			t.Fatalf("rendering %d resolved window %d tool_call %s, want the lexicographically first upstream key (window 1, tool_call yes)",
				i, spec.ContextWindow, flagWord(spec.ToolCall))
		}
	}
}
