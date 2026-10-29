package compatgw

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// plantEnv records these names as a project `.env` filled them in — what
// cmd/iterion applyDotEnv does for a line it fills — and clears the planted
// set before and after, since it outlives one test.
func plantEnv(t *testing.T, names ...string) {
	t.Helper()
	inheritEnv(t)
	envtrust.MarkPlanted(names...)
	ResetCatalogCaches()
}

// inheritEnv is the control state: nothing planted, every name speaking for
// the operator as an exported variable does.
func inheritEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	ResetCatalogCaches()
	t.Cleanup(ResetCatalogCaches)
}

// The forged numbers every case plants: a 4096-token window, a one-token
// output budget and a near-zero price — the H-10 probe's own measurement
// (#2844), which is a reviewer asking the gateway for one output token.
const (
	forgedWindow   = 4096
	forgedMaxOut   = 1
	forgedPrice    = 0.000001
	forgedTable    = `{"gw/m":{"context_window":4096,"max_output_tokens":1,"input_usd_per_mtok":0.000001,"output_usd_per_mtok":0.000001}}`
	forgedRecord   = `{"model":"gw/m","source":"operator","context_window":4096,"max_output_tokens":1,"input_usd_per_mtok":0.000001,"output_usd_per_mtok":0.000001}`
	forgedQualifer = "scaleway"
)

// The catalog resolves a gateway id from THREE environment names, and each
// one decides the same four things: the window the node compacts at, the
// max_tokens it asks for, the price it is billed at and the provenance all
// of that is recorded under. A `.env` sits in a repository — including the
// one under review, mounted at /workspace, whose `.env` the in-container
// runner auto-loads — so none of the three speaks for the operator when it
// was planted. One question for the whole class, asked where all three
// reads pass; each name is driven on its own here, inherited (the control:
// it answers) and planted (it does not).
func TestResolveCatalog_APlantedSourceNeverAnswersForTheOperator(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		live *modelspecs.Registry
	}{
		{CatalogModelsEnv, map[string]string{CatalogModelsEnv: forgedTable}, nil},
		{CatalogProviderEnv, map[string]string{CatalogProviderEnv: forgedQualifer},
			modelspecs.NewSeeded(map[string]modelspecs.Spec{
				forgedQualifer + "/gw/m": {ContextWindow: forgedWindow, MaxOutputTokens: forgedMaxOut, InputCostPerM: forgedPrice, OutputCostPerM: forgedPrice},
			})},
		{ResolvedEnvName, map[string]string{ResolvedEnvName: forgedRecord}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			get := mapGet(tc.env)

			// Inherited, it answers — an operator's own deployment must
			// not lose its catalog to this.
			inheritEnv(t)
			if res := ResolveCatalog("gw/m", tc.live, get); !res.Known() || res.Spec.ContextWindow != forgedWindow {
				t.Fatalf("an inherited %s resolved %+v, want the window %d: the operator's own source stopped answering", tc.name, res, forgedWindow)
			}

			// The same bytes, planted by a workspace `.env`.
			plantEnv(t, tc.name)
			res := ResolveCatalog("gw/m", tc.live, get)
			if res.Source != SourceUnknown {
				t.Fatalf("a planted %s resolved source %q, want %q", tc.name, res.Source, SourceUnknown)
			}
			// The EFFECT, not the label: none of the forged numbers
			// reached the node — not the window it compacts at, not the
			// budget it asks for, not the price it is billed at.
			if res.Spec.ContextWindow != 0 || res.Spec.MaxOutputTokens != 0 || res.Priced() {
				t.Fatalf("a planted %s resolved %+v, want nothing: the repository under review chose the window, the max output and the price of the node reviewing it", tc.name, res)
			}
		})
	}
}

// The quiet form is the same resolution — the capability READ paths take it
// (one call per keystroke of a free-text model field), and a guard that
// held only on the launch path would leave the studio's picker, `iterion
// models` and the REST catalog answering from the repository's numbers.
func TestResolveCatalogQuiet_APlantedTableNeverAnswersEither(t *testing.T) {
	get := mapGet(map[string]string{CatalogModelsEnv: forgedTable})
	plantEnv(t, CatalogModelsEnv)
	if res := ResolveCatalogQuiet("gw/m", nil, get); res.Known() {
		t.Fatalf("the read path resolved %+v from a planted %s", res, CatalogModelsEnv)
	}
}

// The price is a reading of the same sources, through the pricer the cost
// estimator holds: a planted table must not price the node either.
func TestStaticPricer_APlantedTableDoesNotPrice(t *testing.T) {
	t.Setenv(CatalogModelsEnv, forgedTable)
	t.Setenv(CatalogProviderEnv, "")
	t.Setenv(ResolvedEnvName, "")
	plantEnv(t, CatalogModelsEnv)
	if in, out, known := StaticPricer("gw/m"); known {
		t.Fatalf("a planted %s priced the node at in=%v out=%v", CatalogModelsEnv, in, out)
	}
}

