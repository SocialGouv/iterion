package compatgw

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// pliageEfforts is the DomiFa route's accepted list, as the platform's
// declared feed states it.
var pliageEfforts = []string{"low", "high", "xhigh", "max"}

// declaredEffortsTable folds a declared feed `pliage` through the
// production builder (pricefeed.Fold) and answers the run table's wire
// bytes: scaleway/deepseek-v4.1-flash priced with its efforts, kimi-k2
// efforts only.
func declaredEffortsTable(t *testing.T) string {
	t.Helper()
	now := time.Now()
	table := pricefeed.Fold(pricefeed.FoldConfig{Now: now}, pricefeed.ScopeState{Config: &pricefeed.ConfigRecord{
		UpdatedAt: now,
		Feeds: []pricefeed.FeedConfig{{
			Name: "pliage", Type: pricefeed.TypeDeclared, Enabled: true,
			Interval: pricefeed.D(30 * 24 * time.Hour), Timeout: pricefeed.D(time.Second),
			Params: map[string]string{"entries": `{
				"scaleway/deepseek-v4.1-flash":{"context_window":128000,"input_usd_per_mtok":0.3,"output_usd_per_mtok":1.2,"reasoning_efforts":["low","high","xhigh","max"]},
				"kimi-k2":{"reasoning_efforts":["low","medium"]}}`},
		}},
	}})
	if table == nil {
		t.Fatal("the declaration folded to nothing")
	}
	raw, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// warnSink records warnings in the iterlog shape.
type warnSink struct{ lines []string }

func (w *warnSink) warn(format string, args ...any) {
	w.lines = append(w.lines, fmt.Sprintf(format, args...))
}

func (w *warnSink) said(fragments ...string) bool {
	for _, line := range w.lines {
		all := true
		for _, f := range fragments {
			all = all && strings.Contains(line, f)
		}
		if all {
			return true
		}
	}
	return false
}

// The run table answers first: its efforts, under the normalized id, beat
// the runner's env table for the same id — whatever the env lists — and
// the answer names the feed.
func TestAcceptedEffortsReadsTheRunTableFirst(t *testing.T) {
	ResetCatalogCaches()
	t.Cleanup(ResetCatalogCaches)
	get := mapGet(map[string]string{
		FeedTableEnv:        declaredEffortsTable(t),
		ReasoningEffortsEnv: `{"scaleway/deepseek-v4.1-flash":["low","max"],"Scaleway/DeepSeek-v4.1-flash":["medium"]}`,
	})
	for _, wire := range []string{"scaleway/deepseek-v4.1-flash", "Scaleway/DeepSeek-v4.1-flash"} {
		var w warnSink
		values, source := AcceptedEfforts(wire, get, w.warn)
		if !slices.Equal(values, pliageEfforts) || source != SourceFeed("pliage") {
			t.Errorf("%s: AcceptedEfforts = %v from %q, want %v from feed:pliage", wire, values, source, pliageEfforts)
		}
		if len(w.lines) != 0 {
			t.Errorf("%s: a valid run table warned: %v", wire, w.lines)
		}
	}
}

// The env table answers what the run table leaves open — exact id, as
// before — and with neither, nothing answers: the requested effort passes.
func TestAcceptedEffortsFallsBackToTheEnvTable(t *testing.T) {
	ResetCatalogCaches()
	t.Cleanup(ResetCatalogCaches)
	get := mapGet(map[string]string{
		FeedTableEnv:        declaredEffortsTable(t),
		ReasoningEffortsEnv: `{"scaleway/glm-5.3":["low","high","max"]}`,
	})
	var w warnSink
	values, source := AcceptedEfforts("scaleway/glm-5.3", get, w.warn)
	if !slices.Equal(values, []string{"low", "high", "max"}) || source != SourceReasoningEffortsEnv {
		t.Fatalf("AcceptedEfforts = %v from %q, want the env list from %s", values, source, SourceReasoningEffortsEnv)
	}
	if values, source := AcceptedEfforts("unlisted", get, w.warn); values != nil || source != "" {
		t.Fatalf("an id nothing lists answered %v from %q", values, source)
	}
	if values, source := AcceptedEfforts("scaleway/glm-5.3", mapGet(nil), w.warn); values != nil || source != "" {
		t.Fatalf("an empty environment answered %v from %q", values, source)
	}
}

// A run-table entry the vocabulary refuses (a newer server's level, a
// damaged value) warns by name and steps down to the env table — it never
// maps an effort, and never hides the env's answer.
func TestAcceptedEffortsStepsDownFromAnInvalidRunTableEntry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values string
	}{
		{"unknown level", `["low","hihg"]`},
		{"empty list", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetCatalogCaches()
			t.Cleanup(ResetCatalogCaches)
			table := `{"entries":{},"efforts":{"scaleway/deepseek-v4.1-flash":{"feed":"pliage","values":` + tc.values + `}}}`
			if pricefeed.ParseTable(table) == nil {
				t.Fatal("the fixture table does not parse")
			}
			get := mapGet(map[string]string{
				FeedTableEnv:        table,
				ReasoningEffortsEnv: `{"scaleway/deepseek-v4.1-flash":["low","max"]}`,
			})
			var w warnSink
			values, source := AcceptedEfforts("scaleway/deepseek-v4.1-flash", get, w.warn)
			if !slices.Equal(values, []string{"low", "max"}) || source != SourceReasoningEffortsEnv {
				t.Errorf("AcceptedEfforts = %v from %q, want the env list", values, source)
			}
			if !w.said("REASONING_EFFORTS_INVALID", FeedTableEnv, "scaleway/deepseek-v4.1-flash", "pliage") {
				t.Errorf("the invalid run-table entry stepped down silently: %v", w.lines)
			}
		})
	}
}

