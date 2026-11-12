package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// effective renders the real server's effective view of a team: per
// requested id, the entry and the efforts a run reads, each with the feed
// and level that answers it, every contributing feed, and the server's
// word on the runner's env fallback. The platform states the efforts, the
// team re-prices glm-5.3: each answer names its own source.
func TestRemotePriceFeedsEffectiveRendersEachAnswerAndItsSource(t *testing.T) {
	live := newPriceFeedsLive(t)
	live.seed(t, pricefeed.PlatformConfigDocID, declaredTestFeed("pliage",
		`{"glm-5.3":{"context_window":256000,"max_output_tokens":16384,"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15},"scaleway/deepseek-v4.1-flash":{"reasoning_efforts":["low","high","xhigh","max"]}}`,
		30*24*time.Hour))
	live.seed(t, pricefeed.TeamConfigDocID("team-pf"), declaredTestFeed("team-tariffs",
		`{"glm-5.3":{"input_usd_per_mtok":3,"output_usd_per_mtok":9}}`, 30*24*time.Hour))

	out, err := runPriceFeedsCommand(t, "remote", "price-feeds", "effective", "--model", "scaleway/deepseek-v4.1-flash", "--model", "GLM-5.3")
	if err != nil {
		t.Fatalf("effective: %v\n%s", err, out)
	}
	live.mu.Lock()
	requests := append([]string(nil), live.requests...)
	live.mu.Unlock()
	if len(requests) != 1 || requests[0] != "GET /api/teams/team-pf/price-feeds/effective?model=scaleway%2Fdeepseek-v4.1-flash&model=GLM-5.3" {
		t.Fatalf("effective hit %q", requests)
	}
	deepseek, glm, ok := strings.Cut(out, "glm-5.3 ──")
	if !ok || !strings.Contains(deepseek, "scaleway/deepseek-v4.1-flash") {
		t.Fatalf("effective must render each requested id under its own header, in request order:\n%s", out)
	}
	for _, want := range []string{
		"efforts:", "low,high,xhigh,max — feed pliage (platform)",
		"entry:", "no feed prices it",
	} {
		if !strings.Contains(deepseek, want) {
			t.Errorf("scaleway/deepseek-v4.1-flash misses %q:\n%s", want, deepseek)
		}
	}
	for _, want := range []string{
		"$3/$9/mtok — feed team-tariffs (team team-pf), declared",
		"platform pliage: $2.01/$6.15/mtok window 256000 max output 16384",
		"team team-pf team-tariffs: $3/$9/mtok (answers the entry)",
	} {
		if !strings.Contains(glm, want) {
			t.Errorf("glm-5.3 misses %q:\n%s", want, glm)
		}
	}
	if !strings.Contains(out, "not visible from the server") {
		t.Errorf("effective must relay the server's word on the runner's env fallback:\n%s", out)
	}

	// --json relays the server's answer whole.
	out, err = runPriceFeedsCommand(t, "remote", "price-feeds", "effective", "--json", "--model", "glm-5.3")
	if err != nil {
		t.Fatalf("effective --json: %v\n%s", err, out)
	}
	var view struct {
		TeamID string `json:"team_id"`
		Models []struct {
			ID    string           `json:"id"`
			Entry *pricefeed.Entry `json:"entry"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("effective --json is not the view: %v\n%s", err, out)
	}
	if view.TeamID != "team-pf" || len(view.Models) != 1 || view.Models[0].Entry == nil || view.Models[0].Entry.Feed != "team-tariffs" {
		t.Fatalf("effective --json: %+v", view)
	}
}

// A listing whose folded table exceeds the run channel's byte budget is
// sealed whole by no run: effective says so — the size, the budget, how a
// run seals it and the --model that shows one — never "no feed answers",
// and still renders each id's answer. A listing no feed answers says that.
func TestRemotePriceFeedsEffectiveSaysAnOverBudgetListingIsNarrowedPerRun(t *testing.T) {
	live := newPriceFeedsLive(t)
	var doc strings.Builder
	doc.WriteString(`{"scaleway/deepseek-v4.1-flash":{"reasoning_efforts":["low","high","xhigh","max"]}`)
	for i := range 3000 {
		fmt.Fprintf(&doc, `,"model-%04d":{"reasoning_efforts":["low","high"]}`, i)
	}
	doc.WriteString("}")
	live.seed(t, pricefeed.PlatformConfigDocID, declaredTestFeed("big", doc.String(), 30*24*time.Hour))

	out, err := runPriceFeedsCommand(t, "remote", "price-feeds", "effective")
	if err != nil {
		t.Fatalf("effective: %v\n%s", err, out)
	}
	if strings.Contains(out, "no feed answers") {
		t.Errorf("effective says no feed answers an over-budget listing:\n%.600s", out)
	}
	if !regexp.MustCompile(`the folded table is \d+ bytes, over the run channel's 102400: no run seals it whole — a run seals it narrowed to the gateway ids it names`).MatchString(out) || !strings.Contains(out, "--model ID") {
		t.Errorf("effective does not say how a run seals an over-budget listing:\n%.600s", out)
	}
	if !strings.Contains(out, "low,high,xhigh,max — feed big (platform)") {
		t.Errorf("effective does not render the listing's answers:\n%.600s", out)
	}

	live.seed(t, pricefeed.PlatformConfigDocID)
	out, err = runPriceFeedsCommand(t, "remote", "price-feeds", "effective")
	if err != nil {
		t.Fatalf("effective over no feed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no feed answers: a run carries no table") {
		t.Errorf("effective over no feed:\n%s", out)
	}
}

// Two levels naming their feed alike and stating the same efforts: the run
// reads the team's, and effective names the team as the level that answers.
func TestRemotePriceFeedsEffectiveNamesTheMoreSpecificOfTwoLikeFeeds(t *testing.T) {
	live := newPriceFeedsLive(t)
	const efforts = `{"scaleway/deepseek-v4.1-flash":{"reasoning_efforts":["low","high","xhigh","max"]}}`
	live.seed(t, pricefeed.PlatformConfigDocID, declaredTestFeed("pliage", efforts, 30*24*time.Hour))
	live.seed(t, pricefeed.TeamConfigDocID("team-pf"), declaredTestFeed("pliage", efforts, 30*24*time.Hour))

	out, err := runPriceFeedsCommand(t, "remote", "price-feeds", "effective", "--model", "scaleway/deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("effective: %v\n%s", err, out)
	}
	if !strings.Contains(out, "low,high,xhigh,max — feed pliage (team team-pf)") {
		t.Errorf("effective does not name the team as the level that answers the efforts:\n%s", out)
	}
	if strings.Count(out, "(answers the efforts)") != 1 {
		t.Errorf("effective marks %d contributors as answering the efforts, want one:\n%s", strings.Count(out, "(answers the efforts)"), out)
	}
}