// CheckOperatorTable is the second caller of the same read — the seam probe
// every gateway route runs through (checkGatewayEnv). A malformed table a
// repository planted must not refuse its reviewer's routes either: one
// `.env` line would otherwise fail every gateway node of the run reviewing
// it, and the refusal is memoised for the process.
func TestCheckOperatorTable_APlantedMalformedTableRefusesNothing(t *testing.T) {
	get := mapGet(map[string]string{CatalogModelsEnv: `{"gw/m":{"input_usd_per_mtok":0.1}}`}) // unpaired price

	inheritEnv(t)
	if err := CheckOperatorTable(get); err == nil {
		t.Fatal("an inherited malformed table was accepted: the operator's own typo must still refuse the route")
	}

	plantEnv(t, CatalogModelsEnv)
	if err := CheckOperatorTable(get); err != nil {
		t.Fatalf("a planted malformed table refused the gateway route: %v", err)
	}
}

// Not refusing is not the same as saying nothing. A malformed table is
// documented as a REFUSAL at every seam that resolves a gateway route, and
// for a planted one the refusal is gone: the ids resolve to unknown, the run
// goes unpriced, and the operator whose own project `.env` holds the typo has
// nothing to look for. Both halves of the fact are theirs — the typo, and the
// file it sits in not being a channel — and this seam is where a run is about
// to dispatch a gateway node.
func TestCheckOperatorTable_APlantedTableIsSaidOutLoudOnTheLaunchSeam(t *testing.T) {
	for _, tc := range []struct {
		name  string
		table string
		// want is the fragment proving the diagnostic covers this case: the
		// refusal text for a malformed table, the plain statement otherwise.
		want []string
	}{
		{"malformed", `{"gw/m":{"input_usd_per_mtok":0.1}}`,
			[]string{"the prices arrive as a pair or not at all", "refuses nothing", plantedTableRemedy}},
		{"well formed", forgedTable,
			[]string{"prices nothing and refuses nothing", plantedTableRemedy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			get := mapGet(map[string]string{CatalogModelsEnv: tc.table})

			// An inherited table says nothing: it answers (or refuses) for
			// itself, and a line per launch would be noise.
			inheritEnv(t)
			read, restore := captureStderr(t)
			_ = CheckOperatorTable(get)
			if said := read(); strings.Contains(said, CatalogModelsEnv) {
				restore()
				t.Fatalf("an inherited table earned a planted-table diagnostic: %q", said)
			}
			restore()

			plantEnv(t, CatalogModelsEnv)
			read, restore = captureStderr(t)
			err := CheckOperatorTable(get)
			said := read()
			restore()
			if err != nil {
				t.Fatalf("a planted table refused the route: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(said, want) {
					t.Errorf("the diagnostic does not say %q:\n%s", want, said)
				}
			}
			// Once per process, like every other catalog warning: the seam
			// runs per dispatched element.
			read, restore = captureStderr(t)
			_ = CheckOperatorTable(get)
			again := read()
			restore()
			if strings.Contains(again, CatalogModelsEnv) {
				t.Errorf("the diagnostic repeated on the second launch: %q", again)
			}
		})
	}
}

// The READ paths must stay silent: the studio's free-text model fields query
// the capability route once per keystroke, and #2103 closed exactly that
// noise. They never reach this seam, and the resolution itself says nothing
// about provenance.
func TestResolveCatalogQuiet_APlantedTableEarnsNoLine(t *testing.T) {
	get := mapGet(map[string]string{CatalogModelsEnv: forgedTable})
	plantEnv(t, CatalogModelsEnv)
	read, restore := captureStderr(t)
	ResolveCatalogQuiet("gw/m", nil, get)
	said := read()
	restore()
	if strings.Contains(said, CatalogModelsEnv) {
		t.Errorf("a per-keystroke read printed %q", said)
	}
}

// captureStderr redirects os.Stderr to a pipe for the caller's scope. The
// catalog's diagnostics go through iterlog.NewFromEnv(os.Stderr), read by the
// operator of the run, so the pipe is the only seam. `read` drains everything
// so far and reopens, `restore` undoes the redirect.
func captureStderr(t *testing.T) (read func() string, restore func()) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	restored := false
	restore = func() {
		if restored {
			return
		}
		restored = true
		os.Stderr = orig
		_ = w.Close()
		_ = r.Close()
	}
	read = func() string {
		// Close the write side so ReadAll sees EOF, then reopen a fresh pipe
		// so the caller can drain more than once without blocking. Order
		// matters — close BEFORE reopening.
		os.Stderr = orig
		_ = w.Close()
		buf, _ := io.ReadAll(r)
		_ = r.Close()
		if nr, nw, perr := os.Pipe(); perr == nil {
			r, w = nr, nw
			os.Stderr = w
		}
		return string(buf)
	}
	return read, restore
}
