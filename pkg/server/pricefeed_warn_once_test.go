package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// The layout ADR-166 §9 recommends — a declaration listed before the
// scaleway preset, whose entries state flags the declaration leaves
// unstated — warns at every fold. The publisher's fold says each such line
// once per fold input on a replica: three launches over one configuration
// say it once, a configuration write says it again. The unfiltered fold
// (EffectivePriceFeed, the view's) says it at every fold, and a level's
// read error reaches the operator at every refusal.
func TestPublisherPriceFeedEffectiveSaysEachFoldWarningOncePerInput(t *testing.T) {
	const id = "scaleway/deepseek-v4.1-flash"
	configs, states := pricefeed.NewMemoryConfigStore(), pricefeed.NewMemoryStateStore()
	preset := pricefeed.FeedConfig{Name: "scaleway", Type: pricefeed.TypeScaleway, Enabled: true, Interval: pricefeed.D(24 * time.Hour), Timeout: pricefeed.D(30 * time.Second)}
	write := func() {
		putFeedDoc(t, configs, pricefeed.PlatformConfigDocID,
			declaredFeed("pliage", `{"`+id+`":{"context_window":256000,"max_output_tokens":32768,"input_usd_per_mtok":0.672,"output_usd_per_mtok":2.016}}`), preset)
	}
	write()
	yes := true
	if err := states.Put(context.Background(), pricefeed.StateDocID(pricefeed.PlatformConfigDocID, "scaleway"), &pricefeed.StateRecord{
		Feed: "scaleway", Type: pricefeed.TypeScaleway, Status: pricefeed.StateOK, RefreshedAt: time.Now(), FreshFor: pricefeed.D(48 * time.Hour),
		Entries: map[string]modelspecs.Spec{id: {ContextWindow: 128000, InputCostPerM: 0.5, OutputCostPerM: 2, Reasoning: &yes, ToolCall: &yes}},
	}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lines []string
	warnf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	count := func(sub string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, line := range lines {
			if strings.Contains(line, sub) {
				n++
			}
		}
		return n
	}
	const shadow = "price feed pliage (platform) answers whole over feed scaleway (platform) and leaves reasoning, tool_call unstated"

	fold := PublisherPriceFeedEffective(configs, states, warnf)
	for range 3 {
		mustTable(t)(fold(context.Background(), "", "team1", []string{id}))
	}
	if n := count(shadow); n != 1 {
		t.Fatalf("three folds over one input said the shadowing %d times, want once: %q", n, lines)
	}
	time.Sleep(time.Millisecond)
	write()
	mustTable(t)(fold(context.Background(), "", "team1", []string{id}))
	if n := count(shadow); n != 2 {
		t.Fatalf("a configuration write did not say the shadowing again (%d lines): %q", n, lines)
	}

	for range 2 {
		mustTable(t)(EffectivePriceFeed(context.Background(), configs, states, "", "team1", []string{id}, warnf))
	}
	if n := count(shadow); n != 4 {
		t.Fatalf("the unfiltered fold said the shadowing %d times over two folds, want each", n-2)
	}

	unreadFold := PublisherPriceFeedEffective(failingConfigStore{ConfigStore: configs, docID: pricefeed.PlatformConfigDocID, err: errors.New("replica set has no primary")}, states, warnf)
	for range 2 {
		if _, err := unreadFold(context.Background(), "", "team1", nil); err == nil {
			t.Fatal("an unread level answered a table")
		}
	}
	if n := count("platform unread — the launch or resume folding it is refused: replica set has no primary"); n != 2 {
		t.Fatalf("two refusals logged the read error %d times, want each", n)
	}
}
