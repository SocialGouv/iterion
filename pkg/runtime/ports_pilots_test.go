package runtime

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// These fixtures isolate one fan-out/convergence stage from each frozen
// reference in docs/public-contracts-pilot-thresholds.md. All external work
// is a declared fake; no source project or paid service is touched.
const pilotNativeSource = `dsl: 2
contract Batch:
  display_name: "Produce deliverables"
  responsibility: "Publish all prepared deliverables"
  inputs:
    items: string[]
  outputs:
    results: string[]
    summary: string
contract Prepare:
  display_name: "Prepare jobs"
  responsibility: "Select the jobs to run"
  inputs:
    items: string[]
  outputs:
    items: string[]
contract Produce:
  display_name: "Produce one job"
  responsibility: "Produce one declared deliverable"
  inputs:
    item: string
  outputs:
    deliverable: string
contract Collect:
  display_name: "Collect jobs"
  responsibility: "Publish the complete ordered collection"
  inputs:
    deliverables: string[]
  outputs:
    results: string[]
    summary: string
tool prepare_impl:
  command: "fake-prepare"
tool produce_impl:
  command: "fake-produce"
tool collect_impl:
  command: "fake-collect"
workflow pilot:
  worktree: none
  runtime_semantics: "ports-v1"
  contract: Batch
  graph:
    nodes:
      prepare:
        implementation: prepare_impl
        contract: Prepare
      produce:
        implementation: produce_impl
        contract: Produce
      collect:
        implementation: collect_impl
        contract: Collect
    bindings:
      input.items -> prepare.items
      prepare.items -> produce.item
      produce.deliverable -> collect.deliverables
    exports:
      results: collect.results
      summary: collect.summary
    products: ["results", "summary"]
  budget:
    max_parallel_branches: 2
`

type pilotObservation struct {
	Results []string
	Summary string
	Counts  [3]int32 // preparation, item, collection
	Peak    int32
	Elapsed time.Duration
}

type pilotFakeEffects struct {
	mu          sync.Mutex
	results     map[string]string
	active      atomic.Int32
	peak        atomic.Int32
	waitForPair bool
	started     atomic.Int32
	pairReady   chan struct{}
}

