package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/pflag"

	"github.com/SocialGouv/iterion/internal/stdiotest"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// priceFeedsListAnswer is a team view the recording server answers with.
const priceFeedsListAnswer = `{"scope":"team","origin":"db","source_types":["modelsdev","scaleway","litellm","exec"],
	"feeds":[{"feed":{"name":"scw","type":"exec","enabled":true,"interval":3600,"timeout":10},
	"status":"ok","entries":2,"refreshed_at":"2026-11-07T10:00:00Z","fresh_for":7200}]}`

// priceFeedsRecorder runs the root command against a recording server and
// returns the requests it received.
func priceFeedsRecorder(t *testing.T, args ...string) ([]string, error) {
	t.Helper()
	got, _, err := priceFeedsServe(t, func(*http.Request) (int, string) { return http.StatusOK, priceFeedsListAnswer }, args...)
	return got, err
}

// priceFeedsServe runs the root command against a server answering through
// respond, and returns the requests it received ("METHOD path?query body")
// and what the command printed on stdout.
func priceFeedsServe(t *testing.T, respond func(r *http.Request) (int, string), args ...string) ([]string, string, error) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		line := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			line += "?" + r.URL.RawQuery
		}
		mu.Lock()
		got = append(got, line+" "+strings.TrimSpace(string(b)))
		mu.Unlock()
		code, body := respond(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	priceFeedsScratchEnv(t)
	t.Setenv("ITERION_REMOTE_URL", srv.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "test-token-not-a-secret")
	t.Setenv("ITERION_REMOTE_TEAM", "team-a")
	out, err := runPriceFeedsCommand(t, args...)
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), got...), out, err
}

// runPriceFeedsCommand executes the root command from clean flags and
// returns what it printed on stdout.
func runPriceFeedsCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetPriceFeedsFlags(t)
	rootCmd.SetArgs(args)
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	var err error
	out := stdiotest.Run(t, &os.Stdout, func() { err = rootCmd.Execute() })
	return out, err
}

// resetPriceFeedsFlags returns every price-feeds subcommand's flags, and
// the global --json, to their defaults: Cobra keeps flag values between
// executions in one process, so each case starts from a clean command line.
func resetPriceFeedsFlags(t *testing.T) {
	t.Helper()
	jsonOutput = false
	for _, cmd := range remotePriceFeedsCmd.Commands() {
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if slice, ok := f.Value.(pflag.SliceValue); ok {
				if err := slice.Replace(nil); err != nil {
					t.Fatalf("reset --%s of %s: %v", f.Name, cmd.Name(), err)
				}
			} else if err := f.Value.Set(f.DefValue); err != nil {
				t.Fatalf("reset --%s of %s: %v", f.Name, cmd.Name(), err)
			}
			f.Changed = false
		})
	}
}

func TestRemotePriceFeedsCommands(t *testing.T) {
	got, err := priceFeedsRecorder(t, "remote", "price-feeds", "list")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "GET /api/teams/team-a/price-feeds " {
		t.Fatalf("list hit %v", got)
	}

	got, err = priceFeedsRecorder(t, "remote", "price-feeds", "list", "--org", "org-9")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "GET /api/orgs/org-9/price-feeds " {
		t.Fatalf("org-scoped list hit %v", got)
	}

	got, err = priceFeedsRecorder(t, "remote", "price-feeds", "sync", "scw")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "POST /api/teams/team-a/price-feeds/sync {\"name\":\"scw\"}" {
		t.Fatalf("named sync hit %v", got)
	}

	got, err = priceFeedsRecorder(t, "remote", "price-feeds", "delete")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "PUT /api/teams/team-a/price-feeds {\"feeds\":null}") {
		t.Fatalf("delete hit %v", got)
	}
}