// The sandbox crossing writes the merged table under the name it already
// crosses, so an OLDER sandbox image — whose route reads only the env
// table, exact id, through its own frozen parser (olderImageEfforts) —
// maps an effort by the run table: the node's exact wire spelling (upper
// case included) and the normalized id answer the feed's list, an env
// entry under any spelling of a feed id is replaced by the feed's list,
// env entries for other ids survive, and an invalid env entry does not
// cross. A NEW image reads the crossed feed table first and lands on the
// same answer.
func TestCrossingReasoningEffortsServesAnOlderSandbox(t *testing.T) {
	ResetCatalogCaches()
	t.Cleanup(ResetCatalogCaches)
	feedTable := declaredEffortsTable(t)
	host := map[string]string{
		FeedTableEnv:        feedTable,
		ReasoningEffortsEnv: `{"other/model":["low","max"],"scaleway/deepseek-v4.1-flash":["medium"],"SCALEWAY/DEEPSEEK-V4.1-FLASH":["medium"],"broken":["hihg"]}`,
	}
	const wire = "Scaleway/DeepSeek-v4.1-flash"
	var w warnSink
	crossed := CrossingReasoningEfforts(wire, mapGet(host), w.warn)

	old := mapGet(map[string]string{ReasoningEffortsEnv: crossed})
	var oldWarn warnSink
	for _, id := range []string{wire, "scaleway/deepseek-v4.1-flash", "SCALEWAY/DEEPSEEK-V4.1-FLASH"} {
		if got := olderImageEfforts(id, old, oldWarn.warn); !slices.Equal(got, pliageEfforts) {
			t.Errorf("an older sandbox reads %v for %q from the crossing %s, want the feed's %v", got, id, crossed, pliageEfforts)
		}
	}
	if got := olderImageEfforts("kimi-k2", old, oldWarn.warn); !slices.Equal(got, []string{"low", "medium"}) {
		t.Errorf("an efforts-only feed id did not cross: %v (%s)", got, crossed)
	}
	if got := olderImageEfforts("other/model", old, oldWarn.warn); !slices.Equal(got, []string{"low", "max"}) {
		t.Errorf("an env entry for another id did not survive the crossing: %v (%s)", got, crossed)
	}
	if strings.Contains(crossed, "broken") || len(oldWarn.lines) != 0 {
		t.Errorf("an invalid env entry crossed: %s (container warnings %v)", crossed, oldWarn.lines)
	}

	container := mapGet(map[string]string{FeedTableEnv: feedTable, ReasoningEffortsEnv: crossed})
	hostValues, hostSource := AcceptedEfforts(wire, mapGet(host), w.warn)
	values, source := AcceptedEfforts(wire, container, w.warn)
	if !slices.Equal(values, hostValues) || source != hostSource {
		t.Errorf("a new sandbox answers %v from %q, the host %v from %q", values, source, hostValues, hostSource)
	}

	// Nothing on either side crosses as the explicit empty.
	if got := CrossingReasoningEfforts(wire, mapGet(nil), w.warn); got != "" {
		t.Errorf("an empty environment crossed %q, want the explicit empty", got)
	}
}

// A run-table entry the vocabulary refuses does not cross: the env
// table's entry for the id crosses instead — the answer the host's own
// ladder steps down to — and the crossing warns by name.
func TestCrossingReasoningEffortsStepsDownFromAnInvalidRunTableEntry(t *testing.T) {
	ResetCatalogCaches()
	t.Cleanup(ResetCatalogCaches)
	host := mapGet(map[string]string{
		FeedTableEnv:        `{"entries":{},"efforts":{"scaleway/deepseek-v4.1-flash":{"feed":"pliage","values":["hihg"]}}}`,
		ReasoningEffortsEnv: `{"scaleway/deepseek-v4.1-flash":["low","max"]}`,
	})
	var w warnSink
	crossed := CrossingReasoningEfforts("scaleway/deepseek-v4.1-flash", host, w.warn)
	old := mapGet(map[string]string{ReasoningEffortsEnv: crossed})
	if got := olderImageEfforts("scaleway/deepseek-v4.1-flash", old, w.warn); !slices.Equal(got, []string{"low", "max"}) {
		t.Errorf("an older sandbox reads %v from the crossing %s, want the env list the host steps down to", got, crossed)
	}
	if hostValues, _ := AcceptedEfforts("scaleway/deepseek-v4.1-flash", host, w.warn); !slices.Equal(hostValues, []string{"low", "max"}) {
		t.Errorf("the host answers %v, want the env list", hostValues)
	}
	if !w.said("REASONING_EFFORTS_INVALID", FeedTableEnv, "scaleway/deepseek-v4.1-flash", "not crossed") {
		t.Errorf("the invalid run-table entry was dropped from the crossing silently: %v", w.lines)
	}
}