func (f *pilotFakeEffects) produce(ctx context.Context, id string) (string, error) {
	active := f.active.Add(1)
	for {
		peak := f.peak.Load()
		if active <= peak || f.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	defer f.active.Add(-1)
	if f.waitForPair {
		started := f.started.Add(1)
		if started == 2 {
			close(f.pairReady)
		}
		if started <= 2 {
			select {
			case <-f.pairReady:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	select {
	case <-time.After(10 * time.Millisecond):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	value := id + "-ready"
	f.mu.Lock()
	f.results[id] = value
	f.mu.Unlock()
	return value, nil
}

func pilotIDs(label string, count int) ([]string, []any) {
	ids, values := make([]string, count), make([]any, count)
	for i := range count {
		ids[i] = fmt.Sprintf("%s-%d", label, i)
		values[i] = ids[i]
	}
	return ids, values
}

func pilotExpected(ids []string) ([]string, string) {
	results := make([]string, len(ids))
	for i, id := range ids {
		results[i] = id + "-ready"
	}
	return results, strings.Join(results, ",")
}

func runLegacyPilot(t *testing.T, ids []string) pilotObservation {
	t.Helper()
	fake := &pilotFakeEffects{results: map[string]string{}, waitForPair: len(ids) >= 2, pairReady: make(chan struct{})}
	var counts [3]atomic.Int32
	var delivered pilotObservation
	wf := fanOutEachWorkflow(false, ir.AwaitWaitAll, 2)
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		switch node.NodeID() {
		case "entry":
			counts[0].Add(1)
			jobs := make([]any, len(ids))
			for i, id := range ids {
				jobs[i] = item(id)
			}
			return map[string]any{"items": jobs}, nil
		case "handle":
			counts[1].Add(1)
			value, err := fake.produce(ctx, input["id"].(string))
			return map[string]any{"deliverable": value}, err
		case "collect":
			counts[2].Add(1)
			fake.mu.Lock()
			results := make([]string, len(ids))
			for i, id := range ids {
				results[i] = fake.results[id]
			}
			fake.mu.Unlock()
			delivered.Results, delivered.Summary = results, strings.Join(results, ",")
			return map[string]any{"results": results, "summary": delivered.Summary}, nil
		}
		return nil, fmt.Errorf("unexpected legacy node %s", node.NodeID())
	})
	s := tmpStore(t)
	start := time.Now()
	if err := New(wf, s, executor, WithSandboxOverride("none")).Run(portsTestContext(t), "legacy-pilot", nil); err != nil {
		t.Fatal(err)
	}
	delivered.Elapsed = time.Since(start)
	r, err := s.LoadRun(portsTestContext(t), "legacy-pilot")
	if err != nil || r.Status != store.RunStatusFinished {
		t.Fatalf("legacy pilot did not finish: %+v %v", r, err)
	}
	for i := range counts {
		delivered.Counts[i] = counts[i].Load()
	}
	delivered.Peak = fake.peak.Load()
	return delivered
}

func runNativePilot(t *testing.T, ids []string, values []any) pilotObservation {
	t.Helper()
	fake := &pilotFakeEffects{results: map[string]string{}, waitForPair: len(ids) >= 2, pairReady: make(chan struct{})}
	var counts [3]atomic.Int32
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		switch {
		case node.NodeID() == "prepare":
			counts[0].Add(1)
			return map[string]any{"items": input["items"]}, nil
		case strings.HasPrefix(node.NodeID(), "@produce["):
			counts[1].Add(1)
			value, err := fake.produce(ctx, input["item"].(string))
			return map[string]any{"deliverable": value}, err
		case node.NodeID() == "collect":
			counts[2].Add(1)
			raw := input["deliverables"].([]any)
			results := make([]string, len(raw))
			for i, value := range raw {
				results[i] = value.(string)
			}
			return map[string]any{"results": raw, "summary": strings.Join(results, ",")}, nil
		}
		return nil, fmt.Errorf("unexpected native node %s", node.NodeID())
	})
	engine, s := portsTestEngine(t, tmpStore, pilotNativeSource, executor)
	start := time.Now()
	if err := engine.Run(portsTestContext(t), "pc1_pilot", map[string]any{"items": values}); err != nil {
		t.Fatal(err)
	}
	observation := pilotObservation{Elapsed: time.Since(start), Peak: fake.peak.Load()}
	r := portsTestRun(t, s, "pc1_pilot")
	if r.Status != store.RunStatusFinished || len(r.PortExecution.Invocations) != len(ids)+2 ||
		len(r.PortExecution.Products) != 2 || len(r.PortExecution.Exports) != 2 {
		t.Fatalf("native pilot omitted an invocation or public product: %+v", r.PortExecution)
	}
	raw := portsTestExport(t, r, "results").([]any)
	observation.Results = make([]string, len(raw))
	for i, value := range raw {
		observation.Results[i] = value.(string)
	}
	observation.Summary = portsTestExport(t, r, "summary").(string)
	for i := range counts {
		observation.Counts[i] = counts[i].Load()
	}
	return observation
}

func medianDuration(values []time.Duration) time.Duration {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[len(values)/2]
}

func TestPublicContractsRepresentativePilots(t *testing.T) {
	for _, pilot := range []struct{ name, label string }{
		{"ShortsUnitDispatch", "unit"},
		{"TownEpicImageStage", "epic-view"},
		{"TabarriaStillStage", "still"},
	} {
		t.Run(pilot.name, func(t *testing.T) {
			for _, count := range []int{0, 1, 4} {
				t.Run(fmt.Sprint(count), func(t *testing.T) {
					ids, values := pilotIDs(pilot.label, count)
					wantResults, wantSummary := pilotExpected(ids)
					var legacyTimes, nativeTimes []time.Duration
					for round := range 3 {
						legacy := runLegacyPilot(t, ids)
						native := runNativePilot(t, ids, values)
						for name, observation := range map[string]pilotObservation{"legacy": legacy, "native": native} {
							if !reflect.DeepEqual(observation.Results, wantResults) || observation.Summary != wantSummary ||
								observation.Counts != [3]int32{1, int32(count), 1} || observation.Peak > 2 ||
								(count == 4 && observation.Peak < 2) {
								t.Fatalf("%s round %d violated frozen equivalence/concurrency: %+v, want %v %q", name, round, observation, wantResults, wantSummary)
							}
						}
						legacyTimes = append(legacyTimes, legacy.Elapsed)
						nativeTimes = append(nativeTimes, native.Elapsed)
					}
					legacyMedian, nativeMedian := medianDuration(legacyTimes), medianDuration(nativeTimes)
					ceiling := max(2*legacyMedian, legacyMedian+100*time.Millisecond)
					// Race instrumentation changes filesystem and scheduler costs by
					// different factors for the two engines. Check the frozen latency
					// ceiling in the ordinary run; keep all semantic assertions under -race.
					if !pilotTimingInstrumented && nativeMedian > ceiling {
						t.Fatalf("native median %s exceeded frozen ceiling %s (legacy %s)", nativeMedian, ceiling, legacyMedian)
					}
					t.Logf("pilot=%s count=%d legacy_median=%s native_median=%s invocations=%d peak=%d retries=0 fake_item_calls=%d paid_effect_calls=0 tokens=unknown cost=unknown deliverables=%v threshold_commit=3d933d067",
						pilot.name, count, legacyMedian, nativeMedian, count+2, min(count, 2), count, wantResults)
				})
			}
		})
	}
}