// --platform addresses the deployment's own feed list on every verb, and a
// command names one scope: two scope flags are refused before any request,
// never resolved by a silent precedence.
func TestRemotePriceFeedsPlatformScope(t *testing.T) {
	feeds := filepath.Join(t.TempDir(), "feeds.json")
	if err := os.WriteFile(feeds, []byte(`[{"name":"scaleway","type":"scaleway","enabled":true,"interval":86400,"timeout":30}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"list", "--platform"}, "GET /api/admin/price-feeds "},
		{[]string{"set", "--platform", "--file", feeds}, `PUT /api/admin/price-feeds {"feeds":[{"name":"scaleway","type":"scaleway","enabled":true,"interval":86400,"timeout":30}]}`},
		{[]string{"delete", "--platform"}, `PUT /api/admin/price-feeds {"feeds":null}`},
		{[]string{"sync", "--platform", "scaleway"}, `POST /api/admin/price-feeds/sync {"name":"scaleway"}`},
	} {
		got, err := priceFeedsRecorder(t, append([]string{"remote", "price-feeds"}, tc.args...)...)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("%v hit %q, want %q", tc.args, got, tc.want)
		}
	}
	for _, args := range [][]string{
		{"list", "--platform", "--team", "team-b"},
		{"list", "--platform", "--org", "org-9"},
		{"list", "--org", "org-9", "--team", "team-b"},
		{"sync", "--platform", "--org", "org-9"},
	} {
		got, err := priceFeedsRecorder(t, append([]string{"remote", "price-feeds"}, args...)...)
		if err == nil || len(got) != 0 {
			t.Fatalf("%v: err %v, requests %q — two scope flags must refuse before any request", args, err, got)
		}
	}
}

// list prints every param in key order, a value over 120 bytes as its size
// (a declaration's whole document would bury the list), and a declaration
// by its config: declared, its stamp and its entry count.
func TestRemotePriceFeedsListSummarisesLongParams(t *testing.T) {
	long := `{"glm-5.3":{"context_window":256000,"max_output_tokens":16384,"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15},"scaleway/deepseek-v4.1-flash":{"reasoning_efforts":["low","high","xhigh","max"]}}`
	answer := `{"scope":"platform","origin":"db","source_types":["declared"],"feeds":[
		{"feed":{"name":"pliage","type":"declared","enabled":true,"interval":2592000,"timeout":1,"params":{"zeta":"z","entries":` + quoteJSON(t, long) + `,"alpha":"a"}},
		 "status":"declared","entries":2,"declared_at":"2026-11-12T09:30:00Z"}]}`
	_, out, err := priceFeedsServe(t, func(*http.Request) (int, string) { return http.StatusOK, answer }, "remote", "price-feeds", "list", "--platform")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "glm-5.3") {
		t.Errorf("list printed the declaration's document whole:\n%s", out)
	}
	if want := "alpha=a entries=(" + strconv.Itoa(len(long)) + " bytes) zeta=z"; !strings.Contains(out, want) {
		t.Errorf("list params: want %q in\n%s", want, out)
	}
	if want := "2026-11-12 09:30:00 — 2 entries"; !strings.Contains(out, want) || !strings.Contains(out, "declared") {
		t.Errorf("list must render the declaration's stamp and entry count (%q):\n%s", want, out)
	}
}

// The summary a sync change or a declare diff prints renders every field
// SpecEqual compares: a change in any one of them reads as a change, never
// as "old → old".
func TestPriceEntrySummaryRendersEveryComparedField(t *testing.T) {
	base := modelspecs.Spec{InputCostPerM: 2.01, OutputCostPerM: 6.15}
	typ := reflect.TypeOf(base)
	for i := 0; i < typ.NumField(); i++ {
		changed := base
		field := reflect.ValueOf(&changed).Elem().Field(i)
		switch field.Kind() {
		case reflect.Int:
			field.SetInt(field.Int() + 4096)
		case reflect.Float64:
			field.SetFloat(field.Float() + 0.00001)
		case reflect.Pointer:
			flag := true
			field.Set(reflect.ValueOf(&flag))
		case reflect.Slice:
			field.Set(reflect.ValueOf([]string{"low", "high"}))
		default:
			t.Fatalf("modelspecs.Spec.%s: kind %s has no case here — add one", typ.Field(i).Name, field.Kind())
		}
		if pricefeed.SpecEqual(base, changed) {
			t.Fatalf("modelspecs.Spec.%s: SpecEqual does not see the change — this test's premise moved", typ.Field(i).Name)
		}
		if before, after := priceEntrySummary(&base), priceEntrySummary(&changed); before == after {
			t.Errorf("modelspecs.Spec.%s changed and the summary did not: %q", typ.Field(i).Name, before)
		}
	}
}

// quoteJSON is s as a JSON string literal.
func quoteJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
